package config

import (
	"gorm.io/gorm/logger"

	"github.com/openroute/openroute/internal/database"
)

// OpenDB 按配置打开数据库连接。
//
// 把连接参数从 Config 转换到 database.Options，
// 并把日志级别映射为 Gorm 的日志级别：
//   - log-level: debug → Gorm 记录所有 SQL
//   - 其它 → 只记录慢查询与错误，避免 SQL 日志淹没业务日志
//
// 返回数据库包装对象与错误；错误信息已包含方言、地址（脱敏）与详情。
func (c *Config) OpenDB() (*database.DB, error) {
	level := logger.Warn
	if c.LogLevel == "debug" {
		level = logger.Info
	}

	return database.Open(database.Options{
		Path:     c.DatabasePath,
		MaxOpen:  c.MaxOpenConnection,
		MaxIdle:  c.MaxIdleConnection,
		LogLevel: level,
	})
}
