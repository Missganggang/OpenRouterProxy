package util

import (
	"encoding/json"
	"strings"
)

// ToJSON 把任意值序列化为 JSON 字符串，失败时返回 "null"。
//
// 用于审计日志的 before/after 快照、Webhook 请求体等场景。
func ToJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// PrettyJSON 返回缩进后的 JSON 字符串，便于终端报告与差异对比展示。
func PrettyJSON(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "null"
	}
	return string(b)
}

// IsValidJSON 判断字符串是否为合法 JSON。
//
// 供迁移工具使用：SQLite 的 TEXT JSON 字段迁往 MySQL 的 JSON 列时，
// MUST 校验合法性，非法值记录并跳过该行。
func IsValidJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	return json.Valid([]byte(s))
}

// Clone 通过 JSON 序列化做深拷贝。
//
// 用于审计日志记录「变更前快照」——必须与数据库中的对象完全隔离，
// 否则后续修改会污染已记录的快照。
func Clone[T any](v T) T {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out
	}
	return out
}

// ParseIntList 解析逗号分隔的整数列表，如「1,2,3」。
//
// 用于兼容 Nyanpass 的「限制出口」写法（规格书 6.3）。
// 非法条目会被跳过；返回解析成功的 ID 列表与「是否包含禁止单端」标记。
func ParseIntList(s string) ([]uint64, bool) {
	out := []uint64{}
	banDirect := false
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "禁止单端") {
			banDirect = true
			continue
		}
		var id uint64
		if _, err := parseUint(part, &id); err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, banDirect
}

// parseUint 是 strconv.ParseUint 的薄封装，避免在本文件引入额外依赖。
func parseUint(s string, out *uint64) (int, error) {
	var v uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNotNumber
		}
		v = v*10 + uint64(c-'0')
	}
	*out = v
	return len(s), nil
}

// errNotNumber 表示解析到非数字字符。
var errNotNumber = &parseError{"不是合法数字"}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }
