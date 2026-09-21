package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// JSON 是跨方言统一的 JSON 字段类型。
//
// 设计目的：SQLite/PostgreSQL 用 TEXT/JSONB 存储 JSON，MySQL 用 JSON，
// 三者的读写差异由本类型在 Gorm 层统一抹平，业务代码只面对 []byte。
//
// 实现 driver.Valuer 与 sql.Scanner，因此可以直接作为模型字段类型使用。
type JSON []byte

// Value 实现 driver.Valuer，把 JSON 写入数据库时转成字符串。
// 空值写入 "null"，避免出现 SQL NULL 与空串两种「空状态」。
func (j JSON) Value() (driver.Value, error) {
	if len(j) == 0 {
		return "null", nil
	}
	return string(j), nil
}

// Scan 实现 sql.Scanner，从数据库读出任意类型并规范化为 JSON 字节。
func (j *JSON) Scan(value interface{}) error {
	if value == nil {
		*j = JSON("null")
		return nil
	}
	switch v := value.(type) {
	case []byte:
		*j = append((*j)[0:0], v...)
	case string:
		*j = JSON(v)
	default:
		return fmt.Errorf("model.JSON: 不支持的扫描类型 %T", value)
	}
	if len(*j) == 0 {
		*j = JSON("null")
	}
	return nil
}

// MarshalJSON 实现 json.Marshaler，避免字段被重复编码为 base64。
func (j JSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(j) {
		// 非法 JSON 原样包装为字符串，保证响应体始终可被前端解析。
		return json.Marshal(string(j))
	}
	return j, nil
}

// UnmarshalJSON 实现 json.Unmarshaler，原样保存请求体中的 JSON 片段。
func (j *JSON) UnmarshalJSON(data []byte) error {
	if j == nil {
		return errors.New("model.JSON: UnmarshalJSON 到 nil 指针")
	}
	*j = append((*j)[0:0], data...)
	return nil
}

// GormDataType 返回默认列类型，SQLite/未知方言使用 text。
func (JSON) GormDataType() string { return "text" }

// GormDBDataType 按方言返回合适的列类型：
// MySQL 用 JSON，PostgreSQL 用 JSONB，其余（含 SQLite）用 TEXT。
func (JSON) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	switch db.Dialector.Name() {
	case "mysql":
		return "JSON"
	case "postgres":
		return "JSONB"
	default:
		return "TEXT"
	}
}

// 编译期校验类型满足度，避免运行期才发现签名不匹配。
var (
	_ driver.Valuer    = JSON(nil)
	_ json.Marshaler   = JSON(nil)
	_ json.Unmarshaler = (*JSON)(nil)
)

// AsSlice 把 JSON 数组解析为 []string，解析失败返回空切片（不报错，容错优先）。
func (j JSON) AsSlice() []string {
	var out []string
	if len(j) == 0 || !json.Valid(j) {
		return []string{}
	}
	if err := json.Unmarshal(j, &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// AsUint64Slice 把 JSON 解析为 []uint64，同样容错返回空切片。
func (j JSON) AsUint64Slice() []uint64 {
	var out []uint64
	if len(j) == 0 || !json.Valid(j) {
		return []uint64{}
	}
	if err := json.Unmarshal(j, &out); err != nil {
		return []uint64{}
	}
	if out == nil {
		return []uint64{}
	}
	return out
}

// unmarshalJSON 是包内共用的小工具，把 JSON 字节解析到目标结构。
// 与 json.Unmarshal 的区别是空值直接视为成功，避免调用方到处判空。
func unmarshalJSON(data JSON, v interface{}) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

// FromAny 把任意 Go 值序列化为 JSON 字段。
func FromAny(v interface{}) JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return JSON("null")
	}
	return JSON(b)
}
