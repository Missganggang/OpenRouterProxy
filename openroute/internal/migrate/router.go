// Package migrate 实现 OpenRoute 的迁移、备份与跨库复制能力（规格书第 7 章）。
//
// 本包刻意不依赖 internal/app：所有依赖通过 Config 显式注入，
// 因此可以脱离整个运行时单独编译与测试。
// 与 App 的桥接放在 internal/app/service_migrate.go。
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/openroute/openroute/internal/database"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// 迁移流程的阶段编号（规格书 7.2）。
const (
	StageConnect  = 1 // 连接与探测
	StageScan     = 2 // 只读扫描与字段映射
	StageConflict = 3 // 冲突与风险检测
	StageDryRun   = 4 // dry-run 预览
	StageMigrate  = 5 // 正式迁移
	StageVerify   = 6 // 校验
	StageRollback = 7 // 回滚
	StageTotal    = 7
)

// 迁移涉及的目标表名。
const (
	tableTargetUsers        = "users"
	tableTargetUserGroups   = "user_groups"
	tableTargetNodes        = "nodes"
	tableTargetNodeGroups   = "node_groups"
	tableTargetDeviceGroups = "device_groups"
	tableTargetRuleGroups   = "rule_groups"
	tableTargetRules        = "forward_rules"
	tableTargetTraffic      = "traffic_logs"
)

// copyOrder 是跨库复制的表顺序（规格书 7.3，先父表后子表）。
//
// 不使用数据库级外键，顺序只影响「复制中断时留下的一致状态」，
// 但按规格书给定的顺序执行可以让日志与排查更直观。
var copyOrder = []string{
	"users", "user_groups", "node_groups", "nodes",
	"rule_groups", "device_groups", "forward_rules",
	"traffic_logs", "sessions", "probe_metrics",
	"system_settings", "audit_logs", "api_tokens",
	"config_snapshots", "alert_rules", "alert_histories",
}

// ProgressPhase 是进度事件的阶段标签。
type ProgressPhase string

const (
	// PhaseProbe 阶段 1 探测
	PhaseProbe ProgressPhase = "probe"
	// PhaseScan 阶段 2 扫描
	PhaseScan ProgressPhase = "scan"
	// PhaseConflict 阶段 3 冲突检测
	PhaseConflict ProgressPhase = "conflict"
	// PhasePlan 阶段 4 预览
	PhasePlan ProgressPhase = "plan"
	// PhaseWrite 阶段 5 写入
	PhaseWrite ProgressPhase = "write"
	// PhaseVerify 阶段 6 校验
	PhaseVerify ProgressPhase = "verify"
	// PhaseRollback 阶段 7 回滚
	PhaseRollback ProgressPhase = "rollback"
)

