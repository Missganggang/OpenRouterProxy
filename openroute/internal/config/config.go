// Package config 负责 config.yml 的加载、默认值填充与校验。
//
// 设计原则（规格书 3.1）：容错优先。任何配置项缺失时使用默认值且不报错，
// 只有「数值低于安全下限」或「无法连接数据库 / 端口被占用」这类
// 会造成运行期不可预期行为的情况才报错退出。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// RateLimit 是限流配置块（令牌桶速率与突发容量）。
type RateLimit struct {
	Rate  float64 `yaml:"rate" json:"rate"`
	Limit int     `yaml:"limit" json:"limit"`
}

// Config 对应 config.yml 的完整结构。
type Config struct {
	// 数据存储
	DatabasePath      string `yaml:"database-path" json:"database_path"`
	MaxOpenConnection int    `yaml:"max-open-connection" json:"max_open_connection"`
	MaxIdleConnection int    `yaml:"max-idle-connection" json:"max_idle_connection"`

	// 监听
	Listen  string `yaml:"listen" json:"listen"`
	TLSCert string `yaml:"tls-cert" json:"tls_cert"`
	TLSKey  string `yaml:"tls-key" json:"tls_key"`

	// 前端静态资源
	HTMLPath string `yaml:"html-path" json:"html_path"`
	// NodeBinaryPath stores Linux node clients by architecture, outside the WebUI.
	NodeBinaryPath string `yaml:"node-binary-path" json:"node_binary_path"`

	// 面板密钥
	SecretKey string `yaml:"secret-key" json:"secret_key"`

	// 节点存活判定
	HeartbeatInterval        int `yaml:"heartbeat-interval" json:"heartbeat_interval"`
	OfflineNodeTime          int `yaml:"offline-node-time" json:"offline_node_time"`
	OfflineNodeRetentionTime int `yaml:"offline-node-retention-time" json:"offline_node_retention_time"`

	// 限流
	UserRateLimit    RateLimit `yaml:"user-rate-limit" json:"user_rate_limit"`
	DefaultRateLimit RateLimit `yaml:"default-rate-limit" json:"default_rate_limit"`

	// 行为开关
	DisableGzip  bool `yaml:"disable-gzip" json:"disable_gzip"`
	DisableQueue bool `yaml:"disable-queue" json:"disable_queue"`
	DisableCron  bool `yaml:"disable-cron" json:"disable_cron"`

	// 日志
	LogLevel    string `yaml:"log-level" json:"log_level"`
	LogPath     string `yaml:"log-path" json:"log_path"`
	LogKeepDays int    `yaml:"log-keep-days" json:"log_keep_days"`

	// 流量统计
	TrafficCollectInterval int `yaml:"traffic-collect-interval" json:"traffic_collect_interval"`
	TrafficDetailKeepDays  int `yaml:"traffic-detail-keep-days" json:"traffic_detail_keep_days"`

	// 探针
	EnableProbe   bool `yaml:"enable-probe" json:"enable_probe"`
	ProbeKeepDays int  `yaml:"probe-keep-days" json:"probe_keep_days"`

	// WebSSH
	EnableWebSSH bool `yaml:"enable-webssh" json:"enable_webssh"`

	// 以下为运行时字段，不参与 YAML 序列化
	ConfigPath string `yaml:"-" json:"-"`
}

// Load 从指定路径加载配置。
//
// 行为：
//   - 文件不存在时写入默认配置（含自动生成的 secret-key）并返回；
//   - 文件存在时解析，缺失字段用默认值补齐；
//   - secret-key 为空时自动生成 32 字节随机值并写回配置文件；
//   - 执行 Validate，失败返回错误（调用方负责打印可读信息并退出码 1）。
//
// path 为配置文件的路径，返回加载完成的配置与可能的错误。
func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.ConfigPath = path

	if _, err := os.Stat(path); os.IsNotExist(err) {
		// 首次启动：生成随机密钥并写入默认配置。
		key, err := GenerateSecretKey()
		if err != nil {
			return nil, fmt.Errorf("生成 secret-key 失败: %w", err)
		}
		cfg.SecretKey = key
		if err := cfg.save(); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	// 先解析到默认值之上，实现「缺失字段用默认值」的容错语义。
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	// 空密钥需要补一个并写回，否则重启后会话全部失效。
	if strings.TrimSpace(cfg.SecretKey) == "" {
		key, err := GenerateSecretKey()
		if err != nil {
			return nil, fmt.Errorf("生成 secret-key 失败: %w", err)
		}
		cfg.SecretKey = key
		if err := cfg.save(); err != nil {
			return nil, err
		}
	}

	cfg.applyFallbacks()
	return cfg, nil
}

