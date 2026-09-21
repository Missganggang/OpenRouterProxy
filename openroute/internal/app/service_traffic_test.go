package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/model"
)

// 本文件覆盖规格书 6.10「流量统计」与 4.2.10「在线会话」。
//
// 覆盖点：
//  1. 小时行与天行的区分（hour = -1 vs 0~23），两者混读会重复计数；
//  2. 原始字节与折算字节都要落库；
//  3. 反规范化计数器（forward_rules.traffic_in/out、users.traffic_used）同步累加；
//  4. 展示口径可切换（默认折算值，可切原始值）；
//  5. 时间桶基于整数运算，SQLite 下可用；
//  6. 会话的连接数增减与在线用户统计。

// TestTrafficRecordWritesHourAndDayRows 验证 Record 同时写小时行与天行，且两者不互相覆盖。
func TestTrafficRecordWritesHourAndDayRows(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	now := timeNow()
	hour := now.Hour()

	err := svc.Record(ctx, []TrafficSample{{
		Hour:      hour,
		RuleID:    11,
		UserID:    22,
		NodeID:    33,
		Direction: model.DirectionIn,
		RawBytes:  1000,
		Bytes:     1500,
	}})
	if err != nil {
		t.Fatalf("写入流量失败: %v", err)
	}

	// 小时行：hour = 当前小时。
	var hourRow model.TrafficLog
	if err := app.DB.Where("rule_id = ? AND hour = ?", 11, hour).First(&hourRow).Error; err != nil {
		t.Fatalf("未找到小时行: %v", err)
	}
	if hourRow.RawBytes != 1000 || hourRow.Bytes != 1500 {
		t.Errorf("小时行字节不符: raw=%d bytes=%d，期望 1000/1500",
			hourRow.RawBytes, hourRow.Bytes)
	}

	// 天行：hour = -1。
	var dayRow model.TrafficLog
	if err := app.DB.Where("rule_id = ? AND hour = ?", 11, model.HourDaily).First(&dayRow).Error; err != nil {
		t.Fatalf("未找到天行: %v", err)
	}
	if dayRow.RawBytes != 1000 || dayRow.Bytes != 1500 {
		t.Errorf("天行字节不符: raw=%d bytes=%d，期望 1000/1500",
			dayRow.RawBytes, dayRow.Bytes)
	}

	// 总共两行：一小时行 + 一天行。
	var total int64
	app.DB.Model(&model.TrafficLog{}).Count(&total)
	if total != 2 {
		t.Errorf("流量行数 = %d，期望 2（小时行 + 天行）", total)
	}
}

