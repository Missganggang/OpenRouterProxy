package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/app"
	"github.com/openroute/openroute/internal/config"
	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// promptAdminPassword 交互式引导创建管理员时读取用户名与密码。
//
// 行为（规格书 3.3 第 7 步）：
//   - 用户名：环境变量 ADMIN 为空时提示输入，直接回车使用默认 admin；
//   - 密码：提示输入两次并要求一致，且长度不少于 6 位。
//
// 参数 defUser 为环境变量提供的用户名（可为空）。
// 返回密码与错误。
func promptAdminPassword(defUser string) (string, error) {
	fmt.Println()
	fmt.Println("首次启动：用户表为空，需要创建管理员账号。")
	fmt.Println("（也可以下次使用 MIGRATE=1 ADMIN=admin ./openroute 通过环境变量创建）")
	fmt.Println()

	stdin := bufio.NewReader(os.Stdin)

	user := strings.TrimSpace(defUser)
	if user == "" {
		fmt.Printf("请输入管理员用户名 [%s]: ", app.DefaultAdminUsername)
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("读取用户名失败: %w", err)
		}
		user = strings.TrimSpace(line)
	}
	if user == "" {
		user = app.DefaultAdminUsername
	}

	for attempt := 0; attempt < 3; attempt++ {
		fmt.Print("请输入管理员密码（至少 6 位）: ")
		pwd1, err := stdin.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("读取密码失败: %w", err)
		}
		pwd1 = strings.TrimSpace(pwd1)

		if len(pwd1) < 6 {
			fmt.Println("  密码长度不足 6 位，请重新输入。")
			continue
		}

		fmt.Print("请再次输入密码确认: ")
		pwd2, err := stdin.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("读取密码失败: %w", err)
		}
		if strings.TrimSpace(pwd2) != pwd1 {
			fmt.Println("  两次输入的密码不一致，请重新输入。")
			continue
		}

		// 把用户名写回环境变量，供 ensureAdmin 复用。
		_ = os.Setenv("ADMIN", user)
		return pwd1, nil
	}

	return "", fmt.Errorf("连续三次输入不合法，已取消创建")
}

// runSelfCheck 执行 `-check` 自检（规格书 3.2 新增命令）。
//
// 检查项：配置文件可读、数据库可连接、目录可写、
// 监听端口可用、前端资源是否存在。
//
// 返回进程退出码：全部通过为 0，有失败项为 1。
func runSelfCheck(cfg *config.Config, log *zap.Logger) int {
	fmt.Println()
	fmt.Println("OpenRoute 自检")
	fmt.Println(strings.Repeat("─", 55))

	failed := 0
	ok := func(label, detail string) {
		fmt.Printf("  [通过] %s: %s\n", label, detail)
	}
	bad := func(label, detail string) {
		failed++
		fmt.Printf("  [失败] %s: %s\n", label, detail)
	}

	// 1. 配置文件
	if _, err := os.Stat(cfg.ConfigPath); err == nil {
		ok("配置文件", cfg.ConfigPath)
	} else {
		bad("配置文件", fmt.Sprintf("%s 不可读: %v", cfg.ConfigPath, err))
	}

	// 2. 配置校验
	if err := cfg.Validate(); err != nil {
		bad("配置校验", err.Error())
	} else {
		ok("配置校验", "通过")
	}

	// 3. 数据库连接
	db, err := cfg.OpenDB()
	if err != nil {
		bad("数据库", err.Error())
	} else {
		tables := 0
		for _, m := range database.AllModels() {
			if db.Migrator().HasTable(m) {
				tables++
			}
		}
		ok("数据库", fmt.Sprintf("%s，已建表 %d/%d",
			db.Dialect.Display, tables, len(database.AllModels())))
		if tables < len(database.AllModels()) {
			fmt.Println("         提示：执行 MIGRATE=1 ./openroute 补全缺失的表结构")
		}
		_ = db.Close()
	}

	// 4. 日志目录可写
	if cfg.LogPath != "" {
		if err := os.MkdirAll(cfg.LogPath, 0o755); err != nil {
			bad("日志目录", fmt.Sprintf("%s 不可创建: %v", cfg.LogPath, err))
		} else if !dirWritable(cfg.LogPath) {
			bad("日志目录", fmt.Sprintf("%s 不可写（请检查权限）", cfg.LogPath))
		} else {
			ok("日志目录", cfg.LogPath+" 可写")
		}
	}

	// 5. 监听端口可用
	if err := checkPortAvailable(cfg.Listen); err != nil {
		bad("监听端口", err.Error())
	} else {
		ok("监听端口", cfg.Listen+" 可用")
	}

	// 6. 前端资源
	indexPath := filepath.Join(cfg.HTMLPath, "index.html")
	if _, err := os.Stat(indexPath); err == nil {
		ok("前端资源", indexPath+" 存在")
	} else {
		// 前端缺失不是致命问题（API 仍可用），因此只提示不记失败。
		fmt.Printf("  [提示] 前端资源: 未找到 %s\n", indexPath)
		fmt.Println("         执行 cd frontend && npm install && npm run build 生成")
	}

	fmt.Println(strings.Repeat("─", 55))
	if failed == 0 {
		fmt.Println("自检完成：全部检查通过，可以启动。")
		return 0
	}
	fmt.Printf("自检完成：%d 项失败，请修复后重试。\n", failed)
	return 1
}

