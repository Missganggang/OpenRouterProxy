package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/model"
)

// openTestDB 在临时目录建立一个 SQLite 测试库并完成建表。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(Options{Path: "sqlite3://" + path, MaxOpen: 1, MaxIdle: 1})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	if _, err := db.AutoMigrate(); err != nil {
		t.Fatalf("自动建表失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestParseDatabasePath 覆盖三种方言的解析与密码脱敏。
func TestParseDatabasePath(t *testing.T) {
	cases := []struct {
		in      string
		name    string
		dsn     string
		display string
	}{
		{in: "sqlite3://data.db", name: "sqlite", dsn: "data.db", display: "sqlite3 (data.db)"},
		{in: "sqlite3:///opt/or/data.db", name: "sqlite", dsn: "opt/or/data.db"},
		{in: "./data.db", name: "sqlite", dsn: "./data.db"},
		{in: "mysql://root:secret@tcp(127.0.0.1:3306)/openroute", name: "mysql"},
		{in: "postgres://u:p@localhost:5432/db?sslmode=disable", name: "postgres"},
	}
	for _, c := range cases {
		d, err := ParseDatabasePath(c.in)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", c.in, err)
		}
		if d.Name != c.name {
			t.Errorf("解析 %q：方言 = %q，期望 %q", c.in, d.Name, c.name)
		}
		if c.dsn != "" && d.DSN != c.dsn {
			t.Errorf("解析 %q：DSN = %q，期望 %q", c.in, d.DSN, c.dsn)
		}
		// 密码绝不能出现在可打印描述里。
		if d.Name != "sqlite" && (strings.Contains(d.Display, "secret") || strings.Contains(d.Display, ":p@")) {
			t.Errorf("解析 %q：Display 泄露了密码: %s", c.in, d.Display)
		}
	}
}

// TestParseDatabasePathEmpty 空路径必须报错，而不是静默回退到某个默认库。
func TestParseDatabasePathEmpty(t *testing.T) {
	if _, err := ParseDatabasePath("   "); err == nil {
		t.Fatal("空 database-path 应当报错")
	}
}

// TestAutoMigrateCreatesTables 验证全部模型都能建表。
func TestAutoMigrateCreatesTables(t *testing.T) {
	db := openTestDB(t)

	for _, m := range AllModels() {
		if !db.Migrator().HasTable(m) {
			t.Errorf("表未创建: %T", m)
		}
	}
	if v := db.SchemaVersionOf(); v != CurrentSchemaVersion {
		t.Errorf("schema_version = %d，期望 %d", v, CurrentSchemaVersion)
	}
}

// TestUpsertTrafficAccumulates 是核心用例：验证流量 UPSERT 的累加语义。
//
// 同一唯一键写入两次，第二次必须把字节数累加而不是覆盖或报错。
func TestUpsertTrafficAccumulates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	row := func(raw, scaled int64) TrafficUpsert {
		return TrafficUpsert{
			Date: "2026-01-01", Hour: 12, UserID: 1, RuleID: 1, NodeID: 1,
			Direction: model.DirectionIn, RawBytes: raw, Bytes: scaled,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
	}

	if err := db.UpsertTraffic(ctx, []TrafficUpsert{row(500, 750)}); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if err := db.UpsertTraffic(ctx, []TrafficUpsert{row(100, 150)}); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}

	var got model.TrafficLog
	if err := db.Where("date = ? AND hour = ?", "2026-01-01", 12).First(&got).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.RawBytes != 600 {
		t.Errorf("raw_bytes = %d，期望 600（500+100）", got.RawBytes)
	}
	if got.Bytes != 900 {
		t.Errorf("bytes = %d，期望 900（750+150）", got.Bytes)
	}

	// 唯一键约束必须真的生效：写入相同键的行数仍为 1。
	var count int64
	db.Model(&model.TrafficLog{}).Count(&count)
	if count != 1 {
		t.Errorf("行数 = %d，期望 1（唯一索引未生效）", count)
	}
}

