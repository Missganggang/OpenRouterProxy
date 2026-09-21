package config

// DefaultYAML 是首次启动时写入 config.yml 的默认配置文本。
//
// 内容与规格书 3.1 完全一致：个人自用版，开箱即用，
// 默认 SQLite、单端口监听、不依赖域名与证书。
const DefaultYAML = `# ── 数据存储 ───────────────────────────────────────────────
# 默认 SQLite，文件与本程序同目录，无需任何外部依赖
database-path: sqlite3://data.db
# MySQL 示例（如需切换）：
# database-path: "mysql://user:pass@tcp(127.0.0.1:3306)/openroute?charset=utf8mb4&parseTime=true"
# PostgreSQL 示例：
# database-path: "postgres://host=localhost user=gorm password=gorm dbname=gorm port=5432 sslmode=disable TimeZone=Asia/Shanghai"
max-open-connection: 100
max-idle-connection: 5

# ── 监听 ──────────────────────────────────────────────────
# 个人自用可直接监听 0.0.0.0，用 IP + 非标准端口访问，无需域名与 HTTPS
listen: 0.0.0.0:18888
# 若自行配置了证书可开启（非必须）
# tls-cert: ./some.crt
# tls-key: ./some.key

# ── 前端静态资源 ──────────────────────────────────────────
html-path: ./public
# Linux node clients: <directory>/<arch>/rel_nodeclient
node-binary-path: ./node-binaries

# ── 面板密钥（首次启动自动生成随机值，请勿外泄）─────────────
# 用于签发 Session/JWT、加密节点通信凭证
secret-key: ""

# ── 节点存活判定 ──────────────────────────────────────────
# 心跳间隔（秒）；节点每 N 秒上报一次
heartbeat-interval: 10
# 超过多少秒未上报视为离线（最低 20）
offline-node-time: 20
# 离线节点数据保留时长（秒），超期清理（最低 600）
offline-node-retention-time: 86400

# ── 接口限流 ──────────────────────────────────────────────
user-rate-limit:
  rate: 5
  limit: 5
default-rate-limit:
  rate: 5
  limit: 5

# ── 行为开关 ──────────────────────────────────────────────
disable-gzip: false
disable-queue: false
# 关闭全部定时任务（调试用）
disable-cron: false

# ── 日志 ──────────────────────────────────────────────────
log-level: info          # debug | info | warn | error
log-path: ./logs         # 目录，按天切割
log-keep-days: 14

# ── 流量统计 ──────────────────────────────────────────────
# 流量落库聚合间隔（秒）
traffic-collect-interval: 60
# 明细采样保留天数（聚合数据永久保留）
traffic-detail-keep-days: 30

# ── 探针 ──────────────────────────────────────────────────
enable-probe: true
# 探针数据保留天数
probe-keep-days: 7

# ── WebSSH ────────────────────────────────────────────────
enable-webssh: true
`

// Default 返回一份完整的默认配置（内存值），用于字段缺省填充。
func Default() *Config {
	return &Config{
		DatabasePath:             "sqlite3://data.db",
		MaxOpenConnection:        100,
		MaxIdleConnection:        5,
		Listen:                   "0.0.0.0:18888",
		HTMLPath:                 "./public",
		NodeBinaryPath:           "./node-binaries",
		HeartbeatInterval:        10,
		OfflineNodeTime:          20,
		OfflineNodeRetentionTime: 86400,
		UserRateLimit:            RateLimit{Rate: 5, Limit: 5},
		DefaultRateLimit:         RateLimit{Rate: 5, Limit: 5},
		DisableGzip:              false,
		DisableQueue:             false,
		DisableCron:              false,
		LogLevel:                 "info",
		LogPath:                  "./logs",
		LogKeepDays:              14,
		TrafficCollectInterval:   60,
		TrafficDetailKeepDays:    30,
		EnableProbe:              true,
		ProbeKeepDays:            7,
		EnableWebSSH:             true,
	}
}