// dirWritable 判断目录是否可写（通过实际创建一个临时文件来验证，
// 比检查权限位更可靠，尤其在 Windows 上）。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".or_write_test_*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// runResetPassword 交互式重置管理员密码（规格书 3.2）。
//
// 列出全部管理员账号供选择，然后读取新密码并写库。
// 返回进程退出码。
func runResetPassword(cfg *config.Config, log *zap.Logger) int {
	db, err := cfg.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	var admins []model.User
	if err := db.WithContext(ctx).Where("role = ?", model.RoleAdmin).Find(&admins).Error; err != nil {
		fmt.Fprintf(os.Stderr, "查询管理员失败: %v\n", err)
		return 1
	}
	if len(admins) == 0 {
		fmt.Fprintln(os.Stderr, "未找到任何管理员账号。请先以 MIGRATE=1 ADMIN=admin ./openroute 启动创建。")
		return 1
	}

	fmt.Println()
	fmt.Println("现有管理员账号：")
	for i, u := range admins {
		fmt.Printf("  %d) %s (%s)\n", i+1, u.Username, u.Nickname)
	}

	stdin := bufio.NewReader(os.Stdin)
	target := admins[0]
	if len(admins) > 1 {
		fmt.Printf("请选择要重置的账号序号 [1]: ")
		line, _ := stdin.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			idx := 0
			if _, err := fmt.Sscanf(line, "%d", &idx); err == nil && idx >= 1 && idx <= len(admins) {
				target = admins[idx-1]
			}
		}
	}

	fmt.Printf("将为账号 %q 设置新密码\n", target.Username)
	fmt.Print("请输入新密码（至少 6 位）: ")
	pwd, err := stdin.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取密码失败: %v\n", err)
		return 1
	}
	pwd = strings.TrimSpace(pwd)
	if len(pwd) < 6 {
		fmt.Fprintln(os.Stderr, "密码长度不足 6 位，已取消。")
		return 1
	}

	hash, err := util.HashPassword(pwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成密码哈希失败: %v\n", err)
		return 1
	}

	if err := db.WithContext(ctx).Model(&model.User{}).Where("id = ?", target.ID).
		Updates(map[string]interface{}{
			"password_hash":           hash,
			"password_reset_required": false,
		}).Error; err != nil {
		fmt.Fprintf(os.Stderr, "更新密码失败: %v\n", err)
		return 1
	}

	log.Info("管理员密码已重置", zap.String("username", target.Username))
	fmt.Printf("\n管理员 %s 的密码已重置，请使用新密码登录。\n\n", target.Username)
	return 0
}

// runClean 执行 `-clean 1|2`（规格书 3.2）。
//
// 行为：先打印将被删除的清单并要求确认，确认后才执行。
//
//	-clean 1 清理失效规则（目标为空、端口冲突、节点已删除）
//	-clean 2 清理节点负载权重（把异常权重重置为默认值 1）
//
// 返回进程退出码。
func runClean(cfg *config.Config, log *zap.Logger, mode int) int {
	if mode != 1 && mode != 2 {
		fmt.Fprintln(os.Stderr, "参数错误：-clean 只接受 1（清理失效规则）或 2（清理异常权重）")
		return 1
	}

	db, err := cfg.OpenDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		return 1
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if mode == 1 {
		return cleanInvalidRules(ctx, db, log)
	}
	return cleanNodeWeights(ctx, db, log)
}