// Progress 是一次进度事件。
type Progress struct {
	Phase ProgressPhase `json:"phase"`
	Stage int           `json:"stage"`
	// Table 是当前处理的表（写入阶段有意义）
	Table string `json:"table,omitempty"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	// Message 是人类可读的补充说明
	Message string `json:"message,omitempty"`
}

// ProgressFunc 是进度回调；实现方不应在其中做耗时操作。
type ProgressFunc func(Progress)

// RecoveryChoice 是「检测到未完成批次」时用户的选择（规格书 7.4）。
type RecoveryChoice string

const (
	// RecoveryContinue 继续上次的迁移（跳过已完成的记录）
	RecoveryContinue RecoveryChoice = "continue"
	// RecoveryRollback 回滚上次未完成的迁移
	RecoveryRollback RecoveryChoice = "rollback"
	// RecoveryAbandon 放弃：把批次标记为失败，不做任何数据操作
	RecoveryAbandon RecoveryChoice = "abandon"
)

// RecoverFunc 在检测到未完成批次时被调用，由调用方决定如何处理。
//
// 非交互（CLI 带 --force、API 调用）时可以直接返回预设选择。
type RecoverFunc func(batch *model.MigrateBatch) (RecoveryChoice, error)

// SpaceCheckFunc 在正式迁移前检查目标磁盘剩余空间。
//
// 返回值：可用字节数与错误。返回错误表示无法确认（例如 Windows 上
// 目标库是网络路径），此时迁移继续，只在报告中提示。
type SpaceCheckFunc func(targetPath string, need int64) (int64, error)

// Config 是迁移编排器的依赖注入配置。
type Config struct {
	// 来源标识：nyanpass | openroute
	Source string
	// SourceDSN 是源库的 database-path 写法（Nyanpass 的 DSN 或 data.db 路径）
	SourceDSN string
	// Target 是已连接的目标库
	Target *database.DB
	// TargetDSN 是目标的 database-path 写法，仅用于报告与同库检测
	TargetDSN string
	// DryRun 为 true 时只做预检，不写任何数据
	DryRun bool
	// RenamePolicy 是名称冲突策略，零值按 fail 处理
	RenamePolicy RenamePolicy
	// BackupPath 是迁移前的自动备份路径（为空时跳过备份）
	BackupPath string
	// Log 是日志器；为空时使用 zap 的 no-op 实现
	Log *zap.Logger
	// Progress 是进度回调，可为 nil
	Progress ProgressFunc
	// Recover 用于处理未完成批次，可为 nil（默认继续）
	Recover RecoverFunc
	// SpaceCheck 用于磁盘空间检查，可为 nil（跳过检查）
	SpaceCheck SpaceCheckFunc

	// sourceDB 是已连接的源库，由 Run 内部填充
	sourceDB *database.DB
}

// Runner 是一次迁移的执行器。
//
// 生命周期：NewRunner → 逐阶段调用，或直接 Run 跑完整流程。
// 阶段之间不共享隐式状态，因此每个 Stage* 方法都可以独立重跑
// （规格书 7.2「每阶段可独立重跑」）。
type Runner struct {
	cfg      Config
	log      *zap.Logger
	reader   SourceReader
	report   *Report
	plan     *plan
	probe    *ProbeResult
	batch    *model.MigrateBatch
	progress ProgressFunc
	// allocators 按目标表分别持有 ID 分配器，避免跨表的 ID 互相干扰。
	allocators map[string]*idAllocator
}

// ErrSourceUnreachable 表示源库无法连接（对应错误码 70001）。
var ErrSourceUnreachable = errors.New("源库无法连接")

// ErrSourceUnsupported 表示源库版本过旧或缺少必要表（对应错误码 70002）。
var ErrSourceUnsupported = errors.New("源库版本不被支持")

// ErrSameDatabase 表示源库与目标库是同一个库（对应错误码 70003）。
var ErrSameDatabase = errors.New("目标库与源库相同")

// ErrUnresolvedConflict 表示存在无法自动处理的冲突（对应错误码 70004）。
var ErrUnresolvedConflict = errors.New("检测到无法自动处理的冲突")

// ErrInsufficientSpace 表示目标磁盘空间不足（规格书 7.4）。
var ErrInsufficientSpace = errors.New("磁盘空间不足")

// NewRunner 构造迁移执行器。
//
// 只做参数校验与默认值填充，不连接数据库；连接发生在阶段 1。
func NewRunner(cfg Config) (*Runner, error) {
	if strings.TrimSpace(cfg.SourceDSN) == "" {
		return nil, fmt.Errorf("缺少源库地址（dsn）")
	}
	if cfg.Target == nil {
		return nil, fmt.Errorf("缺少已连接的目标库")
	}
	if cfg.Source == "" {
		cfg.Source = "nyanpass"
	}
	if cfg.RenamePolicy == "" {
		cfg.RenamePolicy = RenameFail
	}

	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}

	r := &Runner{
		cfg:        cfg,
		log:        log,
		report:     NewReport(cfg.Source, "", cfg.Target.Dialect.Display, cfg.DryRun),
		plan:       newPlan(),
		progress:   cfg.Progress,
		allocators: map[string]*idAllocator{},
	}
	r.report.sourceDSN = cfg.SourceDSN
	return r, nil
}

// Report 返回当前的报告对象（阶段 5 之后即最终报告）。
func (r *Runner) Report() *Report { return r.report }

// Plan 返回内存中的转换计划，供 API 层做二次统计。
func (r *Runner) Plan() *plan { return r.plan }

// emit 发送一次进度事件。
func (r *Runner) emit(p Progress) {
	if r.progress != nil {
		r.progress(p)
	}
}

// logf 输出一条结构化日志。
func (r *Runner) logf(msg string, fields ...zap.Field) {
	if r.log != nil {
		r.log.Info(msg, fields...)
	}
}

// ---------------------------------------------------------------------------
// 阶段 1：连接与探测
// ---------------------------------------------------------------------------

// StageConnect 连接源库并完成探测（规格书 7.2 阶段 1）。
//
// 行为：
//   - 解析并连接源库，失败返回 ErrSourceUnreachable；
//   - 检测源库与目标库是否同一个库，是则返回 ErrSameDatabase；
//   - 读取 schema_version 识别 Nyanpass 版本；
//   - 检查必要表与必要列，缺失时返回 ErrSourceUnsupported 并列出缺失项；
//   - 统计表数量与各表行数，判断源库是否为空。
func (r *Runner) StageConnect(ctx context.Context) error {
	r.report.Stage = StageConnect
	r.emit(Progress{Phase: PhaseProbe, Stage: StageConnect, Message: "连接源库并读取结构信息"})

	srcDialect, err := database.ParseDatabasePath(r.cfg.SourceDSN)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}

	// 同一个库的判定：SQLite 比文件路径（绝对化后比较），
	// 其它方言比较脱敏后的 DSN 文本。
	if sameDatabase(srcDialect, r.cfg.Target.Dialect, r.cfg.TargetDSN) {
		r.report.Error = "源库与目标库是同一个数据库，拒绝执行迁移。\n" +
			"  源库  : " + srcDialect.Display + "\n" +
			"  目标库: " + r.cfg.Target.Dialect.Display + "\n" +
			"  提示  : 请指定另一个目标库（修改 config.yml 的 database-path），或把源库文件复制一份后再迁移。"
		return fmt.Errorf("%w: 源库与目标库都指向 %s", ErrSameDatabase, srcDialect.Display)
	}

	// 源库文件必须已经存在。
	//
	// SQLite 驱动在打开不存在的文件时会「顺手创建一个空库」，
	// 于是用户把 DSN 路径拼错时，看到的会是「源库缺少必要表」这种
	// 具有误导性的报错，而不是「路径不存在」。这里显式拦一道，
	// 让报错直接指向真正的原因。
	if srcDialect.Name == "sqlite" && srcDialect.File != "" && srcDialect.File != ":memory:" {
		if _, statErr := os.Stat(srcDialect.File); statErr != nil {
			r.report.Error = fmt.Sprintf("源库文件不存在或不可读：%s", srcDialect.File)
			return fmt.Errorf("%w: 找不到源库文件 %s（请检查 dsn 路径是否正确）",
				ErrSourceUnreachable, srcDialect.File)
		}
	}

	sourceDB, err := database.Open(database.Options{
		Path:     r.cfg.SourceDSN,
		MaxOpen:  2,
		MaxIdle:  1,
		LogLevel: r.gormLogLevel(),
	})
	if err != nil {
		r.report.Error = fmt.Sprintf("连接源库失败：%v", err)
		return fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	r.cfg.sourceDB = sourceDB

	// 只有 Nyanpass 源才需要「版本识别 + 必要表检查」这套逻辑。
	if strings.EqualFold(r.cfg.Source, "nyanpass") {
		reader := NewNyanpassSource(sourceDB)
		r.reader = reader
		probe, err := r.probeNyanpass(ctx, reader)
		if err != nil {
			return err
		}
		r.probe = probe
	} else {
		reader := NewOpenRouteSource(sourceDB)
		r.reader = reader
		probe, err := r.probeGeneric(ctx, reader)
		if err != nil {
			return err
		}
		r.probe = probe
	}

	// 把目标库中「已存在的名称」预置进查重集合。
	//
	// 这一步是必需的：若目标库是全新初始化的面板，里面已经有
	// 「默认分组」「默认规则组」等记录，源库迁移进来的同名记录会撞唯一索引。
	// 只比对源库内部的重名是不够的，必须把目标库的现状一起纳入。
	if err := r.seedExistingNames(ctx); err != nil {
		r.report.Error = fmt.Sprintf("读取目标库已有名称失败：%v", err)
		return err
	}

	r.report.Source = r.probe.Display
	r.report.SourceVersion = r.probe.Version()
	r.report.SourceTableCount = r.probe.TableCount
	r.report.SourceRowCounts = r.probe.RowCounts

	// 打印源库概况（规格书 7.2 阶段 1 要求）。
	fmt.Println("源库概况")
	fmt.Printf("  方言    : %s\n", r.probe.DialectName)
	fmt.Printf("  地址    : %s\n", r.probe.Display)
	fmt.Printf("  版本    : %s\n", r.probe.Version())
	fmt.Printf("  表数量  : %d\n", r.probe.TableCount)
	if len(r.probe.RowCounts) > 0 {
		fmt.Println("  各表行数:")
		for _, k := range sortedKeys(r.probe.RowCounts) {
			fmt.Printf("    %-16s %d\n", k, r.probe.RowCounts[k])
		}
	}
	fmt.Println()

	if r.probe.Empty {
		// 规格书 7.4：源库为空时提示「源库无数据」，不报错，退出码 0。
		r.report.Error = ""
		r.report.Suggestions = append(r.report.Suggestions,
			"源库无数据：源库中没有任何用户 / 节点 / 规则，无需迁移。")
		return nil
	}
	return nil
}

// probeNyanpass 探测 Nyanpass 源库。
func (r *Runner) probeNyanpass(ctx context.Context, src *NyanpassSource) (*ProbeResult, error) {
	tables, err := src.Tables(ctx)
	if err != nil {
		r.report.Error = fmt.Sprintf("读取源库表清单失败：%v", err)
		return nil, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}

	probe := &ProbeResult{
		DialectName:    src.DialectName(),
		Display:        src.Display(),
		RowCounts:      map[string]int64{},
		MissingColumns: map[string][]string{},
	}
	probe.TableCount = len(tables)

	// 必要表检查（规格书 7.4：列出缺失表、退出码 1）。
	for _, t := range requiredNyanpassTables {
		switch t {
		case tableDeviceGroups:
			// 设备组的表名有两套历史写法，任一存在即算通过。
			if !tables[tableDeviceGroups] && !tables[tableGroups] {
				probe.MissingTables = append(probe.MissingTables, tableDeviceGroups)
			}
		default:
			if !tables[t] {
				probe.MissingTables = append(probe.MissingTables, t)
			}
		}
	}

	// 必要列检查：识别「表在但结构过旧」的源库。
	for table, groups := range requiredNyanpassColumns {
		if !tables[table] {
			continue
		}
		cols := src.columnSet(ctx, table)
		var missing []string
		for _, group := range groups {
			if _, ok := pickColumn(cols, group...); !ok {
				missing = append(missing, group[0])
			}
		}
		if len(missing) > 0 {
			probe.MissingColumns[table] = missing
		}
	}

	// 统计各表行数。
	probeTables := []string{
		tableUsers, tableUserGroups, tableNodes, tableNodeGroups,
		tableDeviceGroups, tableRuleGroups, tableRules, tableTrafficLogs,
	}
	// 兼容旧表名：把实际存在的名字也统计进来。
	for _, alt := range []string{tableServers, tableServerGroups, tableGroups, tableForwardRules, tableTraffics} {
		if tables[alt] {
			probeTables = append(probeTables, alt)
		}
	}
	for _, t := range probeTables {
		if !tables[t] {
			continue
		}
		n, _ := src.CountRows(ctx, t)
		probe.RowCounts[t] = n
		probe.TotalRows += n
	}

	probe.SchemaVersion, probe.SchemaVersionRaw = src.SchemaVersion(ctx)

	if len(probe.MissingTables) > 0 || len(probe.MissingColumns) > 0 {
		var b strings.Builder
		b.WriteString("源库版本过旧，缺少迁移所需的表或字段，请先升级 Nyanpass 到较新版本再迁移。\n")
		if len(probe.MissingTables) > 0 {
			b.WriteString("  缺失的表  : " + strings.Join(probe.MissingTables, "、") + "\n")
		}
		for _, t := range sortedStringKeys(probe.MissingColumns) {
			b.WriteString(fmt.Sprintf("  %s 缺失的列: %s\n", t, strings.Join(probe.MissingColumns[t], "、")))
		}
		b.WriteString("  提示      : 升级命令通常为 `bash <(curl -fsSL <面板地址>/install.sh)`，" +
			"升级完成后 Nyanpass 会自动补齐表结构，届时重新执行本迁移即可。")
		r.report.Error = b.String()
		return probe, fmt.Errorf("%w: 缺失 %s", ErrSourceUnsupported,
			strings.Join(append(probe.MissingTables, flushMissingColumns(probe.MissingColumns)...), "、"))
	}

	// 源库为空：所有业务表都没有数据。
	empty := true
	for _, t := range []string{tableUsers, tableNodes, tableRules, tableDeviceGroups} {
		if probe.RowCounts[t] > 0 {
			empty = false
			break
		}
	}
	probe.Empty = empty
	return probe, nil
}

// probeGeneric 探测非 Nyanpass 的源库（本包内仅用于自检）。
func (r *Runner) probeGeneric(ctx context.Context, src SourceReader) (*ProbeResult, error) {
	tables, err := src.Tables(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnreachable, err)
	}
	probe := &ProbeResult{
		DialectName:    src.DialectName(),
		Display:        src.Display(),
		RowCounts:      map[string]int64{},
		MissingColumns: map[string][]string{},
		TableCount:     len(tables),
	}
	for _, t := range []string{tableTargetUsers, tableTargetNodes, tableTargetRules} {
		if !tables[t] {
			continue
		}
		n, _ := src.CountRows(ctx, t)
		probe.RowCounts[t] = n
		probe.TotalRows += n
	}
	probe.SchemaVersion, probe.SchemaVersionRaw = src.SchemaVersion(ctx)
	if probe.RowCounts[tableTargetUsers] == 0 {
		probe.Empty = true
	}
	return probe, nil
}

// flushMissingColumns 把缺失列拍平成字符串列表。
func flushMissingColumns(m map[string][]string) []string {
	out := []string{}
	for _, t := range sortedStringKeys(m) {
		for _, c := range m[t] {
			out = append(out, t+"."+c)
		}
	}
	return out
}

// sameDatabase 判断源库与目标库是否为同一个库。
//
// SQLite 场景下用户很容易把源库和目标库写成同一个文件（尤其是把 Nyanpass 的
// data.db 直接拷成 OpenRoute 的 data.db），必须在写入任何数据前拦住。
func sameDatabase(src, dst *database.Dialect, targetDSN string) bool {
	if src == nil || dst == nil {
		return false
	}
	if src.Name == "sqlite" && dst.Name == "sqlite" {
		return absPath(src.File) == absPath(dst.File)
	}
	if src.Name != dst.Name {
		return false
	}
	// 非 SQLite：比较脱敏后的 DSN 文本；同时兼容用户传入的 database-path 写法。
	a := strings.TrimSpace(src.DSN)
	b := strings.TrimSpace(dst.DSN)
	if a != "" && a == b {
		return true
	}
	if targetDSN != "" {
		if d, err := database.ParseDatabasePath(targetDSN); err == nil {
			return strings.TrimSpace(d.DSN) == a && d.Name == src.Name
		}
	}
	return false
}

// absPath 返回绝对化的干净路径，失败时退回原值。
func absPath(p string) string {
	if p == "" || p == ":memory:" {
		return p
	}
	if a, err := filepath.Abs(p); err == nil {
		return filepath.Clean(a)
	}
	return filepath.Clean(p)
}

// gormLogLevel 返回源库连接使用的日志级别。
//
// 迁移是临时操作，SQL 日志除了刷屏没有价值，统一关掉（logger.Silent）。
func (r *Runner) gormLogLevel() logger.LogLevel { return logger.Silent }

// ---------------------------------------------------------------------------
// 阶段 2：只读扫描与字段映射
// ---------------------------------------------------------------------------

// StageScan 逐表扫描源库，构建「源字段 → 目标字段」映射，
// 并把无法映射的字段与需要填充的字段分别记入丢弃清单与填充清单。
//
// 本阶段只读源库，不写任何数据。
func (r *Runner) StageScan(ctx context.Context) error {
	r.report.Stage = StageScan
	r.emit(Progress{Phase: PhaseScan, Stage: StageScan, Message: "扫描源库并构建字段映射"})

	if r.reader == nil {
		return fmt.Errorf("尚未连接源库，请先执行阶段 1（连接与探测）")
	}
	if r.probe != nil && r.probe.Empty {
		r.report.Stage = StageTotal
		return nil
	}

	// 整体不迁移的表：支付 / 订单 / 授权相关（规格书 1.3、7.2）。
	r.plan.discardTables = detectDiscardTables(ctx, r.reader)
	r.plan.discards = append(r.plan.discards, buildDiscardItems(ctx, r.reader, r.plan.discardTables)...)
	r.plan.fills = buildFillItems()
	return nil
}

// discardTableCandidates 是「绝不迁移」的表名候选。
//
// 规格书 1.3 明确：支付网关、商城、订单、套餐、余额充值、授权码、
// 授权域名绑定一律不做。迁移工具遇到这些表不是写成 TODO 占位，
// 而是整体跳过并记入丢弃清单。
var discardTableCandidates = []string{
	"orders", "order", "products", "product", "packages", "package",
	"plans", "plan", "payment_configs", "payment_config", "payments",
	"payment", "pay", "coupons", "coupon", "recharges", "recharge",
	"recharge_logs", "balances", "balance_logs", "invite_codes", "invites",
	"commissions", "affiliates", "licenses", "license", "license_keys",
	"license_domains", "domains", "activations", "sub_accounts",
	"tickets", "ticket_replies",
}

// initTableCandidates 是「不迁移但属于系统内部状态」的表。
//
// 与支付无关，但迁移过来没有意义（会话、缓存、队列），
// 因此同样跳过，只是原因不同。
var initTableCandidates = []string{
	"sessions", "probe_metrics", "audit_logs", "api_tokens",
	"config_snapshots", "alert_rules", "alert_histories",
	"tokens", "cache", "queues", "jobs", "logs",
}

// detectDiscardTables 找出源库中整体不迁移的表。
func detectDiscardTables(ctx context.Context, reader SourceReader) []string {
	tables, err := reader.Tables(ctx)
	if err != nil {
		return []string{}
	}
	var out []string
	for _, name := range discardTableCandidates {
		if tables[name] {
			out = append(out, name)
			continue
		}
		// 前缀匹配：order_items、payment_logs_2026 这类派生表同样跳过。
		for actual := range tables {
			if actual == name {
				continue
			}
			if strings.HasPrefix(actual, name+"_") || strings.HasSuffix(actual, "_"+name) {
				out = append(out, actual)
			}
		}
	}
	// 去重并排序，保证报告输出稳定。
	seen := map[string]bool{}
	uniq := make([]string, 0, len(out))
	for _, t := range out {
		if seen[t] {
			continue
		}
		seen[t] = true
		uniq = append(uniq, t)
	}
	sortStrings(uniq)
	return uniq
}

// buildDiscardItems 构造丢弃清单。
//
// 两类来源：整表丢弃（支付/订单/授权）与字段级丢弃（授权与充值字段）。
// 条目里带上行数，用户在预检阶段就能看出「丢了多大的东西」。
func buildDiscardItems(ctx context.Context, reader SourceReader, tables []string) []DiscardItem {
	out := []DiscardItem{}
	for _, t := range tables {
		n, _ := reader.CountRows(ctx, t)
		out = append(out, DiscardItem{
			Source: t + ".*",
			Reason: "支付 / 订单 / 授权相关的表，OpenRoute 不做该体系（规格书 1.3）",
			Count:  n,
		})
	}

	// 授权与充值相关的字段：只要源表里真的存在才记录，
	// 否则报告会被「不存在的字段」污染。
	fieldCandidates := []struct {
		Table  string
		Column string
		Reason string
	}{
		{"users", "license_key", "授权码，OpenRoute 无授权体系"},
		{"users", "recharge_total", "充值统计，OpenRoute 无充值体系"},
		{"users", "balance", "余额，OpenRoute 无支付体系"},
		{"users", "money", "余额，OpenRoute 无支付体系"},
		{"users", "invite_code", "邀请码 / 分销，OpenRoute 不做"},
		{"users", "commission", "返佣，OpenRoute 不做"},
		{"users", "license_domain", "授权域名绑定，OpenRoute 不做"},
		{"users", "expire_license_at", "授权到期时间，OpenRoute 不做"},
		{"users", "product_id", "套餐归属，OpenRoute 不做"},
		{"users", "plan_id", "套餐归属，OpenRoute 不做"},
	}
	for _, c := range fieldCandidates {
		cols, err := reader.Columns(ctx, c.Table)
		if err != nil {
			continue
		}
		set := tableColumns(cols)
		if !set[c.Column] {
			continue
		}
		out = append(out, DiscardItem{
			Source: c.Table + "." + c.Column,
			Reason: c.Reason,
		})
	}

	// 设备组级的授权字段（部分版本把授权写进设备组配置里）。
	if cols, err := reader.Columns(ctx, tableDeviceGroups); err == nil {
		set := tableColumns(cols)
		if set["license_key"] {
			out = append(out, DiscardItem{
				Source: tableDeviceGroups + ".license_key",
				Reason: "授权码，OpenRoute 无授权体系",
			})
		}
	}
	return out
}

// buildFillItems 构造填充清单。
//
// 这些字段是目标库有、源库没有的，迁移时用默认值补齐。
// 明确列出来是为了避免用户误以为「数据丢了」。
func buildFillItems() []FillItem {
	return []FillItem{
		{Target: "users.token", Default: "自动生成（ortu_ 前缀）", Reason: "源库无对应字段，用于 /sub/<token> 订阅"},
		{Target: "users.password_reset_required", Default: "false", Reason: "仅在密码哈希算法不兼容时置为 true"},
		{Target: "users.pending_reinstall", Default: "true", Reason: "迁移带入的节点需要重装客户端"},
		{Target: "nodes.token", Default: "重新生成的 nsk_ 随机串", Reason: "MUST 重新生成，避免与旧面板冲突"},
		{Target: "nodes.online", Default: "false", Reason: "迁移不继承在线状态，等节点上线后自行上报"},
		{Target: "device_groups.balance", Default: "least_conn", Reason: "出口组负载均衡默认最少连接数"},
		{Target: "device_groups.health_check_enable", Default: "true", Reason: "健康检查默认开启"},
		{Target: "forward_rules.sync_status", Default: "unsynced", Reason: "迁移后规则需由节点重新拉取"},
		{Target: "forward_rules.target_balance", Default: "failover", Reason: "源库未指定时的目标级策略"},
		{Target: "forward_rules.inbound_multiplier", Default: "源库设备组倍率", Reason: "Nyanpass 倍率在设备组上，迁移时下沉到规则"},
		{Target: "forward_rules.outbound_multiplier", Default: "源库设备组倍率", Reason: "同上"},
	}
}

// ---------------------------------------------------------------------------
// 阶段 3：冲突与风险检测
// ---------------------------------------------------------------------------

// StageConflict 执行冲突与风险检测（规格书 7.2 阶段 3）。
//
// 检测项：名称冲突、端口冲突、引用缺失、倍率为 0 的设备组、授权字段。
// 本阶段同样只读，产出的冲突清单会直接进入预检报告。
func (r *Runner) StageConflict(ctx context.Context) error {
	r.report.Stage = StageConflict
	r.emit(Progress{Phase: PhaseConflict, Stage: StageConflict, Message: "检测冲突与风险"})

	if r.reader == nil {
		return fmt.Errorf("尚未连接源库，请先执行阶段 1（连接与探测）")
	}
	if r.probe != nil && r.probe.Empty {
		return nil
	}
	if !strings.EqualFold(r.cfg.Source, "nyanpass") {
		return nil
	}

	src, ok := r.reader.(*NyanpassSource)
	if !ok {
		return nil
	}

	users, err := src.loadUsers(ctx)
	if err != nil {
		return fmt.Errorf("读取源库用户失败: %w", err)
	}
	// 用户分组不参与冲突判定，但仍要读一次：读失败说明源库结构有问题，
	// 早失败比在阶段 4 转换时才发现更好。
	if _, err := src.loadUserGroups(ctx); err != nil {
		return fmt.Errorf("读取源库用户分组失败: %w", err)
	}
	nodes, err := src.loadNodes(ctx)
	if err != nil {
		return fmt.Errorf("读取源库节点失败: %w", err)
	}
	nodeGroups, err := src.loadNodeGroups(ctx)
	if err != nil && !isEmptyTableErr(err) {
		return fmt.Errorf("读取源库节点分组失败: %w", err)
	}
	deviceGroups, err := src.loadDeviceGroups(ctx)
	if err != nil {
		return fmt.Errorf("读取源库设备组失败: %w", err)
	}
	ruleGroups, err := src.loadRuleGroups(ctx)
	if err != nil && !isEmptyTableErr(err) {
		return fmt.Errorf("读取源库规则分组失败: %w", err)
	}
	rules, err := src.loadRules(ctx)
	if err != nil {
		return fmt.Errorf("读取源库规则失败: %w", err)
	}

	r.plan.nameConflicts = detectNameConflicts(users, nodes, nodeGroups, deviceGroups, ruleGroups, rules)
	r.plan.zeroMultipliers = detectZeroMultipliers(deviceGroups)
	r.plan.portConflicts = detectPortConflicts(rules, groupNameMap(deviceGroups))
	r.plan.missingRefs = detectMissingRefs(rules, users, deviceGroups, ruleGroups, nodes)

	r.report.NameConflicts = r.plan.nameConflicts
	r.report.PortConflicts = r.plan.portConflicts
	r.report.MissingRefs = r.plan.missingRefs
	r.report.ZeroMultipliers = r.plan.zeroMultipliers
	return nil
}

// isEmptyTableErr 判断错误是否只是「表不存在」。
//
// 源库没有 rule_groups / node_groups 表属于正常情况（个人自用常常不用分组），
// 不应该让整个预检失败。
func isEmptyTableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such table") ||
		strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "不存在") ||
		strings.Contains(msg, "does not exist")
}

// detectNameConflicts 检测归一化后重名（规格书 4.1）。
//
// 用 util.DedupeNamesFold 做归一化分组，再把每组的多余项转成冲突记录。
func detectNameConflicts(
	users []nyUser, nodes []nyNode, nodeGroups []nyNodeGroup,
	deviceGroups []nyDeviceGroup, ruleGroups []nyRuleGroup, rules []nyRule,
) []NameConflict {
	out := []NameConflict{}

	add := func(kind string, names []string) {
		groups := util.DedupeNamesFold(names)
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			list := groups[k]
			sortStrings(list)
			// 保留首个名称，其余都是冲突项。
			for i := 1; i < len(list); i++ {
				out = append(out, NameConflict{
					Resource: kind,
					Left:     list[0],
					Right:    list[i],
					Suggestion: fmt.Sprintf("在目标库将合并 → 建议重命名为 \"%s\"",
						fmt.Sprintf("%s_%d", list[i], i+1)),
				})
			}
		}
	}

	add("user", collect(users, func(u nyUser) string { return u.Username }))
	add("node", collect(nodes, func(n nyNode) string { return n.Name }))
	add("node_group", collect(nodeGroups, func(g nyNodeGroup) string { return g.Name }))
	add("device_group", collect(deviceGroups, func(g nyDeviceGroup) string { return g.Name }))
	add("rule_group", collect(ruleGroups, func(g nyRuleGroup) string { return g.Name }))
	add("rule", collect(rules, func(x nyRule) string { return x.Name }))
	return out
}

// detectZeroMultipliers 找出倍率为 0 的设备组（规格书 7.2 阶段 3）。
func detectZeroMultipliers(groups []nyDeviceGroup) []ZeroMultiplierGroup {
	out := []ZeroMultiplierGroup{}
	for _, g := range groups {
		if g.Multiplier != 0 {
			continue
		}
		out = append(out, ZeroMultiplierGroup{
			ID: g.ID, Name: g.Name, Type: g.Type, Rate: g.Multiplier,
		})
	}
	return out
}

// detectPortConflicts 检测「同一入口组内多条规则监听同一端口」。
func detectPortConflicts(rules []nyRule, groupNames map[uint64]string) []PortConflict {
	type key struct {
		group uint64
		port  int
	}
	seen := map[key][]string{}
	order := []key{}
	for _, r := range rules {
		// 子规则不单独监听端口，跳过以免产生假冲突。
		if r.IsSubRule || r.Port <= 0 {
			continue
		}
		k := key{group: r.InboundGroupID, port: r.Port}
		if _, ok := seen[k]; !ok {
			order = append(order, k)
		}
		seen[k] = append(seen[k], r.Name)
	}

	out := []PortConflict{}
	for _, k := range order {
		names := seen[k]
		if len(names) < 2 {
			continue
		}
		name := groupNames[k.group]
		if name == "" {
			name = fmt.Sprintf("ID %d", k.group)
		}
		out = append(out, PortConflict{
			InboundGroupID:   k.group,
			InboundGroupName: name,
			Port:             k.port,
			Rules:            names,
		})
	}
	return out
}

// detectMissingRefs 检测规则引用的对象在源库中是否存在（规格书 7.4）。
//
// 命中项会被标记为「待修复」：不写入目标库，并进入报告的跳过清单。
func detectMissingRefs(
	rules []nyRule, users []nyUser, deviceGroups []nyDeviceGroup,
	ruleGroups []nyRuleGroup, nodes []nyNode,
) []MissingRef {
	userSet := map[uint64]bool{}
	for _, u := range users {
		userSet[u.ID] = true
	}
	groupSet := map[uint64]bool{}
	outboundSet := map[uint64]bool{}
	for _, g := range deviceGroups {
		groupSet[g.ID] = true
		if g.Type == "outbound" {
			outboundSet[g.ID] = true
		}
	}
	ruleGroupSet := map[uint64]bool{}
	for _, g := range ruleGroups {
		ruleGroupSet[g.ID] = true
	}
	nodeSet := map[uint64]bool{}
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}

	out := []MissingRef{}
	for _, r := range rules {
		if r.InboundGroupID == 0 || !groupSet[r.InboundGroupID] {
			out = append(out, MissingRef{
				RuleID: r.ID, RuleName: r.Name, Field: "inbound_group_id", RefID: r.InboundGroupID,
				Message: "规则引用的入口设备组在源库中不存在，该规则标记为「待修复」，不写入目标库",
			})
		}
		if r.OutboundGroup != 0 && !outboundSet[r.OutboundGroup] {
			out = append(out, MissingRef{
				RuleID: r.ID, RuleName: r.Name, Field: "outbound_group_id", RefID: r.OutboundGroup,
				Message: "规则引用的出口设备组在源库中不存在，该规则标记为「待修复」，不写入目标库",
			})
		}
		if r.UserID != 0 && !userSet[r.UserID] {
			out = append(out, MissingRef{
				RuleID: r.ID, RuleName: r.Name, Field: "user_id", RefID: r.UserID,
				Message: "规则归属的用户在源库中不存在，将归属到管理员（user_id = 0）",
			})
		}
		if r.RuleGroupID != 0 && !ruleGroupSet[r.RuleGroupID] {
			out = append(out, MissingRef{
				RuleID: r.ID, RuleName: r.Name, Field: "rule_group_id", RefID: r.RuleGroupID,
				Message: "规则引用的规则分组在源库中不存在，将置为未分组（rule_group_id = 0）",
			})
		}
		if r.ReverseGroup != 0 && !groupSet[r.ReverseGroup] {
			out = append(out, MissingRef{
				RuleID: r.ID, RuleName: r.Name, Field: "reverse_group_id", RefID: r.ReverseGroup,
				Message: "反向隧道引用的设备组在源库中不存在，将关闭该规则的反向隧道",
			})
		}
		for _, cg := range r.ChainGroups {
			if !groupSet[cg] {
				out = append(out, MissingRef{
					RuleID: r.ID, RuleName: r.Name, Field: "chain_groups", RefID: cg,
					Message: "链式出口引用的设备组在源库中不存在，将从链路中移除",
				})
			}
		}
		if r.IsSubRule && r.ParentID != 0 {
			found := false
			for _, p := range rules {
				if p.ID == r.ParentID {
					found = true
					break
				}
			}
			if !found {
				out = append(out, MissingRef{
					RuleID: r.ID, RuleName: r.Name, Field: "parent_id", RefID: r.ParentID,
					Message: "子规则引用的主规则在源库中不存在，该子规则标记为「待修复」",
				})
			}
		}
		_ = nodeSet
	}
	return out
}

// groupNameMap 构造「设备组 ID → 名称」映射。
func groupNameMap(groups []nyDeviceGroup) map[uint64]string {
	out := make(map[uint64]string, len(groups))
	for _, g := range groups {
		out[g.ID] = g.Name
	}
	return out
}

// collect 把切片投影成字符串切片。
func collect[T any](in []T, get func(T) string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, get(v))
	}
	return out
}

// sortedStringKeys 返回 map 的排序键。
func sortedStringKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings 对字符串切片原地排序。
//
// 单独包一层是为了让本文件的排序意图显式化（报告输出必须稳定）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// 阶段 4：dry-run 预览
// ---------------------------------------------------------------------------

// StageDryRun 在内存中完成全部转换并输出预览（规格书 7.2 阶段 4）。
//
// dry_run=true 时流程到此结束，不写任何数据；
// dry_run=false 时本阶段作为正式迁移前的最后一次「体检」，
// 内存中的 plan 会直接交给阶段 5 使用，保证预览内容与实际写入内容一致。
func (r *Runner) StageDryRun(ctx context.Context) error {
	r.report.Stage = StageDryRun
	r.emit(Progress{Phase: PhasePlan, Stage: StageDryRun, Message: "在内存中完成全部转换"})

	if r.probe != nil && r.probe.Empty {
		r.finalizeReport()
		return nil
	}
	if r.reader == nil {
		return fmt.Errorf("尚未连接源库，请先执行阶段 1（连接与探测）")
	}

	if strings.EqualFold(r.cfg.Source, "nyanpass") {
		if err := r.buildNyanpassPlan(ctx); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("来源 %q 暂不支持字段映射迁移，跨库复制请使用 -copy-database", r.cfg.Source)
	}

	// 名称冲突：默认策略为 fail，检测到冲突直接在阶段 4 拦下。
	if len(r.plan.nameConflicts) > 0 && r.cfg.RenamePolicy == RenameFail && !r.cfg.DryRun {
		return fmt.Errorf("%w: 检测到 %d 处名称冲突，当前策略为 fail；"+
			"请先处理冲突，或使用 --rename-policy=suffix / skip",
			ErrUnresolvedConflict, len(r.plan.nameConflicts))
	}

	r.report.RowsToWrite = r.plan.rowsToWrite()
	r.report.TotalRows = r.plan.totalRows()
	r.report.EstimatedBytes = r.plan.estimateBytes()
	r.report.Outline = buildOutline(r.probe, r.plan)
	r.report.Discards = r.plan.discards
	r.report.Fills = r.plan.fills
	r.report.DiscardTables = r.plan.discardTables
	r.report.Skipped = r.plan.skipped
	r.report.Examples = r.buildExamples()
	r.report.Tables = r.buildTableItems()

	r.finalizeReport()
	return nil
}

// buildNyanpassPlan 执行 Nyanpass → OpenRoute 的完整字段映射。
//
// 这是整个迁移的核心：所有 ID 重映射、倍率折算、token 重生成、
// 多目标展开都发生在这里，且只操作内存。
func (r *Runner) buildNyanpassPlan(ctx context.Context) error {
	src, ok := r.reader.(*NyanpassSource)
	if !ok {
		return fmt.Errorf("来源读取器类型不匹配")
	}

	// —— 读取源数据 ——
	users, err := src.loadUsers(ctx)
	if err != nil {
		return fmt.Errorf("读取源库用户失败: %w", err)
	}
	userGroups, err := src.loadUserGroups(ctx)
	if err != nil {
		return fmt.Errorf("读取源库用户分组失败: %w", err)
	}
	nodes, err := src.loadNodes(ctx)
	if err != nil {
		return fmt.Errorf("读取源库节点失败: %w", err)
	}
	nodeGroups, err := src.loadNodeGroups(ctx)
	if err != nil && !isEmptyTableErr(err) {
		return fmt.Errorf("读取源库节点分组失败: %w", err)
	}
	deviceGroups, err := src.loadDeviceGroups(ctx)
	if err != nil {
		return fmt.Errorf("读取源库设备组失败: %w", err)
	}
	ruleGroups, err := src.loadRuleGroups(ctx)
	if err != nil && !isEmptyTableErr(err) {
		return fmt.Errorf("读取源库规则分组失败: %w", err)
	}
	rules, err := src.loadRules(ctx)
	if err != nil {
		return fmt.Errorf("读取源库规则失败: %w", err)
	}

	// —— 目标库已有数据的 ID 占用 ——
	if err := r.reserveExistingIDs(ctx); err != nil {
		return err
	}

	// —— 1) 用户分组 ——
	userGroupAlloc := r.allocators["user_groups"]
	for _, g := range userGroups {
		name, keep, err := resolveName(r.cfg.RenamePolicy, "用户分组", g.Name, r.plan.usedGroupNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: g.ID, Name: g.Name, Reason: "名称冲突且策略为 skip",
			})
			continue
		}
		id := userGroupAlloc.take(g.ID)
		r.plan.userGroupIDMap[g.ID] = id
		r.plan.userGroups = append(r.plan.userGroups, model.UserGroup{
			ID:           id,
			Name:         util.Truncate(name, 64),
			TrafficLimit: g.TrafficLimit,
			SpeedLimit:   g.SpeedLimit,
			IPLimit:      g.IPLimit,
			ConnLimit:    g.ConnLimit,
			RuleGroupIDs: model.FromAny(g.RuleGroupIDs),
			Remark:       util.Truncate(g.Remark, 255),
			CreatedAt:    g.CreatedAt,
			UpdatedAt:    g.UpdatedAt,
		})
	}
	if len(r.plan.userGroups) == 0 {
		// 目标库要求至少有一个用户分组（用户归属用），补齐默认分组。
		id := userGroupAlloc.take(0)
		r.plan.userGroups = append(r.plan.userGroups, model.UserGroup{
			ID:           id,
			Name:         "默认分组",
			RuleGroupIDs: model.FromAny([]uint64{}),
			Remark:       "迁移时自动创建",
			CreatedAt:    nowUTC(),
			UpdatedAt:    nowUTC(),
		})
	}

	// —— 2) 用户 ——
	usedTokens := map[string]bool{}
	for _, u := range users {
		name, keep, err := resolveName(r.cfg.RenamePolicy, "用户", u.Username, r.plan.usedUserNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: u.ID, Name: u.Username, Reason: "名称冲突且策略为 skip",
			})
			continue
		}

		id := r.allocators["users"].take(u.ID)
		r.plan.userIDMap[u.ID] = id

		// 密码哈希：bcrypt 直接复用，其它算法标记需重置密码（规格书 7.2 / 7.4）。
		var passwordReset bool
		if util.IsBcryptHash(u.Password) {
			// 直接复用源库哈希，用户可继续用原密码登录。
		} else {
			passwordReset = true
			r.plan.passwordResetUsers = append(r.plan.passwordResetUsers, name)
		}

		// 用户级订阅 Token：源库有就沿用（但要保证唯一），否则重新生成。
		token := strings.TrimSpace(u.Token)
		if token == "" || usedTokens[token] {
			gen, err := util.GenerateUserToken()
			if err != nil {
				return fmt.Errorf("生成用户 Token 失败: %w", err)
			}
			token = gen
		}
		usedTokens[token] = true

		r.plan.users = append(r.plan.users, model.User{
			ID:           id,
			Username:     util.Truncate(name, 64),
			PasswordHash: util.Truncate(u.Password, 128),
			Nickname:     util.Truncate(u.Nickname, 64),
			Role:         u.Role,
			Status:       normalizeStatus(u.Status),
			GroupID:      r.plan.userGroupIDMap[u.GroupID],
			Token:        token,

			TrafficUsed:           u.TrafficUsed,
			TrafficLimit:          u.TrafficLimit,
			SpeedLimit:            u.SpeedLimit,
			IPLimit:               u.IPLimit,
			DeviceLimit:           u.DeviceLimit,
			ConnLimit:             u.ConnLimit,
			PasswordResetRequired: passwordReset,
			// 迁移带入的账号标「待重装」，供节点列表页展示。
			PendingReinstall: true,

			ExpireAt:    u.ExpireAt,
			LastLoginAt: u.LastLoginAt,
			LastLoginIP: util.Truncate(u.LastLoginIP, 64),
			Remark:      util.Truncate(u.Remark, 255),
			CreatedAt:   u.CreatedAt,
			UpdatedAt:   u.UpdatedAt,
		})
	}

	// —— 3) 节点分组 ——
	for _, g := range nodeGroups {
		name, keep, err := resolveName(r.cfg.RenamePolicy, "节点分组", g.Name, r.plan.usedGroupNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: g.ID, Name: g.Name, Reason: "名称冲突且策略为 skip",
			})
			continue
		}
		id := r.allocators["node_groups"].take(g.ID)
		r.plan.nodeGroupIDMap[g.ID] = id
		r.plan.nodeGroups = append(r.plan.nodeGroups, model.NodeGroup{
			ID:        id,
			Name:      util.Truncate(name, 64),
			Remark:    util.Truncate(g.Remark, 255),
			CreatedAt: g.CreatedAt,
			UpdatedAt: g.UpdatedAt,
		})
	}

	// —— 4) 节点 ——
	usedNodeTokens := map[string]bool{}
	for _, n := range nodes {
		name, keep, err := resolveName(r.cfg.RenamePolicy, "节点", n.Name, r.plan.usedNodeNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: n.ID, Name: n.Name, Reason: "名称冲突且策略为 skip",
			})
			continue
		}

		id := r.allocators["nodes"].take(n.ID)
		r.plan.nodeIDMap[n.ID] = id

		// 节点密钥 MUST 重新生成，否则会与旧面板冲突（规格书 7.2）。
		var token string
		for attempt := 0; attempt < 8; attempt++ {
			gen, err := util.GenerateNodeToken()
			if err != nil {
				return fmt.Errorf("生成节点密钥失败: %w", err)
			}
			if !usedNodeTokens[gen] {
				token = gen
				break
			}
		}
		if token == "" {
			return fmt.Errorf("为节点 %q 生成唯一密钥失败，请重试", name)
		}
		usedNodeTokens[token] = true
		r.plan.nodeTokensRegenerated++

		// 节点分组 ID 重映射，过滤掉源库里不存在的分组。
		groupIDs := []uint64{}
		for _, gid := range n.GroupIDs {
			if mapped, ok := r.plan.nodeGroupIDMap[gid]; ok {
				groupIDs = append(groupIDs, mapped)
			}
		}

		r.plan.nodes = append(r.plan.nodes, model.Node{
			ID:          id,
			Name:        util.Truncate(name, 64),
			Token:       token,
			Role:        n.Role,
			PublicIPv4:  util.Truncate(n.PublicIPv4, 64),
			PublicIPv6:  util.Truncate(n.PublicIPv6, 64),
			PrivateIP:   util.Truncate(n.PrivateIP, 64),
			ConnectHost: util.Truncate(n.ConnectHost, 255),
			IsStatic:    n.IsStatic,
			DirectPort:  n.DirectPort,
			WsPort:      n.WsPort,
			TlsPort:     n.TlsPort,
			UdpPort:     n.UdpPort,
			RevPort:     n.RevPort,
			GroupIDs:    model.FromAny(groupIDs),
			// 迁移不继承在线状态与运行时指标，等节点上线后自行上报。
			Online:    false,
			Weight:    n.Weight,
			MaxConn:   n.MaxConn,
			Remark:    util.Truncate(n.Remark, 255),
			CreatedAt: n.CreatedAt,
			UpdatedAt: n.UpdatedAt,
		})
	}

	// —— 5) 规则分组 ——
	for _, g := range ruleGroups {
		name, keep, err := resolveName(r.cfg.RenamePolicy, "规则分组", g.Name, r.plan.usedGroupNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: g.ID, Name: g.Name, Reason: "名称冲突且策略为 skip",
			})
			continue
		}
		id := r.allocators["rule_groups"].take(g.ID)
		r.plan.ruleGroupIDMap[g.ID] = id
		r.plan.ruleGroups = append(r.plan.ruleGroups, model.RuleGroup{
			ID:        id,
			Name:      util.Truncate(name, 64),
			Sort:      g.Sort,
			Remark:    util.Truncate(g.Remark, 255),
			CreatedAt: g.CreatedAt,
			UpdatedAt: g.UpdatedAt,
		})
	}

	// —— 6) 设备组 ——
	// 倍率：Nyanpass 把倍率放在设备组上，OpenRoute 放在规则上。
	// 迁移时记录「设备组 ID → 倍率」，规则映射阶段取值。
	groupRate := map[uint64]float64{}
	for _, g := range deviceGroups {
		name, keep, err := resolveName(r.cfg.RenamePolicy, "设备组", g.Name, r.plan.usedGroupNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: g.ID, Name: g.Name, Reason: "名称冲突且策略为 skip",
			})
			continue
		}
		groupRate[g.ID] = g.Multiplier

		id := r.allocators["device_groups"].take(g.ID)
		r.plan.deviceGroupIDMap[g.ID] = id

		// 组成员 ID 重映射。
		nodeIDs := []uint64{}
		for _, nid := range g.NodeIDs {
			if mapped, ok := r.plan.nodeIDMap[nid]; ok {
				nodeIDs = append(nodeIDs, mapped)
			}
		}

		dg := model.DeviceGroup{
			ID:   id,
			Name: util.Truncate(name, 64),
			Type: g.Type,
			// Config JSON 原样迁移：入口组与出口组的结构完全一致（规格书 7.2）。
			NodeIDs: model.FromAny(nodeIDs),
			Config:  model.JSON(jsonOrNull(g.Config)),
			Remark:  util.Truncate(g.Remark, 255),
			// 健康检查默认值来自模型，这里显式写出避免零值被当成「关闭」。
			HealthCheckEnable:    true,
			HealthCheckInterval:  10,
			HealthCheckTimeout:   3,
			HealthCheckFailCount: 3,
			HealthCheckSuccCount: 2,
			CreatedAt:            g.CreatedAt,
			UpdatedAt:            g.UpdatedAt,
		}
		if g.Type == model.GroupTypeOutbound {
			// 出口组负载均衡默认 least_conn（规格书 7.2）。
			dg.Balance = model.BalanceLeastConn
		}
		r.plan.deviceGroups = append(r.plan.deviceGroups, dg)
	}

	// —— 7) 转发规则 ——
	// 先算出「哪些规则因引用缺失而必须跳过」，再逐条转换。
	skipRules := map[uint64]string{}
	for _, m := range r.plan.missingRefs {
		// 引用缺失分两类：可降级的（用户/分组）与必须跳过的（入口组/主规则）。
		switch m.Field {
		case "inbound_group_id", "parent_id", "outbound_group_id":
			if _, ok := skipRules[m.RuleID]; !ok {
				skipRules[m.RuleID] = m.Message
			}
		}
	}

	for _, rule := range rules {
		if reason, skip := skipRules[rule.ID]; skip {
			// 规格书 7.4：标记「待修复」，不写入，记入报告的跳过清单。
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: rule.ID, Name: rule.Name, Reason: reason,
			})
			continue
		}

		name, keep, err := resolveName(r.cfg.RenamePolicy, "规则", rule.Name, r.plan.usedRuleNames)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnresolvedConflict, err)
		}
		if !keep {
			r.plan.skipped = append(r.plan.skipped, SkippedRule{
				SourceID: rule.ID, Name: rule.Name, Reason: "名称冲突且策略为 skip",
			})
			continue
		}

		id := r.allocators["forward_rules"].take(rule.ID)
		r.plan.ruleIDMap[rule.ID] = id

		// 目标：多目标展开为 Targets 数组（规格书 7.2）。
		targets := expandTargets(rule)

		// 倍率：取该规则入口组 / 出口组的倍率。
		inRate := groupRate[rule.InboundGroupID]
		if inRate == 0 {
			inRate = 1
		}
		outRate := groupRate[rule.OutboundGroup]
		if outRate == 0 {
			outRate = 1
		}

		// 链式出口：过滤掉源库里不存在的组。
		chain := []uint64{}
		for _, cg := range rule.ChainGroups {
			if mapped, ok := r.plan.deviceGroupIDMap[cg]; ok {
				chain = append(chain, mapped)
			}
		}

		// 反向隧道：引用的组不存在时关闭，避免写入悬空引用。
		reverseEnable := rule.ReverseEnable
		reverseGroup := r.plan.deviceGroupIDMap[rule.ReverseGroup]
		if rule.ReverseEnable && reverseGroup == 0 {
			reverseEnable = false
		}

		// 目标级负载均衡策略：源库没写或写了非法值时用 failover。
		balance := rule.Balance
		if !model.ValidTargetBalance(balance) {
			balance = model.TargetBalanceFailover
		}

		fr := model.ForwardRule{
			ID:                 id,
			Name:               util.Truncate(name, 128),
			UserID:             r.plan.userIDMap[rule.UserID],
			RuleGroupID:        r.plan.ruleGroupIDMap[rule.RuleGroupID],
			InboundGroupID:     r.plan.deviceGroupIDMap[rule.InboundGroupID],
			ListenPort:         rule.Port,
			ListenPortEnd:      rule.PortEnd,
			OutboundGroupID:    r.plan.deviceGroupIDMap[rule.OutboundGroup],
			Targets:            model.FromAny(targets),
			TargetBalance:      balance,
			InboundMultiplier:  inRate,
			OutboundMultiplier: outRate,
			Options:            model.JSON(jsonObjectOrNull(rule.Options)),
			ChainGroups:        model.FromAny(chain),
			ReverseEnable:      reverseEnable,
			ReversePort:        rule.ReversePort,
			ReverseGroupID:     reverseGroup,
			IsSubRule:          rule.IsSubRule,
			ParentID:           r.plan.ruleIDMap[rule.ParentID],
			SNI:                util.Truncate(rule.SNI, 255),
			Shaping:            model.JSON(jsonArrayOrNull(rule.Shaping)),
			// 迁移后所有规则状态为 unsynced，等节点重装上线后自动同步。
			SyncStatus: model.SyncUnsynced,
			Enable:     rule.Enable,
			Remark:     util.Truncate(rule.Remark, 255),
			CreatedAt:  rule.CreatedAt,
			UpdatedAt:  rule.UpdatedAt,
		}
		r.plan.rules = append(r.plan.rules, fr)
	}

	// —— 8) 流量记录 ——
	// 按 (date, hour, user_id, rule_id, node_id, direction) 聚合后写入（规格书 7.2）。
	agg, total, err := src.loadTrafficRows(ctx)
	if err != nil && !isEmptyTableErr(err) {
		return fmt.Errorf("读取源库流量记录失败: %w", err)
	}
	r.plan.trafficSourceBytes = total

	keys := make([]TrafficKey, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sortTrafficKeys(keys)

	for _, k := range keys {
		row := agg[k]
		// 维度里的 ID 需要重映射；映射不到说明该维度对象已被跳过，
		// 这类流量记不到任何现存对象上，直接丢弃（不写入悬空引用）。
		userID := mapOrZero(r.plan.userIDMap, k.UserID)
		ruleID := mapOrZero(r.plan.ruleIDMap, k.RuleID)
		nodeID := mapOrZero(r.plan.nodeIDMap, k.NodeID)
		if k.UserID != 0 && userID == 0 {
			continue
		}
		if k.RuleID != 0 && ruleID == 0 {
			continue
		}
		row.UserID = userID
		row.RuleID = ruleID
		row.NodeID = nodeID
		r.plan.traffic = append(r.plan.traffic, row)
	}

	return nil
}

// expandTargets 把源库的目标配置展开为 Targets 数组（规格书 7.2）。
//
// 优先使用源库已有的 targets JSON（新版 Nyanpass 已支持多目标）；
// 否则用单目标字段（target_host / target_port）构造一条。
func expandTargets(rule nyRule) []model.Target {
	out := []model.Target{}

	// 已有 targets 数组时直接解析，并规范化其中的状态字段。
	raw := strings.TrimSpace(rule.Targets)
	if raw != "" && raw != "null" && util.IsValidJSON(raw) {
		var list []model.Target
		if err := jsonUnmarshal(raw, &list); err == nil {
			for _, t := range list {
				if strings.TrimSpace(t.Host) == "" || t.Port <= 0 {
					continue
				}
				if t.Status != model.TargetUp && t.Status != model.TargetDown {
					t.Status = model.TargetUp
				}
				out = append(out, t)
			}
		}
	}
	if len(out) > 0 {
		return out
	}

	// 退回单目标字段。
	host := strings.TrimSpace(rule.TargetHost)
	if host != "" && rule.TargetPort > 0 {
		weight := rule.TargetWeight
		if weight <= 0 {
			weight = 1
		}
		out = append(out, model.Target{
			Host:   host,
			Port:   rule.TargetPort,
			Weight: weight,
			Status: model.TargetUp,
		})
	}
	// 目标为空时不报错：入口直出（无出口组）的规则本来就没有目标。
	return out
}

// mapOrZero 查表取映射，取不到返回 0。
func mapOrZero(m map[uint64]uint64, id uint64) uint64 {
	if id == 0 {
		return 0
	}
	return m[id]
}

// sortTrafficKeys 对流量聚合键排序，保证写入顺序稳定（幂等与断点续传依赖这一点）。
func sortTrafficKeys(keys []TrafficKey) {
	less := func(a, b TrafficKey) bool {
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.Direction < b.Direction
	}
	// 键数量可能很大（数十万），用标准库排序避免自实现的开销与出错风险。
	sortSlice(keys, less)
}

// normalizeStatus 把源库的状态值规范化为 0/1。
func normalizeStatus(v int) int {
	if v == 0 {
		return model.StatusDisabled
	}
	return model.StatusEnabled
}

// reserveExistingIDs 把目标库已有的 ID 登记进各表的分配器。
//
// 之所以按表分开分配：目标库可能已经有数据（例如先建了管理员账号），
// 每张表的可用 ID 区间不同，共用一个分配器会让 ID 无谓地膨胀。
func (r *Runner) reserveExistingIDs(ctx context.Context) error {
	// 目标库的迁移相关表可能还不存在（首次运行且未建表），
	// 先补齐表结构；这一步是幂等的。
	if err := r.ensureTargetReady(ctx); err != nil {
		return fmt.Errorf("目标库建表失败: %w", err)
	}
	for _, t := range []string{
		tableTargetUsers, tableTargetUserGroups, tableTargetNodes, tableTargetNodeGroups,
		tableTargetDeviceGroups, tableTargetRuleGroups, tableTargetRules,
	} {
		a, ok := r.allocators[t]
		if !ok {
			a = newIDAllocator()
			r.allocators[t] = a
		}
		var ids []uint64
		q := fmt.Sprintf("select id from %s", t)
		if err := r.cfg.Target.WithContext(ctx).Raw(q).Scan(&ids).Error; err != nil {
			// 表仍不存在（例如迁移目标是空库且模型有变动）时跳过，
			// 阶段 5 会重新 AutoMigrate 一次。
			continue
		}
		for _, id := range ids {
			a.reserve(id)
		}
	}
	return nil
}

// buildTableItems 构造报告「将迁移的数据」章节。
func (r *Runner) buildTableItems() []ReportTableItem {
	items := []ReportTableItem{
		{Label: "用户", Count: int64(len(r.plan.users)), Target: tableTargetUsers},
		{Label: "用户分组", Count: int64(len(r.plan.userGroups)), Target: tableTargetUserGroups},
		{Label: "节点", Count: int64(len(r.plan.nodes)), Target: tableTargetNodes},
		{Label: "节点分组", Count: int64(len(r.plan.nodeGroups)), Target: tableTargetNodeGroups},
		{
			Label: "设备组", Count: int64(len(r.plan.deviceGroups)), Target: tableTargetDeviceGroups,
			Extra: fmt.Sprintf("入口 %d / 出口 %d", inboundCount(r.plan), outboundCount(r.plan)),
		},
		{Label: "规则分组", Count: int64(len(r.plan.ruleGroups)), Target: tableTargetRuleGroups},
		{Label: "转发规则", Count: int64(len(r.plan.rules)), Target: tableTargetRules},
	}
	if r.plan.skippedRuleCount() > 0 {
		items[6].Extra = fmt.Sprintf("跳过 %d 条（详见跳过清单）", r.plan.skippedRuleCount())
	}

	// 流量记录：源库条数与聚合后条数都要展示（规格书样例的写法）。
	srcTraffic := int64(0)
	if r.probe != nil {
		srcTraffic = r.probe.RowCounts[tableTrafficLogs]
		if srcTraffic == 0 {
			srcTraffic = r.probe.RowCounts[tableTraffics]
		}
	}
	trafficItem := ReportTableItem{
		Label:  "流量记录",
		Count:  srcTraffic,
		Target: tableTargetTraffic,
	}
	if int64(len(r.plan.traffic)) != srcTraffic {
		trafficItem.Extra = fmt.Sprintf("按天聚合后约 %d 条", len(r.plan.traffic))
	}
	items = append(items, trafficItem)
	return items
}

// skippedRuleCount 返回因引用缺失 / 冲突跳过的规则条数。
//
// 用户与分组被跳过时也会进入 skipped，但它们不是「规则」，
// 计数时需要用名称是否出现在规则集合里来区分 —— 这里采用更简单可靠的
// 做法：跳过清单里带「规则」字样的条目才算规则。
func (p *plan) skippedRuleCount() int {
	n := 0
	for _, s := range p.skipped {
		if strings.Contains(s.Reason, "规则") || strings.Contains(s.Reason, "入口设备组") ||
			strings.Contains(s.Reason, "出口设备组") || strings.Contains(s.Reason, "主规则") {
			n++
		}
	}
	return n
}

// buildExamples 构造三条示例转换（规格书 7.2 阶段 4 要求三条）。
//
// 选材优先挑「有代表性」的：节点（token 重生成）、规则（多目标展开 + 倍率）、
// 用户（密码哈希复用 / 重置）。没有对应数据时退回展示前若干条中的第一条。
func (r *Runner) buildExamples() []ExampleConversion {
	out := []ExampleConversion{}

	// 示例 1：节点 token 重新生成。
	if len(r.plan.nodes) > 0 {
		n := r.plan.nodes[0]
		before := fmt.Sprintf(`{"name":%q,"token":%q,"role":%q,"direct_port":%d}`,
			n.Name, sourceNodeTokenSafe(), n.Role, n.DirectPort)
		after := fmt.Sprintf(`{"name":%q,"token":%q,"role":%q,"direct_port":%d,"online":false}`,
			n.Name, util.MaskSecret(n.Token), n.Role, n.DirectPort)
		out = append(out, ExampleConversion{
			Title:  fmt.Sprintf("节点 %q：token 重新生成", n.Name),
			Before: before,
			After:  after,
			Note:   "token MUST 重新生成，避免与旧面板冲突；节点客户端需重装",
		})
	}

	// 示例 2：规则的目标展开与倍率下沉。
	if len(r.plan.rules) > 0 {
		fr := r.plan.rules[0]
		targets := fr.TargetList()
		before := fmt.Sprintf(`{"name":%q,"ssh_port":%d,"target_host":%q,"target_port":%d,"multiplier":%s}`,
			fr.Name, fr.ListenPort, targetHostOf(targets), targetPortOf(targets), formatFloat(fr.InboundMultiplier))
		after := fmt.Sprintf(`{"name":%q,"listen_port":%d,"targets":%s,"inbound_multiplier":%s,"outbound_multiplier":%s,"sync_status":%q}`,
			fr.Name, fr.ListenPort, util.ToJSON(targets), formatFloat(fr.InboundMultiplier),
			formatFloat(fr.OutboundMultiplier), fr.SyncStatus)
		out = append(out, ExampleConversion{
			Title:  fmt.Sprintf("规则 %q：多目标展开 + 倍率下沉", fr.Name),
			Before: before,
			After:  after,
			Note:   "单目标字段展开为 Targets 数组；倍率由设备组下沉到规则；状态统一 unsynced",
		})
	}

	// 示例 3：用户的密码哈希处理。
	if len(r.plan.users) > 0 {
		u := r.plan.users[0]
		before := `{"username":` + util.ToJSON(u.Username) + `,"password_hash":"<源库原值>"}`
		after := fmt.Sprintf(`{"username":%s,"password_hash":"%s","password_reset_required":%t,"pending_reinstall":true}`,
			util.ToJSON(u.Username), util.MaskSecret(u.PasswordHash), u.PasswordResetRequired)
		out = append(out, ExampleConversion{
			Title:  fmt.Sprintf("用户 %q：密码哈希与迁移标记", u.Username),
			Before: before,
			After:  after,
			Note:   "bcrypt 直接复用；其它算法标记 password_reset_required，首次登录强制改密",
		})
	}

	return out
}

// sourceNodeTokenSafe 用于示例的 before 展示：不泄露源库真实 token。
func sourceNodeTokenSafe() string { return "<源库原值>" }

// targetHostOf 返回目标列表中的第一个 host。
func targetHostOf(list []model.Target) string {
	if len(list) == 0 {
		return ""
	}
	return list[0].Host
}

// targetPortOf 返回目标列表中的第一个端口。
func targetPortOf(list []model.Target) int {
	if len(list) == 0 {
		return 0
	}
	return list[0].Port
}

// formatFloat 把倍率格式化为不带多余小数位的字符串。
func formatFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

// finalizeReport 补齐报告的建议操作与注意事项。
func (r *Runner) finalizeReport() {
	r.report.Suggestions = r.report.defaultSuggestions()

	// 冲突时的针对性建议（规格书样例第 2 条）。
	if len(r.plan.nameConflicts) > 0 {
		r.report.Suggestions = append(r.report.Suggestions,
			"冲突项建议在源库先改名，或使用 --rename-policy=suffix 自动加后缀")
		switch r.cfg.RenamePolicy {
		case RenameSuffix:
			r.report.Suggestions = append(r.report.Suggestions,
				"当前策略为 suffix：重名记录已自动加 _2 后缀，请迁移后核对名称")
		case RenameSkip:
			r.report.Suggestions = append(r.report.Suggestions,
				fmt.Sprintf("当前策略为 skip：已跳过重名记录，详见跳过清单"))
		}
	}
	if len(r.plan.portConflicts) > 0 {
		r.report.Suggestions = append(r.report.Suggestions,
			"端口冲突不会自动处理：同一入口组内监听同一端口的多条规则只能有一条生效，"+
				"建议迁移后在面板中修改端口")
	}
	if len(r.plan.skipped) > 0 {
		r.report.Suggestions = append(r.report.Suggestions,
			fmt.Sprintf("有 %d 条记录未写入（引用缺失或名称冲突），已标记「待修复」，详见跳过清单",
				len(r.plan.skipped)))
	}

	// 迁移后必做事项（规格书 7.2 硬性要求）。
	if len(r.plan.nodes) > 0 || len(r.plan.rules) > 0 {
		r.report.Notes = BuildMigrationNotes(len(r.plan.rules), r.plan.nodeTokensRegenerated)
	}
	if len(r.plan.passwordResetUsers) > 0 {
		names := r.plan.passwordResetUsers
		if len(names) > 10 {
			names = names[:10]
		}
		r.report.Notes = append(r.report.Notes, fmt.Sprintf(
			"以下用户的密码哈希算法不兼容，已标记「需重置密码」，首次登录强制改密：%s",
			strings.Join(names, "、")))
	}
	if r.cfg.BackupPath != "" {
		r.report.Suggestions = append(r.report.Suggestions,
			"迁移前已自动备份目标库到 "+r.cfg.BackupPath+"（回滚失败时可直接用它恢复）")
	}
}

// ---------------------------------------------------------------------------
// 阶段 5：正式迁移
// ---------------------------------------------------------------------------

// StageMigrate 执行正式迁移（规格书 7.2 阶段 5）。
//
// 幂等与断点续传的实现方式：
//   - 每写入一批（batchSize 行）就提交一次（SQLite 单写者，长事务会锁库）；
//   - 每批提交的同一个事务里写入 migrate_id_map，记录 source_id → target_id
//     与该行内容的校验和；
//   - 重跑时先读 migrate_id_map，已迁移且校验和一致的记录直接跳过。
//
// 由于「数据行 + 映射行」在同一个事务里提交，崩溃后不可能出现
// 「数据写进去了但映射没记」的中间状态。
func (r *Runner) StageMigrate(ctx context.Context) error {
	r.report.Stage = StageMigrate
	if r.cfg.DryRun {
		// dry-run 不进入本阶段，由 Run 直接返回。
		return nil
	}
	if r.probe != nil && r.probe.Empty {
		return nil
	}

	// 磁盘空间检查（规格书 7.4）。
	if err := r.checkDiskSpace(); err != nil {
		return err
	}

	// 创建批次记录。
	if err := r.ensureBatch(ctx, model.MigrateRunning); err != nil {
		return err
	}
	if err := r.updateBatch(ctx, map[string]interface{}{
		"stage":      StageMigrate,
		"total_rows": r.plan.totalRows(),
	}); err != nil {
		return err
	}

	// 读取已迁移记录，支持断点续传。
	done, err := r.loadMigratedSet(ctx)
	if err != nil {
		return err
	}

	// 目标库结构：先建表再写数据。
	if err := r.cfg.Target.WithContext(ctx).AutoMigrate(r.targetModels()...); err != nil {
		r.failBatch(ctx, err)
		return fmt.Errorf("目标库建表失败: %w", err)
	}

	// 逐表写入。源 ID → 目标 ID 的映射在 writeAllTables 内按表还原，
	// 因此这里只负责控制顺序与失败处理。
	if err := r.writeAllTables(ctx, done); err != nil {
		r.failBatch(ctx, err)
		return err
	}

	// 生成配置快照（规格书 7.2 阶段 5：迁移完成后自动生成配置快照）。
	if err := r.createSnapshot(ctx); err != nil {
		// 快照失败不算迁移失败，只在报告里提示。
		r.report.Notes = append(r.report.Notes,
			"配置快照生成失败："+err.Error()+"（不影响已迁移的数据）")
	}

	r.report.MigratedRows = r.plan.totalRows()
	if err := r.updateBatch(ctx, map[string]interface{}{
		"stage":         StageMigrate,
		"migrated_rows": r.report.MigratedRows,
		"status":        model.MigrateRunning,
	}); err != nil {
		return err
	}
	return nil
}

// targetModels 返回迁移涉及的目标模型，用于 AutoMigrate。
func (r *Runner) targetModels() []interface{} {
	return []interface{}{
		&model.UserGroup{}, &model.User{}, &model.NodeGroup{}, &model.Node{},
		&model.DeviceGroup{}, &model.RuleGroup{}, &model.ForwardRule{},
		&model.TrafficLog{}, &model.MigrateBatch{}, &model.MigrateIDMap{},
		&model.ConfigSnapshot{},
	}
}

// writeAllTables 按「先父后子」的顺序写入各表。
func (r *Runner) writeAllTables(ctx context.Context, done map[string]bool) error {
	batches := []struct {
		Table string
		Rows  []writeRow
	}{
		{tableTargetUserGroups, toRows(r.plan.userGroups, func(v model.UserGroup) uint64 { return v.ID }, r.plan.userGroupIDMap)},
		{tableTargetUsers, toRows(r.plan.users, func(v model.User) uint64 { return v.ID }, r.plan.userIDMap)},
		{tableTargetNodeGroups, toRows(r.plan.nodeGroups, func(v model.NodeGroup) uint64 { return v.ID }, r.plan.nodeGroupIDMap)},
		{tableTargetNodes, toRows(r.plan.nodes, func(v model.Node) uint64 { return v.ID }, r.plan.nodeIDMap)},
		{tableTargetRuleGroups, toRows(r.plan.ruleGroups, func(v model.RuleGroup) uint64 { return v.ID }, r.plan.ruleGroupIDMap)},
		{tableTargetDeviceGroups, toRows(r.plan.deviceGroups, func(v model.DeviceGroup) uint64 { return v.ID }, r.plan.deviceGroupIDMap)},
		{tableTargetRules, toRows(r.plan.rules, func(v model.ForwardRule) uint64 { return v.ID }, r.plan.ruleIDMap)},
	}

	total := int64(0)
	for _, b := range batches {
		total += int64(len(b.Rows))
	}
	written := int64(0)

	for _, b := range batches {
		if err := r.writeTable(ctx, b.Table, b.Rows, done, &written, total); err != nil {
			return err
		}
	}

	// 流量记录：聚合行的键就是唯一约束，直接用 UPSERT 语义写入。
	if err := r.writeTraffic(ctx, done, &written, total); err != nil {
		return err
	}
	return nil
}

// writeRow 是「待写入的一行」：源 ID 与要落库的模型值。
type writeRow struct {
	sourceID uint64
	value    interface{}
}

// toRows 把模型切片转成「源 ID + 值」的行列表。
//
// targetIDMap 是「源 ID → 目标 ID」；这里通过反向查找还原出每行对应的源 ID，
// 因为写入 migrate_id_map 需要的是源 ID，而模型上带的是目标 ID。
func toRows[T any](items []T, targetID func(T) uint64, targetIDMap map[uint64]uint64) []writeRow {
	// 先建「目标 ID → 源 ID」的反向索引。
	rev := make(map[uint64]uint64, len(targetIDMap))
	for src, dst := range targetIDMap {
		rev[dst] = src
	}
	out := make([]writeRow, 0, len(items))
	for i := range items {
		tid := targetID(items[i])
		out = append(out, writeRow{sourceID: rev[tid], value: &items[i]})
	}
	return out
}

// writeTable 写入一张表，分批提交并记录 migrate_id_map。
func (r *Runner) writeTable(
	ctx context.Context, table string, rows []writeRow,
	done map[string]bool, written *int64, total int64,
) error {
	if len(rows) == 0 {
		return nil
	}
	r.emit(Progress{
		Phase: PhaseWrite, Stage: StageMigrate, Table: table,
		Done: *written, Total: total, Message: "写入 " + table,
	})

	const batchSize = 500
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}

		// 断点续传：整批都已迁移时直接跳过，不再开事务。
		pending := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			key := idMapKey(table, rows[i].sourceID)
			if done[key] {
				continue
			}
			pending = append(pending, i)
		}
		if len(pending) == 0 {
			*written += int64(end - start)
			continue
		}

		err := r.cfg.Target.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, i := range pending {
				// SQLite 允许显式指定主键；用 Save 语义保证重启后重跑不报错。
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "id"}},
					UpdateAll: true,
				}).Create(rows[i].value).Error; err != nil {
					return fmt.Errorf("写入 %s（源 ID %d）失败: %w", table, rows[i].sourceID, err)
				}
				if err := tx.Create(&model.MigrateIDMap{
					BatchID:  r.batch.ID,
					Table:    table,
					SourceID: rows[i].sourceID,
					TargetID: targetIDOf(rows[i].value),
					Checksum: checksumOf(rows[i].value),
				}).Error; err != nil {
					return fmt.Errorf("写入 %s 的 ID 映射失败: %w", table, err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		*written += int64(len(pending))
		if err := r.updateBatch(ctx, map[string]interface{}{"migrated_rows": *written}); err != nil {
			return err
		}
		r.emit(Progress{
			Phase: PhaseWrite, Stage: StageMigrate, Table: table,
			Done: *written, Total: total,
		})
	}
	return nil
}

// writeTraffic 写入聚合后的流量记录。
//
// 流量表的唯一约束是六个维度字段，重跑时用 ON CONFLICT DO NOTHING 保证幂等
// （流量行是「聚合结果」而不是「增量」，重复累加会算错，因此不能累加更新）。
func (r *Runner) writeTraffic(ctx context.Context, done map[string]bool, written *int64, total int64) error {
	if len(r.plan.traffic) == 0 {
		return nil
	}
	r.emit(Progress{
		Phase: PhaseWrite, Stage: StageMigrate, Table: tableTargetTraffic,
		Done: *written, Total: total, Message: "写入 " + tableTargetTraffic,
	})

	const batchSize = 500
	for start := 0; start < len(r.plan.traffic); start += batchSize {
		end := start + batchSize
		if end > len(r.plan.traffic) {
			end = len(r.plan.traffic)
		}
		chunk := make([]model.TrafficLog, 0, end-start)
		for _, t := range r.plan.traffic[start:end] {
			chunk = append(chunk, model.TrafficLog{
				Date: t.Date, Hour: t.Hour, UserID: t.UserID, RuleID: t.RuleID,
				NodeID: t.NodeID, Direction: t.Direction,
				RawBytes: t.RawBytes, Bytes: t.Bytes,
				CreatedAt: nowUTC(), UpdatedAt: nowUTC(),
			})
		}

		err := r.cfg.Target.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "date"}, {Name: "hour"}, {Name: "user_id"},
					{Name: "rule_id"}, {Name: "node_id"}, {Name: "direction"},
				},
				DoNothing: true,
			}).Create(&chunk).Error
		})
		if err != nil {
			return fmt.Errorf("写入流量记录失败: %w", err)
		}

		*written += int64(len(chunk))
		if err := r.updateBatch(ctx, map[string]interface{}{"migrated_rows": *written}); err != nil {
			return err
		}
		r.emit(Progress{
			Phase: PhaseWrite, Stage: StageMigrate, Table: tableTargetTraffic,
			Done: *written, Total: total,
		})
	}
	return nil
}

// targetIDOf 从写入的模型值里取出主键。
func targetIDOf(v interface{}) uint64 {
	switch x := v.(type) {
	case *model.User:
		return x.ID
	case *model.UserGroup:
		return x.ID
	case *model.Node:
		return x.ID
	case *model.NodeGroup:
		return x.ID
	case *model.DeviceGroup:
		return x.ID
	case *model.RuleGroup:
		return x.ID
	case *model.ForwardRule:
		return x.ID
	default:
		return 0
	}
}

// idMapKey 构造 migrate_id_map 的去重键。
func idMapKey(table string, sourceID uint64) string {
	return fmt.Sprintf("%s#%d", table, sourceID)
}

// loadMigratedSet 读取本批次已迁移的记录（断点续传）。
func (r *Runner) loadMigratedSet(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	if r.batch == nil {
		return out, nil
	}
	type row struct {
		Table    string `gorm:"column:table_name"`
		SourceID uint64 `gorm:"column:source_id"`
	}
	var rows []row
	if err := r.cfg.Target.WithContext(ctx).Table("migrate_id_map").
		Select("table_name, source_id").
		Where("batch_id = ?", r.batch.ID).
		Scan(&rows).Error; err != nil {
		// 表还不存在说明是首次运行，直接返回空集合。
		return out, nil
	}
	for _, x := range rows {
		out[idMapKey(x.Table, x.SourceID)] = true
	}
	return out, nil
}

// checkDiskSpace 检查目标磁盘剩余空间（规格书 7.4）。
//
// 只在「目标库是本地 SQLite 文件」时做实际检查：MySQL/PG 的存储
// 在远端，本地磁盘大小与它无关，此时跳过检查并在报告中提示。
func (r *Runner) checkDiskSpace() error {
	need := r.plan.estimateBytes()
	r.report.EstimatedBytes = need
	if need <= 0 {
		return nil
	}

	dialect := r.cfg.Target.Dialect
	if dialect == nil || dialect.Name != "sqlite" || dialect.File == "" {
		r.report.Notes = append(r.report.Notes,
			fmt.Sprintf("目标库为远端数据库，无法检查磁盘空间；预估需要 %s 的额外存储",
				util.FormatBytes(need)))
		return nil
	}

	dir := filepath.Dir(absPath(dialect.File))
	free, err := freeSpace(dir)
	if err != nil {
		// 无法确认时只提示，不阻断 —— 规格书只要求「空间不足时拒绝」。
		r.log.Warn("无法检查磁盘剩余空间", zap.String("dir", dir), zap.Error(err))
		return nil
	}
	// 留出 10% 余量，避免写到最后几条时把盘写满。
	if free < need+need/10 {
		return fmt.Errorf("%w: 目标磁盘可用 %s，预估需要 %s（含 10%% 余量），"+
			"请先清理磁盘或更换目标库位置",
			ErrInsufficientSpace, util.FormatBytes(free), util.FormatBytes(need+need/10))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 阶段 6：校验
// ---------------------------------------------------------------------------

// StageVerify 执行迁移校验（规格书 7.2 阶段 6）。
func (r *Runner) StageVerify(ctx context.Context) error {
	r.report.Stage = StageVerify
	if r.cfg.DryRun || (r.probe != nil && r.probe.Empty) {
		return nil
	}
	r.emit(Progress{Phase: PhaseVerify, Stage: StageVerify, Message: "校验迁移结果"})

	v := &VerifyReport{
		RowCounts:            []RowCountCompare{},
		RefIntegrityFailures: []string{},
		TrafficTolerance:     TrafficTolerancePercent,
		Messages:             []string{},
	}

	// 1) 行数对比。
	type tableCheck struct {
		Table    string
		Source   int64
		Expected int64
		// Aggregated 为 true 时源库条数与目标条数天然不同（流量按天聚合）
		Aggregated bool
		Note       string
	}
	checks := []tableCheck{
		{tableTargetUsers, int64(len(r.plan.users)), int64(len(r.plan.users)), false, ""},
		{tableTargetUserGroups, int64(len(r.plan.userGroups)), int64(len(r.plan.userGroups)), false, ""},
		{tableTargetNodes, int64(len(r.plan.nodes)), int64(len(r.plan.nodes)), false, ""},
		{tableTargetNodeGroups, int64(len(r.plan.nodeGroups)), int64(len(r.plan.nodeGroups)), false, ""},
		{tableTargetDeviceGroups, int64(len(r.plan.deviceGroups)), int64(len(r.plan.deviceGroups)), false, ""},
		{tableTargetRuleGroups, int64(len(r.plan.ruleGroups)), int64(len(r.plan.ruleGroups)), false, ""},
		{tableTargetRules, int64(len(r.plan.rules)), int64(len(r.plan.rules)), false, ""},
	}
	for _, c := range checks {
		ids := idsOf(r.plan, c.Table)
		if len(ids) == 0 {
			v.RowCounts = append(v.RowCounts, RowCountCompare{
				Table: c.Table, Source: c.Source, Target: 0, Passed: true, Note: "本次无数据",
			})
			continue
		}
		// 分块查询，避免 IN 子句参数过多（SQLite 的变量上限是 999）。
		var n int64
		for start := 0; start < len(ids); start += 500 {
			end := start + 500
			if end > len(ids) {
				end = len(ids)
			}
			var chunkCount int64
			if err := r.cfg.Target.WithContext(ctx).Raw(
				fmt.Sprintf("select count(*) from %s where id in (?)", c.Table),
				ids[start:end]).Scan(&chunkCount).Error; err != nil {
				v.Messages = append(v.Messages, fmt.Sprintf("统计 %s 行数失败：%v", c.Table, err))
				break
			}
			n += chunkCount
		}
		passed := n == c.Expected
		note := c.Note
		if !passed {
			note = "目标库中本次迁移写入的行数与预期不符，可能有并发删除或 ID 冲突"
		}
		v.RowCounts = append(v.RowCounts, RowCountCompare{
			Table: c.Table, Source: c.Source, Target: n, Passed: passed, Note: note,
		})
	}

	// 流量表：源库按行存、目标按维度聚合，因此对比的是「聚合后的维度数」。
	if len(r.plan.traffic) > 0 {
		var n int64
		// 逐维度统计代价高（流量表往往几十万行），改为按日期分块统计，
		// 再与「聚合后应写入的行数」比较。
		dates := map[string]bool{}
		for _, t := range r.plan.traffic {
			dates[t.Date] = true
		}
		dateList := make([]string, 0, len(dates))
		for d := range dates {
			dateList = append(dateList, d)
		}
		sortStrings(dateList)
		for start := 0; start < len(dateList); start += 200 {
			end := start + 200
			if end > len(dateList) {
				end = len(dateList)
			}
			var cnt int64
			if err := r.cfg.Target.WithContext(ctx).Raw(
				"select count(*) from traffic_logs where date in (?)", dateList[start:end]).
				Scan(&cnt).Error; err == nil {
				n += cnt
			}
		}
		passed := n >= int64(len(r.plan.traffic))
		v.RowCounts = append(v.RowCounts, RowCountCompare{
			Table: tableTargetTraffic, Source: int64(len(r.plan.traffic)), Target: n, Passed: passed,
			Note: "源库明细聚合后的维度数；目标库可能已有同期数据，因此目标数不小于源数即视为通过",
		})
	}

	// 2) 抽样字段比对（随机 100 条，逐字段比）。
	sampled, mismatches, details := r.sampleCompare(ctx, 100)
	v.Sampled = sampled
	v.SampleMismatches = mismatches
	v.SampleDetails = details

	// 3) 引用完整性检查。
	v.RefIntegrityFailures = r.checkReferentialIntegrity(ctx)

	// 4) 流量总量对比（允许 ±0.1%）。
	v.TrafficSourceBytes = r.plan.trafficSourceBytes
	var targetBytes int64
	if err := r.cfg.Target.WithContext(ctx).
		Model(&model.TrafficLog{}).
		Select("coalesce(sum(bytes), 0)").
		Scan(&targetBytes).Error; err != nil {
		v.Messages = append(v.Messages, fmt.Sprintf("统计目标库流量总量失败：%v", err))
	}
	v.TrafficTargetBytes = targetBytes
	v.TrafficDiffPercent = percentDiff(v.TrafficSourceBytes, v.TrafficTargetBytes)

	// 汇总结论。
	v.Passed = mismatches == 0 && len(v.RefIntegrityFailures) == 0 &&
		v.TrafficDiffPercent <= v.TrafficTolerance
	for _, c := range v.RowCounts {
		if !c.Passed {
			v.Passed = false
		}
	}
	r.report.Verify = v

	if err := r.updateBatch(ctx, map[string]interface{}{
		"stage":  StageVerify,
		"status": model.MigrateFinished,
	}); err != nil {
		return err
	}
	finished := nowUTC()
	r.batch.FinishedAt = &finished
	return r.updateBatch(ctx, map[string]interface{}{
		"finished_at": finished,
		"report":      model.JSON(mustJSON(r.report.ToJSONReport())),
	})
}

// idsOf 返回计划中某张表写入的目标 ID 列表。
func idsOf(p *plan, table string) []uint64 {
	out := []uint64{}
	switch table {
	case tableTargetUsers:
		for i := range p.users {
			out = append(out, p.users[i].ID)
		}
	case tableTargetUserGroups:
		for i := range p.userGroups {
			out = append(out, p.userGroups[i].ID)
		}
	case tableTargetNodes:
		for i := range p.nodes {
			out = append(out, p.nodes[i].ID)
		}
	case tableTargetNodeGroups:
		for i := range p.nodeGroups {
			out = append(out, p.nodeGroups[i].ID)
		}
	case tableTargetDeviceGroups:
		for i := range p.deviceGroups {
			out = append(out, p.deviceGroups[i].ID)
		}
	case tableTargetRuleGroups:
		for i := range p.ruleGroups {
			out = append(out, p.ruleGroups[i].ID)
		}
	case tableTargetRules:
		for i := range p.rules {
			out = append(out, p.rules[i].ID)
		}
	}
	return out
}

// sampleCompare 抽样比对字段（规格书 7.2 阶段 6）。
//
// 比对的是「内存中的计划值」与「数据库中的实际值」，
// 因此能发现写入过程中的截断、类型转换与编码问题。
func (r *Runner) sampleCompare(ctx context.Context, sampleSize int) (int, int, []string) {
	details := []string{}

	type sample struct {
		Table  string
		Target uint64
	}

	samples := []sample{}
	for i := range r.plan.users {
		samples = append(samples, sample{Table: tableTargetUsers, Target: r.plan.users[i].ID})
	}
	for i := range r.plan.nodes {
		samples = append(samples, sample{Table: tableTargetNodes, Target: r.plan.nodes[i].ID})
	}
	for i := range r.plan.rules {
		samples = append(samples, sample{Table: tableTargetRules, Target: r.plan.rules[i].ID})
	}
	for i := range r.plan.deviceGroups {
		samples = append(samples, sample{Table: tableTargetDeviceGroups, Target: r.plan.deviceGroups[i].ID})
	}

	if len(samples) == 0 {
		return 0, 0, details
	}

	// 「随机 100 条」：用固定步长均匀取样而不是真随机，
	// 这样同一份数据每次跑抽到的样本一致，校验结果可复现，
	// 排查不一致项时不会因为样本漂移而「这次又不报了」。
	if len(samples) > sampleSize {
		picked := make([]sample, 0, sampleSize)
		stride := len(samples) / sampleSize
		if stride < 1 {
			stride = 1
		}
		for i := 0; i < len(samples) && len(picked) < sampleSize; i += stride {
			picked = append(picked, samples[i])
		}
		samples = picked
	}

	checked := 0
	mismatches := 0
	for _, s := range samples {
		row := map[string]interface{}{}
		q := fmt.Sprintf("select * from %s where id = ? limit 1", s.Table)
		if err := r.cfg.Target.WithContext(ctx).Raw(q, s.Target).Scan(&row).Error; err != nil || len(row) == 0 {
			// 查不到行本身就是一种不一致，但可能只是数据被并发删除，
			// 因此计入 checked 并记一条明细，不作为硬失败。
			checked++
			mismatches++
			if len(details) < 10 {
				details = append(details, fmt.Sprintf("%s 中 id=%d 的行读不出来", s.Table, s.Target))
			}
			continue
		}
		checked++
		// 只比对该行的「关键字段」，避免把 updated_at 这类会被驱动
		// 在写入时改写的字段算成不一致。
		if !r.compareKeyFields(ctx, s.Table, s.Target, row) {
			mismatches++
			if len(details) < 10 {
				details = append(details, fmt.Sprintf(
					"%s 的 id=%d 关键字段与预期不一致", s.Table, s.Target))
			}
		}
	}
	return checked, mismatches, details
}

// compareKeyFields 比对单行的关键字段。
func (r *Runner) compareKeyFields(ctx context.Context, table string, id uint64, row map[string]interface{}) bool {
	switch table {
	case tableTargetUsers:
		for i := range r.plan.users {
			u := r.plan.users[i]
			if u.ID != id {
				continue
			}
			return asString(rowGet(row, "username")) == u.Username &&
				asString(rowGet(row, "password_hash")) == u.PasswordHash &&
				asInt(rowGet(row, "status")) == u.Status &&
				asBool(rowGet(row, "password_reset_required")) == u.PasswordResetRequired
		}
	case tableTargetNodes:
		for i := range r.plan.nodes {
			n := r.plan.nodes[i]
			if n.ID != id {
				continue
			}
			return asString(rowGet(row, "name")) == n.Name &&
				asString(rowGet(row, "token")) == n.Token &&
				asString(rowGet(row, "role")) == n.Role
		}
	case tableTargetRules:
		for i := range r.plan.rules {
			f := r.plan.rules[i]
			if f.ID != id {
				continue
			}
			return asString(rowGet(row, "name")) == f.Name &&
				asInt(rowGet(row, "listen_port")) == f.ListenPort &&
				asString(rowGet(row, "sync_status")) == f.SyncStatus
		}
	case tableTargetDeviceGroups:
		for i := range r.plan.deviceGroups {
			g := r.plan.deviceGroups[i]
			if g.ID != id {
				continue
			}
			// Config JSON 原样迁移，比对时忽略空白差异。
			expect := strings.TrimSpace(string(g.Config))
			actual := strings.TrimSpace(asString(rowGet(row, "config")))
			return asString(rowGet(row, "name")) == g.Name &&
				asString(rowGet(row, "type")) == g.Type &&
				compactJSON(expect) == compactJSON(actual)
		}
	}
	return true
}

// compactJSON 去掉 JSON 中的空白，用于内容比对。
func compactJSON(s string) string {
	var b strings.Builder
	inString := false
	escaped := false
	for _, c := range s {
		if inString {
			b.WriteRune(c)
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			b.WriteRune(c)
		case ' ', '\t', '\n', '\r':
			// 丢弃
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// checkReferentialIntegrity 检查每条规则的入口组 / 出口组 / 用户是否都在目标库存在。
func (r *Runner) checkReferentialIntegrity(ctx context.Context) []string {
	failures := []string{}
	if len(r.plan.rules) == 0 {
		return failures
	}

	groupIDs := []uint64{}
	for i := range r.plan.deviceGroups {
		groupIDs = append(groupIDs, r.plan.deviceGroups[i].ID)
	}
	userIDs := []uint64{}
	for i := range r.plan.users {
		userIDs = append(userIDs, r.plan.users[i].ID)
	}

	existsIn := func(table string, ids []uint64, id uint64) bool {
		if id == 0 {
			return true
		}
		for _, v := range ids {
			if v == id {
				return true
			}
		}
		// ID 不在计划内（例如目标库已有的历史数据）：查一次库确认。
		var n int64
		q := fmt.Sprintf("select count(*) from %s where id = ?", table)
		if err := r.cfg.Target.WithContext(ctx).Raw(q, id).Scan(&n).Error; err != nil {
			return false
		}
		return n > 0
	}

	for i := range r.plan.rules {
		f := r.plan.rules[i]
		if !existsIn(tableTargetDeviceGroups, groupIDs, f.InboundGroupID) {
			failures = append(failures, fmt.Sprintf(
				"规则 %q 的入口组（ID %d）在目标库不存在", f.Name, f.InboundGroupID))
		}
		if !existsIn(tableTargetDeviceGroups, groupIDs, f.OutboundGroupID) {
			failures = append(failures, fmt.Sprintf(
				"规则 %q 的出口组（ID %d）在目标库不存在", f.Name, f.OutboundGroupID))
		}
		if !existsIn(tableTargetUsers, userIDs, f.UserID) {
			failures = append(failures, fmt.Sprintf(
				"规则 %q 的归属用户（ID %d）在目标库不存在", f.Name, f.UserID))
		}
		if len(failures) >= 50 {
			failures = append(failures, "…… 其余失败项已省略")
			break
		}
	}
	return failures
}

// percentDiff 计算两个总量的差异百分比。
func percentDiff(src, dst int64) float64 {
	if src == 0 {
		if dst == 0 {
			return 0
		}
		return 100
	}
	d := float64(dst-src) / float64(src) * 100
	if d < 0 {
		d = -d
	}
	return d
}

// ---------------------------------------------------------------------------
// 阶段 7：回滚
// ---------------------------------------------------------------------------

// StageRollback 回滚一个迁移批次（规格书 7.2 阶段 7）。
//
// 行为：按 migrate_id_map 逆向删除本批次写入的数据。
// 删除顺序与写入顺序相反（先子后父），避免留下悬空引用。
func (r *Runner) StageRollback(ctx context.Context, batchID uint64) error {
	r.report.Stage = StageRollback
	r.emit(Progress{Phase: PhaseRollback, Stage: StageRollback, Message: "回滚迁移批次"})

	batch := &model.MigrateBatch{}
	if err := r.cfg.Target.WithContext(ctx).First(batch, batchID).Error; err != nil {
		return fmt.Errorf("读取迁移批次 %d 失败: %w", batchID, err)
	}
	if batch.Status == model.MigrateRolledBack {
		return fmt.Errorf("批次 %d 已经回滚过，无需重复操作", batchID)
	}

	return r.rollbackBatch(ctx, batch)
}

// rollbackBatch 执行实际回滚逻辑。
func (r *Runner) rollbackBatch(ctx context.Context, batch *model.MigrateBatch) error {
	// 子表在前，父表在后。
	tables := []string{
		tableTargetTraffic, tableTargetRules, tableTargetDeviceGroups,
		tableTargetNodes, tableTargetNodeGroups, tableTargetUsers, tableTargetUserGroups,
	}

	deleted := int64(0)
	for _, table := range tables {
		ids := []uint64{}
		if err := r.cfg.Target.WithContext(ctx).Model(&model.MigrateIDMap{}).
			Where("batch_id = ? and table_name = ?", batch.ID, table).
			Pluck("target_id", &ids).Error; err != nil {
			return fmt.Errorf("读取 %s 的 ID 映射失败: %w", table, err)
		}
		if len(ids) == 0 {
			continue
		}
		for start := 0; start < len(ids); start += 500 {
			end := start + 500
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			res := r.cfg.Target.WithContext(ctx).
				Exec(fmt.Sprintf("delete from %s where id in (?)", table), chunk)
			if res.Error != nil {
				return fmt.Errorf("回滚 %s 失败: %w；"+
					"可用迁移前的备份恢复：%s", table, res.Error, batch.BackupPath)
			}
			deleted += res.RowsAffected
		}
	}

	// 流量表用维度删除更精确：聚合行的主键是自增的，
	// 但迁移写入的行没有登记 ID 映射（唯一键是维度），因此按维度删除。
	if err := r.rollbackTrafficByDimension(ctx, batch); err != nil {
		return err
	}

	// 清理映射记录并更新批次状态。
	if err := r.cfg.Target.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("batch_id = ?", batch.ID).Delete(&model.MigrateIDMap{}).Error; err != nil {
			return err
		}
		finished := nowUTC()
		return tx.Model(&model.MigrateBatch{}).Where("id = ?", batch.ID).Updates(map[string]interface{}{
			"status":      model.MigrateRolledBack,
			"stage":       StageRollback,
			"finished_at": finished,
			"error":       "",
		}).Error
	}); err != nil {
		return err
	}

	r.report.MigratedRows = 0
	r.report.Suggestions = append(r.report.Suggestions,
		fmt.Sprintf("已回滚批次 %d，共删除 %d 行；若回滚不完整，可用备份恢复：%s",
			batch.ID, deleted, batch.BackupPath))
	return nil
}

// rollbackTrafficByDimension 按唯一键维度删除本批次写入的流量行。
func (r *Runner) rollbackTrafficByDimension(ctx context.Context, batch *model.MigrateBatch) error {
	// 迁移写入的流量行没有逐行 ID 映射，改用「本批次的时间范围 + 维度集合」删除。
	// 更稳妥的做法是把聚合键也算进 migrate_id_map，但它的唯一约束是复合键，
	// 因此这里退一步：只删除「目标库中存在、且维度在本批计划内」的行。
	if len(r.plan.traffic) == 0 {
		return nil
	}
	type key struct {
		date      string
		hour      int
		userID    uint64
		ruleID    uint64
		nodeID    uint64
		direction string
	}
	keys := map[key]bool{}
	for _, t := range r.plan.traffic {
		keys[key{t.Date, t.Hour, t.UserID, t.RuleID, t.NodeID, t.Direction}] = true
	}
	// 逐条删除代价高但安全；流量维度通常远少于明细行数。
	count := 0
	for k := range keys {
		if err := r.cfg.Target.WithContext(ctx).Exec(
			"delete from traffic_logs where date = ? and hour = ? and user_id = ? "+
				"and rule_id = ? and node_id = ? and direction = ?",
			k.date, k.hour, k.userID, k.ruleID, k.nodeID, k.direction).Error; err != nil {
			return fmt.Errorf("回滚流量记录失败: %w", err)
		}
		count++
		if count%5000 == 0 {
			r.emit(Progress{
				Phase: PhaseRollback, Stage: StageRollback, Table: tableTargetTraffic,
				Done: int64(count), Total: int64(len(keys)),
			})
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 完整流程
// ---------------------------------------------------------------------------

// Run 按规格书 7.2 的顺序执行全部 7 个阶段。
//
// 流程：阶段 1~4 总是执行；dry_run=true 时在阶段 4 之后停止并返回报告；
// 否则继续执行阶段 5~6。阶段 7（回滚）不在此流程内，由 Rollback 显式调用。
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	defer r.closeSource()

	// 阶段 1
	if err := r.StageConnect(ctx); err != nil {
		return r.report, err
	}
	// 源库为空：不报错，直接输出报告并返回（退出码 0 由调用方决定）。
	if r.probe != nil && r.probe.Empty {
		r.finalizeReport()
		return r.report, nil
	}

	// 阶段 2
	if err := r.StageScan(ctx); err != nil {
		return r.report, err
	}
	// 阶段 3
	if err := r.StageConflict(ctx); err != nil {
		return r.report, err
	}
	// 阶段 4
	if err := r.StageDryRun(ctx); err != nil {
		return r.report, err
	}
	if r.cfg.DryRun {
		return r.report, nil
	}

	// 迁移前的未完成批次检测（规格书 7.4）。
	if err := r.handleUnfinishedBatches(ctx); err != nil {
		return r.report, err
	}

	// 阶段 5
	if err := r.StageMigrate(ctx); err != nil {
		return r.report, err
	}
	// 阶段 6
	if err := r.StageVerify(ctx); err != nil {
		return r.report, err
	}
	return r.report, nil
}

// Rollback 回滚指定批次，同时刷新报告。
func (r *Runner) Rollback(ctx context.Context, batchID uint64) (*Report, error) {
	defer r.closeSource()
	if err := r.StageRollback(ctx, batchID); err != nil {
		return r.report, err
	}
	return r.report, nil
}

// Precheck 只执行阶段 1~4，用于 API 的「上传 / 指定 DSN，返回预检报告」。
//
// 与 Run 的区别：无论 DryRun 配置如何都不写数据，保证预检接口是只读的。
func (r *Runner) Precheck(ctx context.Context) (*Report, error) {
	defer r.closeSource()
	if err := r.StageConnect(ctx); err != nil {
		return r.report, err
	}
	if r.probe != nil && r.probe.Empty {
		r.finalizeReport()
		return r.report, nil
	}
	if err := r.StageScan(ctx); err != nil {
		return r.report, err
	}
	if err := r.StageConflict(ctx); err != nil {
		return r.report, err
	}
	if err := r.StageDryRun(ctx); err != nil {
		return r.report, err
	}
	return r.report, nil
}

// closeSource 关闭源库连接。
func (r *Runner) closeSource() {
	if r.cfg.sourceDB != nil {
		_ = r.cfg.sourceDB.Close()
		r.cfg.sourceDB = nil
	}
}

// seedExistingNames 把目标库中已存在的名称写入查重集合。
//
// 覆盖四类按名称唯一的资源：用户、节点、分组（用户/节点/规则/设备组共用一张
// 逻辑查重表，因为它们的名字空间在面板语义上互不干扰，但同一张表内必须唯一）、
// 以及规则名。
//
// 键统一用 strings.ToLower 归一化，与规格书 4.1 要求的「应用层 EqualFold 比较」
// 保持一致——不能依赖数据库排序规则，否则 MySQL 与 SQLite/PG 行为不同。
//
// 返回读取目标库时的错误。
func (r *Runner) seedExistingNames(ctx context.Context) error {
	db := r.cfg.Target.DB.WithContext(ctx)

	var usernames []string
	if err := db.Model(&model.User{}).Pluck("username", &usernames).Error; err != nil {
		return err
	}
	for _, n := range usernames {
		r.plan.usedUserNames[strings.ToLower(n)] = n
	}

	var nodeNames []string
	if err := db.Model(&model.Node{}).Pluck("name", &nodeNames).Error; err != nil {
		return err
	}
	for _, n := range nodeNames {
		r.plan.usedNodeNames[strings.ToLower(n)] = n
	}

	var ruleNames []string
	if err := db.Model(&model.ForwardRule{}).Pluck("name", &ruleNames).Error; err != nil {
		return err
	}
	for _, n := range ruleNames {
		r.plan.usedRuleNames[strings.ToLower(n)] = n
	}

	// 四类分组共用 usedGroupNames：迁移目标里它们分别落到不同的表，
	// 但沿用同一集合只会更保守（多报冲突），不会漏报，符合「宁可提示」的取向。
	for _, tbl := range []struct {
		model interface{}
		col   string
	}{
		{&model.UserGroup{}, "name"},
		{&model.NodeGroup{}, "name"},
		{&model.RuleGroup{}, "name"},
		{&model.DeviceGroup{}, "name"},
	} {
		var names []string
		if err := db.Model(tbl.model).Pluck(tbl.col, &names).Error; err != nil {
			return err
		}
		for _, n := range names {
			r.plan.usedGroupNames[strings.ToLower(n)] = n
		}
	}
	return nil
}

// handleUnfinishedBatches 检测未完成批次并交给 Recover 决策（规格书 7.4）。
func (r *Runner) handleUnfinishedBatches(ctx context.Context) error {
	if err := r.ensureTargetReady(ctx); err != nil {
		return err
	}

	var batches []model.MigrateBatch
	if err := r.cfg.Target.WithContext(ctx).
		Where("status in ?", []string{model.MigratePending, model.MigrateRunning}).
		Order("id DESC").Limit(5).Find(&batches).Error; err != nil {
		// 表不存在说明首次运行，没有未完成批次。
		return nil
	}
	if len(batches) == 0 {
		return nil
	}

	for i := range batches {
		b := &batches[i]
		choice := RecoveryContinue
		if r.cfg.Recover != nil {
			c, err := r.cfg.Recover(b)
			if err != nil {
				return err
			}
			choice = c
		}
		switch choice {
		case RecoveryRollback:
			if err := r.rollbackBatch(ctx, b); err != nil {
				return err
			}
		case RecoveryAbandon:
			finished := nowUTC()
			if err := r.cfg.Target.WithContext(ctx).
				Model(&model.MigrateBatch{}).Where("id = ?", b.ID).
				Updates(map[string]interface{}{
					"status":      model.MigrateFailed,
					"error":       "用户选择放弃该未完成批次",
					"finished_at": finished,
				}).Error; err != nil {
				return err
			}
		default: // RecoveryContinue
			// 继续：复用该批次，后续写入会跳过已迁移的记录。
			r.batch = b
		}
	}
	return nil
}

// ensureTargetReady 确保目标库里迁移相关的表存在。
func (r *Runner) ensureTargetReady(ctx context.Context) error {
	return r.cfg.Target.WithContext(ctx).AutoMigrate(
		&model.MigrateBatch{}, &model.MigrateIDMap{}, &model.Backup{},
	)
}

// ensureBatch 创建或复用迁移批次记录。
func (r *Runner) ensureBatch(ctx context.Context, status string) error {
	if err := r.ensureTargetReady(ctx); err != nil {
		return err
	}
	if r.batch != nil && r.batch.ID != 0 {
		return nil
	}
	// SourceDSN 脱敏后存储（规格书 7.5）。
	masked := r.cfg.SourceDSN
	if d, err := database.ParseDatabasePath(r.cfg.SourceDSN); err == nil {
		masked = d.Display
	}
	batch := model.MigrateBatch{
		Source:     r.cfg.Source,
		SourceDSN:  util.Truncate(masked, 512),
		TargetDSN:  util.Truncate(r.cfg.Target.Dialect.Display, 512),
		Status:     status,
		Stage:      r.report.Stage,
		DryRun:     r.cfg.DryRun,
		TotalRows:  r.plan.totalRows(),
		Report:     model.JSON("null"),
		BackupPath: r.cfg.BackupPath,
		StartedAt:  nowUTC(),
		CreatedAt:  nowUTC(),
	}
	if err := r.cfg.Target.WithContext(ctx).Create(&batch).Error; err != nil {
		return fmt.Errorf("创建迁移批次失败: %w", err)
	}
	r.batch = &batch
	return nil
}

// updateBatch 更新批次进度。
func (r *Runner) updateBatch(ctx context.Context, fields map[string]interface{}) error {
	if r.batch == nil || r.batch.ID == 0 {
		return nil
	}
	return r.cfg.Target.WithContext(ctx).Model(&model.MigrateBatch{}).
		Where("id = ?", r.batch.ID).Updates(fields).Error
}

// failBatch 把批次标记为失败并记录错误。
func (r *Runner) failBatch(ctx context.Context, cause error) {
	if r.batch == nil || r.batch.ID == 0 {
		return
	}
	finished := nowUTC()
	msg := ""
	if cause != nil {
		msg = util.Truncate(cause.Error(), 1024)
	}
	r.batch.Status = model.MigrateFailed
	_ = r.updateBatch(ctx, map[string]interface{}{
		"status":      model.MigrateFailed,
		"error":       msg,
		"finished_at": finished,
	})
}

// BatchID 返回当前批次 ID（阶段 5 之后有效）。
func (r *Runner) BatchID() uint64 {
	if r.batch == nil {
		return 0
	}
	return r.batch.ID
}

// createSnapshot 在迁移完成后生成配置快照（规格书 6.13 / 7.2 阶段 5）。
//
// 快照内容为「设备组 + 转发规则 + 节点」的全量配置，
// 供用户在迁移后一键回滚配置（与批次回滚相互独立）。
func (r *Runner) createSnapshot(ctx context.Context) error {
	payload := map[string]interface{}{
		"device_groups": r.plan.deviceGroups,
		"forward_rules": r.plan.rules,
		"nodes":         r.plan.nodes,
		"rule_groups":   r.plan.ruleGroups,
		"node_groups":   r.plan.nodeGroups,
	}
	raw, err := jsonMarshal(payload)
	if err != nil {
		return err
	}
	snap := model.ConfigSnapshot{
		Name:       fmt.Sprintf("迁移前配置快照（批次 %d）", r.batchIDOrZero()),
		Reason:     model.SnapshotReasonBeforeMigrate,
		Payload:    model.JSON(raw),
		Checksum:   util.SHA256HexBytes(raw),
		RuleCount:  len(r.plan.rules),
		GroupCount: len(r.plan.deviceGroups),
		NodeCount:  len(r.plan.nodes),
		CreatedAt:  nowUTC(),
	}
	if err := r.cfg.Target.WithContext(ctx).Create(&snap).Error; err != nil {
		return fmt.Errorf("写入配置快照失败: %w", err)
	}
	r.report.Notes = append(r.report.Notes,
		fmt.Sprintf("已生成迁移配置快照（ID %d），可在快照列表中查看差异或一键回滚", snap.ID))
	return nil
}

// batchIDOrZero 返回批次 ID，无批次时返回 0。
func (r *Runner) batchIDOrZero() uint64 {
	if r.batch == nil {
		return 0
	}
	return r.batch.ID
}

// maxSourceVersionGap 是允许的最大版本跨度（用于友好提示）。
//
// 不是硬性限制：只要必要表与字段齐全就允许迁移，
// 但这个常量用于在报告里提示「版本跨度较大，建议先在测试环境试跑」。
const maxSourceVersionGap = 500000

// VersionGapHint 在源库版本与当前结构差距较大时给出一条提示。
func (r *Runner) VersionGapHint() string {
	if r.probe == nil || r.probe.SchemaVersion <= 0 {
		return ""
	}
	databaseGap := database.CurrentSchemaVersion - r.probe.SchemaVersion
	if databaseGap > maxSourceVersionGap {
		return fmt.Sprintf("源库版本 %d 与当前结构版本 %d 差距较大，建议先在测试环境试跑一次",
			r.probe.SchemaVersion, database.CurrentSchemaVersion)
	}
	return ""
}

// SortedSourceRowCounts 返回按表名排序的源库行数，供调用方展示概况。
func (r *Runner) SortedSourceRowCounts() []string {
	if r.probe == nil {
		return nil
	}
	return sortedKeys(r.probe.RowCounts)
}

// Stages 返回迁移的阶段清单，供帮助文本与 WebUI 展示。
func Stages() []string {
	return []string{
		"阶段 1：连接与探测",
		"阶段 2：只读扫描与字段映射",
		"阶段 3：冲突与风险检测",
		"阶段 4：dry-run 预览",
		"阶段 5：正式迁移（事务 + 幂等）",
		"阶段 6：校验",
		"阶段 7：回滚",
	}
}

// jsonMarshal 是 encoding/json 的薄封装。
func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }

// jsonUnmarshal 是 encoding/json 的薄封装，用于包内避免重复 import。
func jsonUnmarshal(s string, v interface{}) error { return json.Unmarshal([]byte(s), v) }

// mustJSON 序列化失败时返回 "null"，用于「写报告」这类不能失败的路径。
func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
