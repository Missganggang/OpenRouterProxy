package database

import (
	"fmt"
	"strings"
)

// Dialect 描述数据库方言的解析结果。
type Dialect struct {
	// Name 是 Gorm 方言名：sqlite / mysql / postgres
	Name string
	// DSN 是传给驱动的连接串（已从 database-path 前缀中剥离）
	DSN string
	// File 是 SQLite 的文件路径，其它方言为空
	File string
	// Display 是用于横幅与日志展示的可读描述，已隐藏密码
	Display string
}

// ParseDatabasePath 把 config.yml 中的 `database-path` 解析为方言信息。
//
// 支持的写法（与 Nyanpass 的 database-path 习惯一致）：
//
//	sqlite3://data.db          → SQLite，文件 data.db
//	sqlite3:///abs/path.db     → SQLite，绝对路径
//	mysql://user:pass@tcp(127.0.0.1:3306)/db?charset=utf8mb4&parseTime=true
//	postgres://user:pass@host:5432/db?sslmode=disable
//
// 不含前缀的纯路径按 SQLite 处理，便于直接填 `./data.db`。
//
// 返回值中的 Display 会把密码替换为 ***，可以安全地打进日志。
func ParseDatabasePath(path string) (*Dialect, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return nil, fmt.Errorf("database-path 为空")
	}

	lower := strings.ToLower(p)

	switch {
	case strings.HasPrefix(lower, "sqlite3://"), strings.HasPrefix(lower, "sqlite://"):
		raw := p[strings.Index(p, "://")+3:]
		// 三段斜杠（sqlite3:///abs/path.db）是「绝对路径」的惯用写法：
		// 前两斜杠是协议分隔符，第三个斜杠才是路径的根。
		// 剥掉一个前导斜杠后交给 SQLite 驱动，Windows 上也能正确解析相对文件名。
		if strings.HasPrefix(raw, "/") {
			raw = strings.TrimPrefix(raw, "/")
		}
		if raw == "" {
			return nil, fmt.Errorf("database-path %q 缺少 SQLite 文件路径", path)
		}
		return &Dialect{Name: "sqlite", DSN: raw, File: raw, Display: "sqlite3 (" + raw + ")"}, nil

	case strings.HasPrefix(lower, "mysql://"):
		dsn := p[len("mysql://"):]
		return &Dialect{
			Name:    "mysql",
			DSN:     dsn,
			Display: "mysql (" + maskCredentials(dsn) + ")",
		}, nil

	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		idx := strings.Index(p, "://")
		dsn := p[idx+3:]
		return &Dialect{
			Name:    "postgres",
			DSN:     dsn,
			Display: "postgres (" + maskCredentials(dsn) + ")",
		}, nil

	default:
		// 无前缀，按 SQLite 文件路径处理。
		return &Dialect{Name: "sqlite", DSN: p, File: p, Display: "sqlite3 (" + p + ")"}, nil
	}
}

// maskCredentials 隐藏 DSN 中的密码部分，返回可安全打印的字符串。
//
// 处理两类写法：URL 形式的 `user:pass@host` 与 PG 关键字形式的 `password=xxx`。
func maskCredentials(dsn string) string {
	out := dsn

	// URL 形式：user:pass@host
	if at := strings.Index(out, "@"); at > 0 {
		if colon := strings.Index(out[:at], ":"); colon >= 0 {
			out = out[:colon+1] + "***" + out[at:]
			return out
		}
	}

	// PG 关键字形式：password=secret 后接空格或串尾
	low := strings.ToLower(out)
	if eq := strings.Index(low, "password="); eq >= 0 {
		rest := out[eq+len("password="):]
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		out = out[:eq+len("password=")] + "***" + rest[end:]
	}
	return out
}

// DriverName 返回该方言在 Gorm 中注册的驱动名。
func (d *Dialect) DriverName() string {
	switch d.Name {
	case "mysql":
		return "mysql"
	case "postgres":
		return "postgres"
	default:
		return "sqlite"
	}
}
