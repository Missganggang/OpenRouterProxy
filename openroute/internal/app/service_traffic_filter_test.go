package app

import (
	"context"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/model"
)

func TestTrafficFiltersBeforeAggregation(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewTrafficService(a)
	rows := []model.TrafficLog{
		{Date: "2026-09-21", Hour: 23, UserID: 1, RuleID: 10, NodeID: 20, Direction: "out", Bytes: 20, RawBytes: 10},
		{Date: "2026-09-22", Hour: 0, UserID: 1, RuleID: 10, NodeID: 20, Direction: "out", Bytes: 200, RawBytes: 100},
		{Date: "2026-09-22", Hour: 0, UserID: 1, RuleID: 10, NodeID: 20, Direction: "in", Bytes: 50, RawBytes: 25},
		{Date: "2026-09-22", Hour: 0, UserID: 1, RuleID: 11, NodeID: 21, Direction: "out", Bytes: 900, RawBytes: 450},
		{Date: "2026-09-22", Hour: 0, UserID: 2, RuleID: 12, NodeID: 20, Direction: "out", Bytes: 9000, RawBytes: 4500},
		{Date: "2026-09-22", Hour: 1, UserID: 1, RuleID: 10, NodeID: 20, Direction: "out", Bytes: 800, RawBytes: 400},
		{Date: "2026-09-22", Hour: model.HourDaily, UserID: 1, RuleID: 10, NodeID: 20, Direction: "out", Bytes: 1000, RawBytes: 500},
	}
	for _, row := range rows {
		if err := a.DB.Model(&model.TrafficLog{}).Create(map[string]interface{}{
			"date": row.Date, "hour": row.Hour, "user_id": row.UserID, "rule_id": row.RuleID,
			"node_id": row.NodeID, "direction": row.Direction, "bytes": row.Bytes, "raw_bytes": row.RawBytes,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	from, _ := time.Parse(time.RFC3339, "2026-09-22T07:30:00+08:00")
	to, _ := time.Parse(time.RFC3339, "2026-09-22T08:30:00+08:00")
	filter := TrafficFilter{UserID: 1, NodeID: 20, Direction: "out", Interval: HourBucket}
	for _, group := range []string{GroupByDirection, GroupByUser, GroupByRule, GroupByNode} {
		series, err := s.TimeseriesFiltered(context.Background(), from, to, HourBucket, group, BytesModeScaled, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(series) != 2 || series[0].Total != 20 || series[1].Total != 200 || series[1].Raw != 100 {
			t.Fatalf("group %s lost filters or timezone: %+v", group, series)
		}
	}
	top, err := s.TopFiltered(context.Background(), DimensionRule, 10, from, to, BytesModeRaw, filter)
	if err != nil || len(top) != 1 || top[0].ID != 10 || top[0].Total != 110 || top[0].Percent != 100 {
		t.Fatalf("ranking does not match hourly filters: %+v, %v", top, err)
	}
	filter = TrafficFilter{RuleID: 10}
	series, err := s.TimeseriesFiltered(context.Background(), from, to, HourBucket, GroupByDirection, BytesModeScaled, filter)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, point := range series {
		total += point.Total
	}
	if total != 270 {
		t.Fatalf("rule detail includes another rule: %d", total)
	}
}
