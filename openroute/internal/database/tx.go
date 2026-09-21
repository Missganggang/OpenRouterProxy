package database

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Tx 在事务中执行 fn。
//
// 规格书 2.3 要求：跨表写入（如「删规则 + 清流量 + 写审计」）MUST 放在一个事务里。
// 业务层统一通过本方法开启事务，保证错误时整体回滚。
//
// 参数 ctx 用于传递请求上下文与取消信号。
// 返回 fn 的原始错误，调用方据此决定是否包装成 AppError。
func (db *DB) Tx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(tx)
	})
}

// TrafficUpsert 是流量 UPSERT 的输入行。
//
// 单独定义而不直接复用 model.TrafficLog，是为了让「累加语义」在类型上就清晰：
// 这里传入的 RawBytes / Bytes 是**增量**，不是最终值。
type TrafficUpsert struct {
	Date      string    `gorm:"column:date"`
	Hour      int       `gorm:"column:hour"`
	UserID    uint64    `gorm:"column:user_id"`
	RuleID    uint64    `gorm:"column:rule_id"`
	NodeID    uint64    `gorm:"column:node_id"`
	Direction string    `gorm:"column:direction"`
	RawBytes  int64     `gorm:"column:raw_bytes"`
	Bytes     int64     `gorm:"column:bytes"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 让 Gorm 把 TrafficUpsert 映射到 traffic_logs 表。
func (TrafficUpsert) TableName() string { return "traffic_logs" }

// UpsertTraffic 是流量表专用 UPSERT，跨方言统一使用 clause.OnConflict。
//
// 规格书 4.2.8 明确要求：MUST 使用 Gorm 的 clause.OnConflict，
// 而不是手写 `ON CONFLICT DO UPDATE` / `ON DUPLICATE KEY UPDATE` SQL，
// 否则三种方言要维护三套语句。
//
// 唯一键为 (date, hour, user_id, rule_id, node_id, direction)；
// 冲突时把本批的原始字节与折算字节**累加**到已有行。
//
// 参数 rows 为待写入的流量聚合增量；返回写入错误。
func (db *DB) UpsertTraffic(ctx context.Context, rows []TrafficUpsert) error {
	if len(rows) == 0 {
		return nil
	}
	// 分批写入，避免单条 SQL 的参数数量超过 SQLite 的默认上限（999 个变量）。
	const batchSize = 100
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]

		err := db.DB.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "date"}, {Name: "hour"}, {Name: "user_id"},
				{Name: "rule_id"}, {Name: "node_id"}, {Name: "direction"},
			},
			// 目标行的 raw_bytes 累加上「本批待插入值」。
			// excluded.<col> 指向冲突行（即本次 INSERT 想写入的那一行）的值，
			// 三种方言都支持这一引用，因此不需要手写方言分支。
			DoUpdates: clause.Assignments(map[string]interface{}{
				"raw_bytes":  gorm.Expr("raw_bytes + excluded.raw_bytes"),
				"bytes":      gorm.Expr("bytes + excluded.bytes"),
				"updated_at": time.Now().UTC(),
			}),
		}).Create(&batch).Error
		if err != nil {
			return err
		}
	}
	return nil
}

// UpsertRow 是通用的「按键幂等写入」辅助，用于系统设置等键值表。
//
// 参数 model 为目标模型；conflictCols 为唯一键列名；updates 为冲突时要更新的列。
func (db *DB) UpsertRow(ctx context.Context, value interface{}, conflictCols []clause.Column, updates []string) error {
	assignments := make([]clause.Assignment, 0, len(updates))
	for _, col := range updates {
		assignments = append(assignments, clause.Assignment{
			Column: clause.Column{Name: col},
			// 三大方言都支持引用目标行自身的列名做自赋值，
			// 这里用 excluded 的语义通过 Gorm 统一生成。
			Value: gorm.Expr("excluded." + col),
		})
	}
	return db.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   conflictCols,
		DoUpdates: clause.Assignments(assignmentsToMap(assignments)),
	}).Create(value).Error
}

// assignmentsToMap 把 clause.Assignment 列表转成 DoUpdates 需要的 map 形式。
func assignmentsToMap(list []clause.Assignment) map[string]interface{} {
	out := make(map[string]interface{}, len(list))
	for _, a := range list {
		out[a.Column.Name] = a.Value
	}
	return out
}