// TestUpsertTrafficDifferentDimensions 不同维度必须各自成行。
func TestUpsertTrafficDifferentDimensions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	base := TrafficUpsert{
		Date: "2026-01-01", Hour: 12, UserID: 1, RuleID: 1, NodeID: 1,
		Direction: model.DirectionIn, RawBytes: 10, Bytes: 10,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	rows := []TrafficUpsert{base}
	d2 := base
	d2.Direction = model.DirectionOut
	rows = append(rows, d2)
	d3 := base
	d3.Hour = model.HourDaily
	rows = append(rows, d3)
	d4 := base
	d4.UserID = 2
	rows = append(rows, d4)

	if err := db.UpsertTraffic(ctx, rows); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	var count int64
	db.Model(&model.TrafficLog{}).Count(&count)
	if count != 4 {
		t.Errorf("行数 = %d，期望 4（四个维度组合各不相同）", count)
	}
}

// TestTxRollback 验证事务出错时整体回滚。
func TestTxRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	wantErr := errTest
	err := db.Tx(ctx, func(tx *gorm.DB) error {
		if e := tx.Create(&model.NodeGroup{Name: "应被回滚"}).Error; e != nil {
			return e
		}
		return wantErr
	})
	if err != wantErr {
		t.Fatalf("事务应返回原始错误，得到 %v", err)
	}

	var count int64
	db.Model(&model.NodeGroup{}).Count(&count)
	if count != 0 {
		t.Errorf("事务回滚后行数 = %d，期望 0", count)
	}
}

// TestJSONColumnRoundTrip 验证自定义 JSON 类型在三方言下的读写一致性。
func TestJSONColumnRoundTrip(t *testing.T) {
	db := openTestDB(t)

	node := model.Node{
		Name:        "HK-01",
		Token:       "nsk_test",
		Role:        model.RoleBoth,
		GroupIDs:    model.FromAny([]uint64{1, 2, 3}),
		DriftDetail: model.FromAny(map[string]string{"port": "8443 != 9443"}),
	}
	if err := db.Create(&node).Error; err != nil {
		t.Fatalf("写入节点失败: %v", err)
	}

	var got model.Node
	if err := db.First(&got, node.ID).Error; err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	ids := got.GroupIDs.AsUint64Slice()
	if len(ids) != 3 || ids[0] != 1 || ids[2] != 3 {
		t.Errorf("GroupIDs 往返失败: %v", ids)
	}
}

// TestForwardRuleTargetsRoundTrip 验证 Targets 数组的序列化与解析。
func TestForwardRuleTargetsRoundTrip(t *testing.T) {
	db := openTestDB(t)

	rule := model.ForwardRule{
		Name:           "rule-1",
		InboundGroupID: 1,
		ListenPort:     8443,
		Targets: model.FromAny([]model.Target{
			{Host: "1.2.3.4", Port: 443, Weight: 1, Status: model.TargetUp},
			{Host: "example.com", Port: 80, Weight: 3, Status: model.TargetDown},
		}),
		ChainGroups: model.FromAny([]uint64{5, 6}),
		Shaping:     model.FromAny([]int{1, 2000}),
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}

	var got model.ForwardRule
	if err := db.First(&got, rule.ID).Error; err != nil {
		t.Fatalf("读取规则失败: %v", err)
	}

	targets := got.TargetList()
	if len(targets) != 2 {
		t.Fatalf("Targets 长度 = %d，期望 2", len(targets))
	}
	if targets[1].Host != "example.com" || targets[1].NormalizedWeight() != 3 {
		t.Errorf("Targets 内容不符: %+v", targets[1])
	}
	if targets[1].Up() {
		t.Error("status=down 的目标不应参与分发")
	}
	if chains := got.ChainGroupList(); len(chains) != 2 || chains[1] != 6 {
		t.Errorf("ChainGroups 往返失败: %v", chains)
	}
	if shaping := got.ShapingList(); len(shaping) != 2 || shaping[1] != 2000 {
		t.Errorf("Shaping 往返失败: %v", shaping)
	}
}

// TestUniqueTrafficIndexExists 确认唯一索引真的建出来了。
func TestUniqueTrafficIndexExists(t *testing.T) {
	db := openTestDB(t)
	if !db.Migrator().HasIndex(&model.TrafficLog{}, "idx_traffic_unique") {
		t.Fatal("traffic_logs 缺少 idx_traffic_unique 唯一索引，UPSERT 会退化为重复插入")
	}
}

// errTest 是事务回滚测试用的哨兵错误。
var errTest = &testError{"模拟业务错误"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
