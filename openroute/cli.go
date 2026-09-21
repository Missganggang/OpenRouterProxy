package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/openroute/openroute/internal/app"
)

// Options 是命令行参数解析结果（规格书 3.2）。
type Options struct {
	// ConfigPath 是配置文件路径，默认 ./config.yml
	ConfigPath string

	// 基础操作
	ShowHelp    bool
	ShowVersion bool
	Check       bool

	// 管理员相关
	ResetPassword bool

	// 清理
	Clean int

	// 数据库转换
	CopyDatabase string
	Force        bool

	// 迁移：from=nyanpass,dsn=...,dry-run=true
	MigrateSpec string

	// 备份与恢复
	BackupPath  string
	RestorePath string
	WithSecret  bool
}

// parseFlags 解析全部命令行参数。
//
// 为兼容 Nyanpass 的使用习惯，参数同时支持
// `-clean 1`（单横线）与 `--clean 1`（双横线）两种写法。
// Go 的 flag 包本身已同时接受两者，因此无需额外处理。
//
// 返回解析结果。
func parseFlags() Options {
	var o Options

	flag.StringVar(&o.ConfigPath, "c", "./config.yml", "配置文件路径（默认 ./config.yml）")
	flag.StringVar(&o.ConfigPath, "config", "./config.yml", "配置文件路径（默认 ./config.yml）")

	flag.BoolVar(&o.ShowHelp, "h", false, "显示帮助与当前版本")
	flag.BoolVar(&o.ShowHelp, "help", false, "显示帮助与当前版本")
	flag.BoolVar(&o.ShowVersion, "v", false, "仅打印版本号后退出")
	flag.BoolVar(&o.ShowVersion, "version", false, "仅打印版本号后退出")

	flag.BoolVar(&o.Check, "check", false, "自检：配置、数据库、端口、目录权限")

	flag.BoolVar(&o.ResetPassword, "reset-password", false, "交互式重置管理员密码")

	flag.IntVar(&o.Clean, "clean", 0, "清理失效规则(1)或异常权重(2)")

	flag.StringVar(&o.CopyDatabase, "copy-database", "", "把当前数据复制到指定的 database-path（目标库会被完全覆盖）")
	flag.BoolVar(&o.Force, "force", false, "非交互模式下跳过二次确认（需与 -copy-database 同用）")

	flag.StringVar(&o.MigrateSpec, "migrate", "", "从其它面板迁移数据，格式 from=nyanpass,dsn=...,dry-run=true")

	flag.StringVar(&o.BackupPath, "backup", "", "导出全量备份为单文件（zip）")
	flag.StringVar(&o.RestorePath, "restore", "", "从备份文件恢复（恢复前会自动备份当前数据）")
	flag.BoolVar(&o.WithSecret, "with-secret", false, "备份时保留 secret-key（默认脱敏）")

	// 关闭 flag 包默认的错误输出与用法提示，由 printHelp 统一处理，
	// 保证帮助信息是中文且包含版本号。
	flag.Usage = func() {}
	flag.CommandLine.SetOutput(os.Stderr)
	flag.Parse()

	// 兼容 `-migrate from=...` 之后还跟了逗号分隔片段的写法。
	if o.MigrateSpec != "" {
		o.MigrateSpec = strings.TrimSpace(o.MigrateSpec)
	}

	return o
}

// printHelp 打印帮助信息与当前版本（规格书 3.2）。
func printHelp() {
	fmt.Printf(`OpenRoute 面板 %s

用法：
  openroute [选项]

常用选项：
  -h, -help              显示本帮助与当前版本
  -v, -version           仅打印版本号后退出
  -c, -config <路径>      指定配置文件（默认 ./config.yml）
  -check                 自检：配置、数据库、端口、目录权限、节点通信

管理员：
  -reset-password        交互式重置管理员密码

维护：
  -clean 1               清理失效规则（目标为空、端口冲突、节点已删除）
  -clean 2               清理节点负载权重（重置异常权重为默认值）
  -copy-database <dsn>   迁移/转换数据库（目标库会被完全覆盖且不可恢复）
  -force                 非交互模式下跳过 -copy-database 的二次确认

迁移：
  -migrate from=nyanpass,dsn=<源库DSN>,dry-run=true
                         从 Nyanpass 迁移数据；dry-run=true 只预览不写库
  -migrate rollback=<批次ID>
                         回滚指定迁移批次

备份与恢复：
  -backup <文件>          导出全量备份为单个 zip 文件
  -restore <文件>         从备份文件恢复（恢复前自动备份当前数据）
  -with-secret           备份时保留 secret-key（默认脱敏）

首次初始化：
  MIGRATE=1 ADMIN="admin" ./openroute      创建管理员并启动（推荐）
  ./openroute                              进入交互式引导创建管理员

升级数据库结构：
  MIGRATE=1 ./openroute

示例：
  ./openroute
  MIGRATE=1 ADMIN=admin ./openroute
  ./openroute -backup ./backup-20260101.zip
  ./openroute -migrate from=nyanpass,dsn="sqlite3:///old/data.db",dry-run=true
  ./openroute -copy-database "mysql://user:pass@tcp(127.0.0.1:3306)/openroute"

更多文档见 docs/DEPLOY.md 与 docs/API.md。
`, app.Version)
}