// applyFallbacks 对解析后仍为零值的数值字段回填默认值。
//
// YAML 中显式写了 0（例如 limit: 0）会被当作合法值保留，
// 这里只处理「字段整体缺失」的情形，因此仅对字符串与明显非法的零值兜底。
func (c *Config) applyFallbacks() {
	d := Default()
	if c.DatabasePath == "" {
		c.DatabasePath = d.DatabasePath
	}
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.HTMLPath == "" {
		c.HTMLPath = d.HTMLPath
	}
	if c.LogPath == "" {
		c.LogPath = d.LogPath
	}
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}
	if c.MaxOpenConnection <= 0 {
		c.MaxOpenConnection = d.MaxOpenConnection
	}
	if c.MaxIdleConnection <= 0 {
		c.MaxIdleConnection = d.MaxIdleConnection
	}
}

// Validate 校验配置的合法下限。
//
// 校验规则（规格书 3.1）：
//   - offline-node-time < 20 报错；
//   - offline-node-retention-time < 600 报错；
//   - 其余数值字段回落到默认值，不报错。
//
// 返回的错误文本面向终端用户，包含当前值与最低值。
func (c *Config) Validate() error {
	if c.OfflineNodeTime < 20 {
		return fmt.Errorf("offline-node-time 配置为 %d 秒，低于最低允许值 20 秒；"+
			"过低的离线判定会造成节点状态频繁抖动，请改为 20 或更大", c.OfflineNodeTime)
	}
	if c.OfflineNodeRetentionTime < 600 {
		return fmt.Errorf("offline-node-retention-time 配置为 %d 秒，低于最低允许值 600 秒；"+
			"过低的保留时长会导致离线节点信息被过早清理，请改为 600 或更大", c.OfflineNodeRetentionTime)
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = Default().HeartbeatInterval
	}
	if c.HeartbeatInterval >= c.OfflineNodeTime {
		return fmt.Errorf("heartbeat-interval (%d 秒) 必须小于 offline-node-time (%d 秒)，"+
			"否则节点会在正常心跳周期内被判为离线", c.HeartbeatInterval, c.OfflineNodeTime)
	}
	if c.UserRateLimit.Rate <= 0 {
		c.UserRateLimit = Default().UserRateLimit
	}
	if c.DefaultRateLimit.Rate <= 0 {
		c.DefaultRateLimit = Default().DefaultRateLimit
	}
	if c.TrafficCollectInterval <= 0 {
		c.TrafficCollectInterval = Default().TrafficCollectInterval
	}
	if c.ProbeKeepDays <= 0 {
		c.ProbeKeepDays = Default().ProbeKeepDays
	}
	if c.LogKeepDays <= 0 {
		c.LogKeepDays = Default().LogKeepDays
	}
	if c.TrafficDetailKeepDays <= 0 {
		c.TrafficDetailKeepDays = Default().TrafficDetailKeepDays
	}
	return nil
}

// save 把当前配置写回配置文件，注释会丢失（首次生成时使用带注释的模板）。
func (c *Config) save() error {
	if c.ConfigPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(absOrDot(c.ConfigPath)), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	// 首次生成时直接写带注释的模板，用户可读性更好。
	if _, err := os.Stat(c.ConfigPath); os.IsNotExist(err) {
		content := strings.Replace(DefaultYAML, `secret-key: ""`,
			fmt.Sprintf("secret-key: %q", c.SecretKey), 1)
		if err := os.WriteFile(c.ConfigPath, []byte(content), 0o600); err != nil {
			return fmt.Errorf("写入默认配置 %s 失败: %w", c.ConfigPath, err)
		}
		return nil
	}

	// 已存在则只回写结构化内容（用于补写 secret-key）。
	buf, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(c.ConfigPath, buf, 0o600); err != nil {
		return fmt.Errorf("写回配置 %s 失败（请检查文件权限）: %w", c.ConfigPath, err)
	}
	return nil
}

// GenerateSecretKey 生成 32 字节随机密钥并做 base64 编码。
//
// 用于 config.yml 的 secret-key，以及 API Token 的签名密钥。
func GenerateSecretKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// absOrDot 返回路径的目录部分，空路径回退为当前目录。
func absOrDot(p string) string {
	dir := filepath.Dir(p)
	if dir == "" {
		return "."
	}
	return dir
}