// TestTrafficRecordExplicitDailyOnly 验证显式传 HourDaily 时只写天行。
//
// 场景：调用方已自行按天汇总时再写小时行会造成重复计数。
func TestTrafficRecordExplicitDailyOnly(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := svc.Record(ctx, []TrafficSample{{
		Hour:      model.HourDaily,
		RuleID:    1,
		Direction: model.DirectionIn,
		RawBytes:  500,
		Bytes:     500,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	var total int64
	app.DB.Model(&model.TrafficLog{}).Count(&total)
	if total != 1 {
		t.Errorf("行数 = %d，期望 1（只写天行）", total)
	}
	var row model.TrafficLog
	app.DB.First(&row)
	if row.Hour != model.HourDaily {
		t.Errorf("hour = %d，期望 %d", row.Hour, model.HourDaily)
	}
}

// TestTrafficRecordAccumulates 验证同一维度重复写入是累加而不是覆盖。
func TestTrafficRecordAccumulates(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)
	hour := timeNow().Hour()

	sample := TrafficSample{
		Hour: hour, RuleID: 5, UserID: 6, NodeID: 7,
		Direction: model.DirectionIn, RawBytes: 100, Bytes: 200,
	}
	for i := 0; i < 3; i++ {
		if err := svc.Record(ctx, []TrafficSample{sample}); err != nil {
			t.Fatalf("第 %d 次写入失败: %v", i+1, err)
		}
	}

	var day model.TrafficLog
	if err := app.DB.Where("rule_id = ? AND hour = ?", 5, model.HourDaily).First(&day).Error; err != nil {
		t.Fatalf("读取天行失败: %v", err)
	}
	if day.RawBytes != 300 || day.Bytes != 600 {
		t.Errorf("累加结果 raw=%d bytes=%d，期望 300/600", day.RawBytes, day.Bytes)
	}
}

// TestTrafficRecordUpdatesCounters 验证反规范化计数器同事务累加。
func TestTrafficRecordUpdatesCounters(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.ForwardRule{
		Name: "rule-x", InboundGroupID: 1, ListenPort: 9000,
	}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	if err := app.DB.Create(&model.User{
		Username: "user-x", PasswordHash: "x", Status: model.StatusEnabled,
	}).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}

	hour := timeNow().Hour()
	err := svc.Record(ctx, []TrafficSample{
		{Hour: hour, RuleID: 1, UserID: 1, Direction: model.DirectionIn, RawBytes: 100, Bytes: 100},
		{Hour: hour, RuleID: 1, UserID: 1, Direction: model.DirectionOut, RawBytes: 300, Bytes: 300},
	})
	if err != nil {
		t.Fatalf("写入流量失败: %v", err)
	}

	var rule model.ForwardRule
	if err := app.DB.First(&rule, 1).Error; err != nil {
		t.Fatalf("读取规则失败: %v", err)
	}
	if rule.TrafficIn != 100 {
		t.Errorf("规则入口累计 = %d，期望 100", rule.TrafficIn)
	}
	if rule.TrafficOut != 300 {
		t.Errorf("规则出口累计 = %d，期望 300", rule.TrafficOut)
	}

	var user model.User
	if err := app.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if user.TrafficUsed != 400 {
		t.Errorf("用户已用流量 = %d，期望 400（入口 + 出口）", user.TrafficUsed)
	}
}

// TestTrafficRecordNormalizesSamples 验证非法样本被归一化而不是制造脏数据。
func TestTrafficRecordNormalizesSamples(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	err := svc.Record(ctx, []TrafficSample{
		// 方向非法 → in；字节为负 → 0；只填了 RawBytes → Bytes 补齐。
		{Hour: 0, RuleID: 1, Direction: "sideways", RawBytes: 700},
		// 全零 → 不落库。
		{Hour: 0, RuleID: 2, Direction: model.DirectionIn},
		// 小时越界 → 归为天行。
		{Hour: 99, RuleID: 3, Direction: model.DirectionOut, RawBytes: 50, Bytes: 50},
	})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	var rows []model.TrafficLog
	if err := app.DB.Order("rule_id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("行数 = %d，期望 3（零增量的规则 2 不应落库）", len(rows))
	}

	if rows[0].Direction != model.DirectionIn {
		t.Errorf("非法方向应归一化为 in，实际 %q", rows[0].Direction)
	}
	if rows[0].Bytes != 700 {
		t.Errorf("缺省折算字节应回退为原始字节，实际 %d", rows[0].Bytes)
	}
	// 规则 3 的小时越界 → 天行。
	var rule3 model.TrafficLog
	app.DB.Where("rule_id = ?", 3).First(&rule3)
	if rule3.Hour != model.HourDaily {
		t.Errorf("越界小时应归为天行，实际 hour=%d", rule3.Hour)
	}
}

// TestTrafficOverviewReadsDailyRowsOnly 验证概览只读天行，不重复计数。
func TestTrafficOverviewReadsDailyRowsOnly(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	hour := timeNow().Hour()
	if err := svc.Record(ctx, []TrafficSample{{
		Hour: hour, RuleID: 1, UserID: 1, NodeID: 1,
		Direction: model.DirectionIn, RawBytes: 1000, Bytes: 2000,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	ov, err := svc.Overview(ctx, BytesModeScaled)
	if err != nil {
		t.Fatalf("概览失败: %v", err)
	}
	// 若误把小时行也统计进来，这里会是 4000。
	if ov.Today.Total != 2000 {
		t.Errorf("今日总量 = %d，期望 2000（只统计天行，否则会翻倍）", ov.Today.Total)
	}
	if ov.Total.Total != 2000 {
		t.Errorf("累计总量 = %d，期望 2000", ov.Total.Total)
	}
	if ov.OnlineNodes != 0 {
		t.Errorf("在线节点数 = %d，期望 0", ov.OnlineNodes)
	}
	if ov.BytesMode != BytesModeScaled {
		t.Errorf("展示口径 = %q，期望 scaled", ov.BytesMode)
	}
}

// TestTrafficOverviewBytesModeSwitching 验证可在折算值与原始值之间切换（规格书 6.10 统计口径）。
func TestTrafficOverviewBytesModeSwitching(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	hour := timeNow().Hour()
	if err := svc.Record(ctx, []TrafficSample{{
		Hour: hour, RuleID: 1, Direction: model.DirectionIn,
		RawBytes: 1000, Bytes: 3500, // 倍率 3.5
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	scaled, err := svc.Overview(ctx, BytesModeScaled)
	if err != nil {
		t.Fatalf("折算口径统计失败: %v", err)
	}
	if scaled.Today.Total != 3500 {
		t.Errorf("折算口径今日 = %d，期望 3500", scaled.Today.Total)
	}

	raw, err := svc.Overview(ctx, BytesModeRaw)
	if err != nil {
		t.Fatalf("原始口径统计失败: %v", err)
	}
	if raw.Today.Total != 1000 {
		t.Errorf("原始口径今日 = %d，期望 1000", raw.Today.Total)
	}

	// 空值应回退为默认的折算口径。
	def, err := svc.Overview(ctx, "")
	if err != nil {
		t.Fatalf("默认口径统计失败: %v", err)
	}
	if def.BytesMode != BytesModeScaled || def.Today.Total != 3500 {
		t.Errorf("空口径应回退为 scaled，实际 mode=%q total=%d", def.BytesMode, def.Today.Total)
	}
}

// TestTrafficParseBytesMode 验证展示口径参数的解析。
func TestTrafficParseBytesMode(t *testing.T) {
	cases := map[string]BytesMode{
		"":          BytesModeScaled,
		"scaled":    BytesModeScaled,
		"raw":       BytesModeRaw,
		"raw_bytes": BytesModeRaw,
		"随便写的":      BytesModeScaled,
	}
	for in, want := range cases {
		if got := ParseBytesMode(in); got != want {
			t.Errorf("ParseBytesMode(%q) = %q，期望 %q", in, got, want)
		}
	}
	if BytesModeScaled.Column() != "bytes" || BytesModeRaw.Column() != "raw_bytes" {
		t.Error("展示口径对应的列名不正确")
	}
}

// TestTrafficTimeseriesHourBuckets 验证按小时的时间序列分桶（规格书 8.12）。
func TestTrafficTimeseriesHourBuckets(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	err := svc.Record(ctx, []TrafficSample{
		{Hour: 1, RuleID: 1, Direction: model.DirectionIn, RawBytes: 100, Bytes: 100},
		{Hour: 2, RuleID: 1, Direction: model.DirectionIn, RawBytes: 200, Bytes: 200},
		{Hour: 2, RuleID: 1, Direction: model.DirectionOut, RawBytes: 300, Bytes: 300},
	})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	now := timeNow()
	from := now.Add(-48 * time.Hour)
	to := now.Add(48 * time.Hour)

	series, err := svc.Timeseries(ctx, from, to, HourBucket, GroupByDirection, BytesModeScaled)
	if err != nil {
		t.Fatalf("时间序列失败: %v", err)
	}
	if len(series) != 3 {
		t.Fatalf("分桶数 = %d，期望 3（1 点入口、2 点入口、2 点出口）", len(series))
	}

	// 桶起点必须是整点。
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	for _, p := range series {
		if (p.Unix-dayStart)%3600 != 0 {
			t.Errorf("桶起点 %d 未对齐到整点", p.Unix)
		}
	}

	// 2 点的那两个桶应当分别对应 in 与 out。
	var inSum, outSum int64
	for _, p := range series {
		switch p.Group {
		case model.DirectionIn:
			inSum += p.Total
		case model.DirectionOut:
			outSum += p.Total
		}
	}
	if inSum != 300 {
		t.Errorf("入口总量 = %d，期望 300", inSum)
	}
	if outSum != 300 {
		t.Errorf("出口总量 = %d，期望 300", outSum)
	}
}

// TestTrafficTimeseriesDayBuckets 验证按天粒度只读天行。
func TestTrafficTimeseriesDayBuckets(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := svc.Record(ctx, []TrafficSample{{
		Hour: timeNow().Hour(), RuleID: 1, Direction: model.DirectionIn,
		RawBytes: 111, Bytes: 111,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	now := timeNow()
	series, err := svc.Timeseries(ctx,
		now.Add(-24*time.Hour), now.Add(24*time.Hour), DayBucket, GroupByDirection, BytesModeScaled)
	if err != nil {
		t.Fatalf("时间序列失败: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("天粒度分桶数 = %d，期望 1", len(series))
	}
	// 若误读了小时行，这里会变成 222（小时行 + 天行）。
	if series[0].Total != 111 {
		t.Errorf("天桶总量 = %d，期望 111（只读天行）", series[0].Total)
	}

	// 桶起点应当是当天 00:00。
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	if series[0].Unix != want {
		t.Errorf("天桶起点 = %d，期望 %d", series[0].Unix, want)
	}
}

// TestTrafficTimeseriesRejectsBadRange 验证非法时间区间被拒绝。
func TestTrafficTimeseriesRejectsBadRange(t *testing.T) {
	app := newLimitTestApp(t)
	svc := NewTrafficService(app)

	now := timeNow()
	if _, err := svc.Timeseries(context.Background(), now, now.Add(-time.Hour),
		HourBucket, GroupByDirection, BytesModeScaled); err == nil {
		t.Fatal("结束时间早于开始时间应当报错")
	}
}

// TestTrafficTimeseriesGroupByRuleNames 验证按规则分组时会解析出规则名。
func TestTrafficTimeseriesGroupByRuleNames(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.ForwardRule{
		Name: "香港入口", InboundGroupID: 1, ListenPort: 8443,
	}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	if err := svc.Record(ctx, []TrafficSample{{
		Hour: timeNow().Hour(), RuleID: 1, Direction: model.DirectionIn,
		RawBytes: 42, Bytes: 42,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	now := timeNow()
	series, err := svc.Timeseries(ctx,
		now.Add(-time.Hour), now.Add(time.Hour), HourBucket, GroupByRule, BytesModeScaled)
	if err != nil {
		t.Fatalf("时间序列失败: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("分桶数 = %d，期望 1", len(series))
	}
	if series[0].Group != "1" {
		t.Errorf("分组键 = %q，期望 \"1\"", series[0].Group)
	}
	if series[0].GroupName != "香港入口" {
		t.Errorf("分组名 = %q，期望「香港入口」", series[0].GroupName)
	}
}

// TestTrafficBucketUnix 验证时间桶的整数分桶计算（不依赖方言日期函数）。
func TestTrafficBucketUnix(t *testing.T) {
	base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Unix()

	if got := bucketUnix("2026-01-02", model.HourDaily, DayBucket); got != base {
		t.Errorf("天桶 = %d，期望 %d", got, base)
	}
	if got := bucketUnix("2026-01-02", 5, HourBucket); got != base+5*3600 {
		t.Errorf("5 点桶 = %d，期望 %d", got, base+5*3600)
	}
	// 小时行在按天粒度下也应归到当天 00:00（容错）。
	if got := bucketUnix("2026-01-02", 13, DayBucket); got != base {
		t.Errorf("按天粒度下小时行应归到当天 0 点，实际 %d，期望 %d", got, base)
	}
	// 非法日期返回 0 而不是 panic。
	if got := bucketUnix("not-a-date", 1, HourBucket); got != 0 {
		t.Errorf("非法日期应返回 0，实际 %d", got)
	}
}

// TestTrafficTopRanking 验证 Top-N 排行按总量降序并计算占比。
func TestTrafficTopRanking(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.ForwardRule{Name: "小流量", InboundGroupID: 1, ListenPort: 1}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	if err := app.DB.Create(&model.ForwardRule{Name: "大流量", InboundGroupID: 1, ListenPort: 2}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}

	hour := timeNow().Hour()
	if err := svc.Record(ctx, []TrafficSample{
		{Hour: hour, RuleID: 1, Direction: model.DirectionIn, RawBytes: 100, Bytes: 100},
		{Hour: hour, RuleID: 2, Direction: model.DirectionIn, RawBytes: 300, Bytes: 300},
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	list, err := svc.Top(ctx, DimensionRule, 10, time.Time{}, time.Time{}, BytesModeScaled)
	if err != nil {
		t.Fatalf("排行失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("排行条数 = %d，期望 2", len(list))
	}
	if list[0].ID != 2 || list[0].Total != 300 {
		t.Errorf("首位应为规则 2（300 字节），实际 id=%d total=%d", list[0].ID, list[0].Total)
	}
	if list[0].Name != "大流量" {
		t.Errorf("首位名称 = %q，期望「大流量」", list[0].Name)
	}
	// 占比：300 / 400 = 75%。
	if list[0].Percent < 74.9 || list[0].Percent > 75.1 {
		t.Errorf("首位占比 = %.2f，期望 75", list[0].Percent)
	}

	// limit 生效。
	limited, err := svc.Top(ctx, DimensionRule, 1, time.Time{}, time.Time{}, BytesModeScaled)
	if err != nil {
		t.Fatalf("排行失败: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("limit=1 时应只返回 1 条，实际 %d", len(limited))
	}
}

// TestTrafficRuleBreakdownHourly 验证规则级拆解给出今日 24 小时曲线（规格书 6.10 规则级）。
func TestTrafficRuleBreakdownHourly(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.ForwardRule{
		Name: "r1", InboundGroupID: 1, ListenPort: 8443,
		InboundMultiplier: 2, OutboundMultiplier: 1,
	}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}

	hour := timeNow().Hour()
	if err := svc.Record(ctx, []TrafficSample{{
		Hour: hour, RuleID: 1, Direction: model.DirectionIn, RawBytes: 100, Bytes: 200,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	br, err := svc.RuleBreakdown(ctx, 1, BytesModeScaled)
	if err != nil {
		t.Fatalf("规则拆解失败: %v", err)
	}
	if len(br.Hourly) != 24 {
		t.Fatalf("小时曲线长度 = %d，期望固定 24", len(br.Hourly))
	}
	if br.Hourly[hour] != 200 {
		t.Errorf("第 %d 小时 = %d，期望 200", hour, br.Hourly[hour])
	}
	if br.Today.Total != 200 {
		t.Errorf("今日总量 = %d，期望 200", br.Today.Total)
	}
	if br.LifetimeIn != 200 {
		t.Errorf("规则累计入口 = %d，期望 200", br.LifetimeIn)
	}
	if br.Name != "r1" || br.InboundMult != 2 {
		t.Errorf("规则元信息不符: name=%q mult=%v", br.Name, br.InboundMult)
	}

	// 不存在的规则返回 404 语义的错误。
	if _, err := svc.RuleBreakdown(ctx, 999, BytesModeScaled); err == nil {
		t.Fatal("不存在的规则应当报错")
	}
}

// TestTrafficUserBreakdownRemaining 验证用户级拆解给出剩余流量与百分比（规格书 6.10 用户级）。
func TestTrafficUserBreakdownRemaining(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.User{
		Username: "alice", PasswordHash: "x", Status: model.StatusEnabled,
		Token: "tok-alice", TrafficLimit: 1000, TrafficUsed: 250,
	}).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}

	br, err := svc.UserBreakdown(ctx, 1, BytesModeScaled)
	if err != nil {
		t.Fatalf("用户拆解失败: %v", err)
	}
	if br.Remaining != 750 {
		t.Errorf("剩余流量 = %d，期望 750", br.Remaining)
	}
	if br.Percent < 24.9 || br.Percent > 25.1 {
		t.Errorf("使用百分比 = %.2f，期望 25", br.Percent)
	}

	// 未设上限时剩余应为 -1（前端据此显示「不限」）。
	if err := app.DB.Create(&model.User{
		Username: "bob", PasswordHash: "x", Status: model.StatusEnabled,
		Token: "tok-bob", TrafficLimit: 0,
	}).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	br2, err := svc.UserBreakdown(ctx, 2, BytesModeScaled)
	if err != nil {
		t.Fatalf("用户拆解失败: %v", err)
	}
	if br2.Remaining != -1 {
		t.Errorf("未设上限时剩余应为 -1（不限），实际 %d", br2.Remaining)
	}
	if br2.Percent != 0 {
		t.Errorf("未设上限时百分比应为 0，实际 %v", br2.Percent)
	}
}

// TestTrafficExportCSV 验证 CSV 导出（规格书 6.10：支持导出 CSV）。
func TestTrafficExportCSV(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.ForwardRule{Name: "规则A", InboundGroupID: 1, ListenPort: 1}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	if err := app.DB.Create(&model.User{
		Username: "alice", PasswordHash: "x", Status: model.StatusEnabled, Token: "t1",
	}).Error; err != nil {
		t.Fatalf("写入用户失败: %v", err)
	}
	if err := svc.Record(ctx, []TrafficSample{{
		Hour: timeNow().Hour(), RuleID: 1, UserID: 1, Direction: model.DirectionIn,
		RawBytes: 123, Bytes: 456,
	}}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	var buf bytes.Buffer
	if err := svc.ExportCSV(ctx, TrafficFilter{}, &buf); err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	out := buf.Bytes()
	// 必须带 UTF-8 BOM（EF BB BF），否则 Excel 打开中文会乱码。
	if len(out) < 3 || out[0] != 0xEF || out[1] != 0xBB || out[2] != 0xBF {
		t.Error("CSV 缺少 UTF-8 BOM，Excel 打开中文会乱码")
	}
	body := string(out)
	for _, want := range []string{"日期", "实际字节", "折算字节", "规则A", "alice", "123", "456"} {
		if !strings.Contains(body, want) {
			t.Errorf("CSV 缺少内容 %q", want)
		}
	}
	// 天行的小时应渲染为「全天」而不是 -1。
	if !strings.Contains(body, "全天") {
		t.Error("按天聚合行的小时应显示为「全天」")
	}
}

// TestTrafficQueryFilter 验证明细筛选按用户 / 规则 / 方向组合生效（规格书 6.10 任意组合筛选）。
func TestTrafficQueryFilter(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	hour := timeNow().Hour()
	if err := svc.Record(ctx, []TrafficSample{
		{Hour: hour, RuleID: 1, UserID: 1, Direction: model.DirectionIn, RawBytes: 10, Bytes: 10},
		{Hour: hour, RuleID: 1, UserID: 2, Direction: model.DirectionIn, RawBytes: 20, Bytes: 20},
		{Hour: hour, RuleID: 2, UserID: 1, Direction: model.DirectionOut, RawBytes: 30, Bytes: 30},
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	items, total, err := svc.Query(ctx, TrafficFilter{UserID: 1}, BytesModeScaled)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 用户 1 有两条天行记录（规则 1 的入口与规则 2 的出口）。
	if total != 2 || len(items) != 2 {
		t.Errorf("按用户筛选应得到 2 条，实际 total=%d len=%d", total, len(items))
	}

	items, total, err = svc.Query(ctx, TrafficFilter{RuleID: 1, Direction: model.DirectionIn}, BytesModeScaled)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 规则 1 的入口天行只有一条（用户 1 与用户 2 会合成两条不同的行，因为 user_id 参与唯一键）。
	if total != 2 || len(items) != 2 {
		t.Errorf("按规则 + 方向筛选应得到 2 条，实际 total=%d len=%d", total, len(items))
	}

	// 小时口径查询：默认是 day，显式传 hour 才能看到小时行。
	items, total, err = svc.Query(ctx, TrafficFilter{Interval: HourBucket}, BytesModeScaled)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Errorf("按小时口径应得到 3 条，实际 total=%d len=%d", total, len(items))
	}
}

// TestTrafficDashboardAggregates 验证首页仪表盘一次拿全（规格书 8.12 /traffic/dashboard）。
func TestTrafficDashboardAggregates(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	if err := app.DB.Create(&model.ForwardRule{
		Name: "坏规则", InboundGroupID: 1, ListenPort: 1,
		SyncStatus: model.SyncFailed, SyncError: "节点拒绝",
	}).Error; err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	if err := app.DB.Create(&model.Node{
		Name: "离线节点", Token: "nt1", Role: model.RoleBoth, Online: false,
	}).Error; err != nil {
		t.Fatalf("写入节点失败: %v", err)
	}
	if err := app.DB.Create(&model.AlertHistory{
		RuleID: 1, Level: model.AlertLevelCritical, Title: "节点离线",
		FiredAt: timeNow(),
	}).Error; err != nil {
		t.Fatalf("写入告警失败: %v", err)
	}

	data, err := svc.Dashboard(ctx, BytesModeScaled)
	if err != nil {
		t.Fatalf("仪表盘失败: %v", err)
	}
	if data.Overview == nil {
		t.Fatal("仪表盘应带上全站概览")
	}
	if len(data.SyncFailedRules) != 1 {
		t.Errorf("同步失败规则数 = %d，期望 1", len(data.SyncFailedRules))
	}
	if len(data.OfflineNodes) != 1 {
		t.Errorf("离线节点数 = %d，期望 1", len(data.OfflineNodes))
	}
	if len(data.ActiveAlerts) != 1 {
		t.Errorf("活跃告警数 = %d，期望 1", len(data.ActiveAlerts))
	}
	if data.PendingIssues != 3 {
		t.Errorf("待处理事项 = %d，期望 3", data.PendingIssues)
	}
	// 空列表必须序列化为 []，不能是 null。
	if data.TopRules == nil || data.TopUsers == nil || data.TopNodes == nil || data.Trend == nil {
		t.Error("仪表盘的空列表必须是空切片而不是 nil（否则前端会渲染成 null）")
	}
}

// TestTrafficSessionLifecycle 验证会话的建立、刷新与关闭（规格书 4.2.10）。
func TestTrafficSessionLifecycle(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	in := SessionInput{
		UserID: 1, RuleID: 2, NodeID: 3,
		ClientIP: "1.2.3.4", DeviceID: "dev-1", Protocol: "tls",
	}

	id, err := svc.OpenSession(ctx, in)
	if err != nil {
		t.Fatalf("建立会话失败: %v", err)
	}
	if id == 0 {
		t.Fatal("建立会话应返回非零 ID")
	}
	if got := svc.ActiveConnections(); got != 1 {
		t.Errorf("活跃连接数 = %d，期望 1", got)
	}

	// 同一客户端再次建连：累加连接数而不是新开一行。
	if _, err := svc.OpenSession(ctx, in); err != nil {
		t.Fatalf("第二次建立会话失败: %v", err)
	}
	if got := svc.ActiveConnections(); got != 2 {
		t.Errorf("活跃连接数 = %d，期望 2", got)
	}
	var sessCount int64
	app.DB.Model(&model.Session{}).Count(&sessCount)
	if sessCount != 1 {
		t.Errorf("会话行数 = %d，期望 1（同一客户端应合并成一行）", sessCount)
	}

	// 在线用户统计。
	online, err := svc.OnlineUsers(ctx, 0)
	if err != nil {
		t.Fatalf("统计在线用户失败: %v", err)
	}
	if online != 1 {
		t.Errorf("在线用户数 = %d，期望 1", online)
	}
	if one, _ := svc.OnlineUsers(ctx, 1); one != 1 {
		t.Errorf("用户 1 应在线，实际 %d", one)
	}

	// 按规则查会话。
	ruleSess, err := svc.RuleSessions(ctx, 2)
	if err != nil {
		t.Fatalf("查询规则会话失败: %v", err)
	}
	if len(ruleSess) != 1 {
		t.Fatalf("规则会话数 = %d，期望 1", len(ruleSess))
	}
	if ruleSess[0].ClientIP != "1.2.3.4" {
		t.Errorf("会话客户端 IP = %q", ruleSess[0].ClientIP)
	}
	// IP 哈希必须落库（用于匿名化统计），且不等于明文。
	if ruleSess[0].ClientIPHash == "" || ruleSess[0].ClientIPHash == "1.2.3.4" {
		t.Error("ClientIPHash 应当被写入且不等于明文 IP")
	}

	// 关闭两条连接后会话从热态移除。
	if err := svc.CloseSession(ctx, 2, "1.2.3.4", "dev-1", "tls"); err != nil {
		t.Fatalf("关闭会话失败: %v", err)
	}
	if err := svc.CloseSession(ctx, 2, "1.2.3.4", "dev-1", "tls"); err != nil {
		t.Fatalf("关闭会话失败: %v", err)
	}
	if got := svc.ActiveConnections(); got != 0 {
		t.Errorf("全部连接关闭后活跃连接数 = %d，期望 0", got)
	}
	// 数据库行保留（供历史统计）。
	app.DB.Model(&model.Session{}).Count(&sessCount)
	if sessCount != 1 {
		t.Errorf("关闭连接不应删除会话行，实际行数 = %d", sessCount)
	}
}

// TestTrafficOpenSessionRequiresIP 验证缺少客户端 IP 时拒绝建立会话。
func TestTrafficOpenSessionRequiresIP(t *testing.T) {
	app := newLimitTestApp(t)
	svc := NewTrafficService(app)
	if _, err := svc.OpenSession(context.Background(), SessionInput{RuleID: 1}); err == nil {
		t.Fatal("缺少客户端 IP 时应当报错")
	}
}

// TestTrafficAccountConnections 验证按规则 / 用户统计活跃连接数（连接数限制的依据）。
func TestTrafficAccountConnections(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	for _, in := range []SessionInput{
		{UserID: 1, RuleID: 1, ClientIP: "1.1.1.1", Protocol: "tls"},
		{UserID: 1, RuleID: 1, ClientIP: "2.2.2.2", Protocol: "tls"},
		{UserID: 2, RuleID: 2, ClientIP: "3.3.3.3", Protocol: "tls"},
	} {
		if _, err := svc.OpenSession(ctx, in); err != nil {
			t.Fatalf("建立会话失败: %v", err)
		}
	}

	if got := svc.AccountConnections(1, 0); got != 2 {
		t.Errorf("规则 1 的连接数 = %d，期望 2", got)
	}
	if got := svc.AccountConnections(0, 1); got != 2 {
		t.Errorf("用户 1 的连接数 = %d，期望 2", got)
	}
	if got := svc.AccountConnections(0, 0); got != 3 {
		t.Errorf("全站连接数 = %d，期望 3", got)
	}
}

// TestTrafficCleanupSessionsRemovesIdle 验证垃圾会话清理（规格书 4.2.10 的容量控制）。
func TestTrafficCleanupSessionsRemovesIdle(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	// 一条很久以前、连接数已归零的会话。
	//
	// ConnCount 用取地址的方式显式写入 0：模型层是指针类型，
	// 这样「连接数归零」才能真正落库（见 model.Session.ConnCount 的说明）。
	old := timeNow().Add(-2 * sessionIdleTimeout)
	zero := 0
	if err := app.DB.Create(&model.Session{
		UserID: 1, RuleID: 1, ClientIP: "9.9.9.9", ConnCount: &zero,
		Protocol: "tls", StartedAt: old, LastSeen: old,
	}).Error; err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
	// 一条活跃会话，不应被清理。
	one := 1
	if err := app.DB.Create(&model.Session{
		UserID: 1, RuleID: 1, ClientIP: "8.8.8.8", ConnCount: &one,
		Protocol: "tls", StartedAt: timeNow(), LastSeen: timeNow(),
	}).Error; err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}

	deleted, err := svc.CleanupSessions(ctx)
	if err != nil {
		t.Fatalf("清理会话失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("清理行数 = %d，期望 1（只清掉空闲且连接归零的那条）", deleted)
	}

	var remain int64
	app.DB.Model(&model.Session{}).Count(&remain)
	if remain != 1 {
		t.Errorf("剩余会话数 = %d，期望 1", remain)
	}
}

// TestTrafficCleanupKeepsDailyRows 验证流量清理只删小时行，天行永久保留。
//
// 这是规格书 6.10 的硬要求：小时保留 30 天，天永久保留。
func TestTrafficCleanupKeepsDailyRows(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	ancient := timeNow().AddDate(0, 0, -400).Format("2006-01-02")
	rows := []model.TrafficLog{
		// 很久以前的小时行：应被删除。
		{Date: ancient, Hour: 3, RuleID: 1, Direction: model.DirectionIn, RawBytes: 1, Bytes: 1},
		// 很久以前的天行：必须保留。
		{Date: ancient, Hour: model.HourDaily, RuleID: 1, Direction: model.DirectionIn, RawBytes: 1, Bytes: 1},
		// 今天的小时行：不需要删除。
		{Date: timeNow().Format("2006-01-02"), Hour: 1, RuleID: 1, Direction: model.DirectionIn, RawBytes: 1, Bytes: 1},
	}
	for i := range rows {
		if err := app.DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("写入流量失败: %v", err)
		}
	}

	deleted, err := svc.CleanupTraffic(ctx)
	if err != nil {
		t.Fatalf("清理流量失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("清理行数 = %d，期望 1", deleted)
	}

	var daily int64
	app.DB.Model(&model.TrafficLog{}).Where("hour = ?", model.HourDaily).Count(&daily)
	if daily != 1 {
		t.Errorf("天行数量 = %d，期望 1（天行永久保留，绝不能被清理）", daily)
	}
}

// TestTrafficRebuildSessionsFromDatabase 验证进程重启后能从库中重建会话热态。
func TestTrafficRebuildSessionsFromDatabase(t *testing.T) {
	app := newLimitTestApp(t)
	ctx := context.Background()
	svc := NewTrafficService(app)

	// ConnCount 是指针类型，测试里需要取地址才能指定连接数。
	conn2 := 2
	if err := app.DB.Create(&model.Session{
		UserID: 1, RuleID: 1, ClientIP: "1.1.1.1", ConnCount: &conn2,
		Protocol: "ws", StartedAt: timeNow(), LastSeen: timeNow(),
	}).Error; err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}
	// 一条早已失效的会话，重建时不应被载入。
	old := timeNow().Add(-2 * sessionIdleTimeout)
	conn5 := 5
	if err := app.DB.Create(&model.Session{
		UserID: 1, RuleID: 1, ClientIP: "2.2.2.2", ConnCount: &conn5,
		Protocol: "ws", StartedAt: old, LastSeen: old,
	}).Error; err != nil {
		t.Fatalf("写入会话失败: %v", err)
	}

	if err := svc.RebuildSessions(ctx); err != nil {
		t.Fatalf("重建会话失败: %v", err)
	}
	if got := svc.ActiveConnections(); got != 2 {
		t.Errorf("重建后活跃连接数 = %d，期望 2（过期的会话不应载入）", got)
	}
}

// TestTrafficNormalizeIntervalAndGroupBy 验证时间粒度与分组维度的参数归一化。
func TestTrafficNormalizeIntervalAndGroupBy(t *testing.T) {
	if normalizeInterval("day") != DayBucket || normalizeInterval("hour") != HourBucket {
		t.Error("合法粒度不应被改写")
	}
	if normalizeInterval("week") != HourBucket {
		t.Errorf("非法粒度应回退为 hour，实际 %q", normalizeInterval("week"))
	}
	if normalizeGroupBy("user") != GroupByUser || normalizeGroupBy("rule") != GroupByRule ||
		normalizeGroupBy("node") != GroupByNode {
		t.Error("合法分组维度不应被改写")
	}
	if normalizeGroupBy("") != GroupByDirection || normalizeGroupBy("nonsense") != GroupByDirection {
		t.Error("非法分组维度应回退为 direction")
	}
}

// TestTrafficSortRankItemsDeterministic 验证排行排序是确定性的（总量降序、ID 升序）。
func TestTrafficSortRankItemsDeterministic(t *testing.T) {
	list := []TrafficRankItem{
		{ID: 3, Total: 100},
		{ID: 1, Total: 100},
		{ID: 2, Total: 500},
		{ID: 4, Total: 100},
	}
	sortRankItems(list)

	want := []uint64{2, 1, 3, 4}
	for i, w := range want {
		if list[i].ID != w {
			t.Fatalf("排序结果 = %v，期望 ID 顺序 %v", idsOf(list), want)
		}
	}
}

// idsOf 提取排行项的 ID，便于断言顺序。
func idsOf(list []TrafficRankItem) []uint64 {
	out := make([]uint64, 0, len(list))
	for _, it := range list {
		out = append(out, it.ID)
	}
	return out
}
