package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// NewLogger 按配置初始化 zap 日志器。
//
// 行为（规格书 3.1）：
//   - 日志同时输出到终端与文件（按天切割，保留 log-keep-days 天）；
//   - 终端输出带颜色、人类可读；文件输出为 JSON，便于后续检索；
//   - log-path 不存在时自动创建；创建失败则退化为仅终端输出，不阻断启动。
//
// 返回初始化好的 *zap.Logger。
func (c *Config) NewLogger() *zap.Logger {
	level := parseLevel(c.LogLevel)

	// 终端的编码器：带颜色、时间可读。
	consoleEncCfg := zap.NewDevelopmentEncoderConfig()
	consoleEncCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	consoleCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(consoleEncCfg),
		zapcore.AddSync(os.Stdout),
		level,
	)

	cores := []zapcore.Core{consoleCore}

	// 文件输出：按天切割。创建目录失败时跳过文件日志而不是退出，
	// 因为「面板能启动」比「日志能落盘」优先级更高（个人自用场景）。
	if c.LogPath != "" {
		if err := os.MkdirAll(c.LogPath, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 创建日志目录 %s 失败，将仅输出到终端: %v\n", c.LogPath, err)
		} else {
			fileEncCfg := zap.NewProductionEncoderConfig()
			fileEncCfg.EncodeTime = zapcore.ISO8601TimeEncoder
			keepDays := c.LogKeepDays
			if keepDays <= 0 {
				keepDays = 14
			}
			writer := &lumberjack.Logger{
				Filename: filepath.Join(c.LogPath, "openroute.log"),
				// 单文件 64MB，超过则轮转；同时按天轮转由 lumberjack 的 MaxAge 配合实现保留天数。
				MaxSize:    64,
				MaxBackups: keepDays * 2,
				MaxAge:     keepDays,
				Compress:   true,
				LocalTime:  true,
			}
			cores = append(cores, zapcore.NewCore(
				zapcore.NewJSONEncoder(fileEncCfg),
				zapcore.AddSync(writer),
				level,
			))
		}
	}

	logger := zap.New(zapcore.NewTee(cores...),
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
	)
	// 把标准库 log 与 zap 的全局 logger 都接管，避免第三方库绕过日志配置。
	zap.ReplaceGlobals(logger)
	return logger
}

// parseLevel 把配置中的日志级别字符串转换为 zap 级别。
//
// 无法识别时回退到 info，不报错——调试时写错级别不应导致启动失败。
func parseLevel(s string) zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	case "info", "":
		return zapcore.InfoLevel
	default:
		return zapcore.InfoLevel
	}
}