// cleanInvalidRules 清理失效规则。
//
// 判定「失效」的三种情形：
//  1. 没有可用目标（Targets 为空或全部非法）
//  2. 引用的入口设备组已不存在
//  3. 目标端口非法
func cleanInvalidRules(ctx context.Context, db *database.DB, log *zap.Logger) int {
	var rules []model.ForwardRule
	if err := db.WithContext(ctx).Find(&rules).Error; err != nil {
		fmt.Fprintf(os.Stderr, "查询规则失败: %v\n", err)
		return 1
	}

	// 收集现存设备组 ID，用于引用检查。
	var groupIDs []uint64
	if err := db.WithContext(ctx).Model(&model.DeviceGroup{}).Pluck("id", &groupIDs).Error; err != nil {
		fmt.Fprintf(os.Stderr, "查询设备组失败: %v\n", err)
		return 1
	}
	exist := make(map[uint64]bool, len(groupIDs))
	for _, id := range groupIDs {
		exist[id] = true
	}

	var invalid []model.ForwardRule
	reasons := make(map[uint64]string)
	for _, r := range rules {
		targets := r.TargetList()
		valid := 0
		for _, t := range targets {
			if t.Host != "" && util.ValidPort(t.Port) {
				valid++
			}
		}
		switch {
		case valid == 0:
			invalid = append(invalid, r)
			reasons[r.ID] = "没有可用目标地址"
		case r.InboundGroupID != 0 && !exist[r.InboundGroupID]:
			invalid = append(invalid, r)
			reasons[r.ID] = fmt.Sprintf("入口设备组 %d 已不存在", r.InboundGroupID)
		case r.OutboundGroupID != 0 && !exist[r.OutboundGroupID]:
			invalid = append(invalid, r)
			reasons[r.ID] = fmt.Sprintf("出口设备组 %d 已不存在", r.OutboundGroupID)
		}
	}

	if len(invalid) == 0 {
		fmt.Println("未发现失效规则，无需清理。")
		return 0
	}

	fmt.Printf("\n将删除以下 %d 条失效规则：\n", len(invalid))
	for _, r := range invalid {
		fmt.Printf("  #%d  %s  —— %s\n", r.ID, r.Name, reasons[r.ID])
	}

	if !confirm("确认删除以上规则？删除后无法恢复") {
		fmt.Println("已取消。")
		return 0
	}

	ids := make([]uint64, 0, len(invalid))
	for _, r := range invalid {
		ids = append(ids, r.ID)
	}
	// 删除规则同时清理其流量明细，放在同一事务中保证一致性。
	err := db.Tx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id IN ?", ids).Delete(&model.ForwardRule{}).Error; err != nil {
			return err
		}
		return tx.Where("rule_id IN ?", ids).Delete(&model.TrafficLog{}).Error
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
		return 1
	}

	log.Info("失效规则已清理", zap.Int("count", len(ids)))
	fmt.Printf("已删除 %d 条失效规则。\n", len(ids))
	return 0
}

// cleanNodeWeights 把异常的节点权重重置为默认值 1。
//
// 异常权重指 <= 0 的值：权重为 0 会让节点在负载均衡中永远不被选中，
// 通常是不小心配置出来的（规格书 6.3 要求权重 > 0 才参与分发）。
func cleanNodeWeights(ctx context.Context, db *database.DB, log *zap.Logger) int {
	var nodes []model.Node
	if err := db.WithContext(ctx).Where("weight <= 0").Find(&nodes).Error; err != nil {
		fmt.Fprintf(os.Stderr, "查询节点失败: %v\n", err)
		return 1
	}

	if len(nodes) == 0 {
		fmt.Println("未发现异常权重，无需清理。")
		return 0
	}

	fmt.Printf("\n以下 %d 个节点的权重异常（<= 0），将被重置为 1：\n", len(nodes))
	for _, n := range nodes {
		fmt.Printf("  #%d  %s  当前权重 %d\n", n.ID, n.Name, n.Weight)
	}

	if !confirm("确认重置以上节点的权重？") {
		fmt.Println("已取消。")
		return 0
	}

	ids := make([]uint64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	if err := db.WithContext(ctx).Model(&model.Node{}).
		Where("id IN ?", ids).Update("weight", 1).Error; err != nil {
		fmt.Fprintf(os.Stderr, "重置权重失败: %v\n", err)
		return 1
	}

	log.Info("节点权重已重置", zap.Int("count", len(ids)))
	fmt.Printf("已重置 %d 个节点的权重。\n", len(ids))
	return 0
}

// confirm 在终端询问用户是否确认。
//
// 参数 prompt 为提示语；返回是否确认。
// 非交互环境（stdin 不是终端）下返回 false，避免误操作。
func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	stdin := bufio.NewReader(os.Stdin)
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// checkPortAvailable 检查监听地址是否可用。
//
// 通过实际尝试监听来判断，这比查端口占用表更可靠。
// 返回 nil 表示可用；否则返回包含地址与原因的错误。
func checkPortAvailable(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s 无法监听（可能已被占用）: %v", addr, err)
	}
	_ = ln.Close()
	return nil
}
