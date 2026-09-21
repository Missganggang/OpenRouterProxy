package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/model"
)

func TestTrafficMonthToDateIncludesTodayAndSameMonthYesterday(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewTrafficService(a)
	rows := []model.TrafficLog{
		{Date: "2026-08-31", Hour: model.HourDaily, Direction: "out", Bytes: 8, RawBytes: 4},
		{Date: "2026-09-01", Hour: model.HourDaily, Direction: "in", Bytes: 10, RawBytes: 5},
		{Date: "2026-09-20", Hour: model.HourDaily, Direction: "out", Bytes: 20, RawBytes: 10},
		{Date: "2026-09-21", Hour: model.HourDaily, Direction: "in", Bytes: 30, RawBytes: 15},
		{Date: "2026-09-22", Hour: model.HourDaily, Direction: "out", Bytes: 40, RawBytes: 20},
		{Date: "2026-09-30", Hour: model.HourDaily, Direction: "out", Bytes: 80, RawBytes: 40},
		{Date: "2026-10-01", Hour: model.HourDaily, Direction: "in", Bytes: 50, RawBytes: 25},
		{Date: "2026-10-02", Hour: model.HourDaily, Direction: "out", Bytes: 60, RawBytes: 30},
		{Date: "2026-12-31", Hour: model.HourDaily, Direction: "out", Bytes: 90, RawBytes: 45},
		{Date: "2027-01-01", Hour: model.HourDaily, Direction: "in", Bytes: 70, RawBytes: 35},
		// Hourly data must not be counted again in daily/monthly totals.
		{Date: "2026-09-22", Hour: 12, Direction: "out", Bytes: 40, RawBytes: 20},
	}
	if err := a.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, today                                   string
		todayBytes, yesterdayBytes, monthIn, monthOut int64
	}{
		{"middle of month", "2026-09-22", 40, 30, 40, 60},
		{"month boundary", "2026-10-01", 50, 80, 50, 0},
		{"year boundary", "2027-01-01", 70, 90, 70, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse("2006-01-02", tc.today)
			if err != nil {
				t.Fatal(err)
			}
			for _, col := range []string{"bytes", "raw_bytes"} {
				factor := int64(1)
				if col == "raw_bytes" {
					factor = 2
				}
				today, yesterday, month, total, err := s.periods(context.Background(), col, tc.today,
					now.AddDate(0, 0, -1).Format("2006-01-02"), now.Format("2006-01")+"-01")
				if err != nil {
					t.Fatal(err)
				}
				if today.Total != tc.todayBytes/factor || yesterday.Total != tc.yesterdayBytes/factor {
					t.Fatalf("%s day totals: today=%+v yesterday=%+v", col, today, yesterday)
				}
				if month.In != tc.monthIn/factor || month.Out != tc.monthOut/factor || month.Total != (tc.monthIn+tc.monthOut)/factor {
					t.Fatalf("%s month excludes current days or includes prior/future months: %+v", col, month)
				}
				if month.Raw != (tc.monthIn+tc.monthOut)/2 || total.Total != 458/factor || total.Raw != 229 {
					t.Fatalf("%s raw/daily-only totals: month=%+v total=%+v", col, month, total)
				}
			}
		})
	}
}

func TestTrafficAllBreakdownsIncludeTodayInMonth(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewTrafficService(a)
	u := model.User{Username: "month-user", Token: "month-user"}
	if err := a.DB.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	r := model.ForwardRule{Name: "month-rule", UserID: u.ID}
	if err := a.DB.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	rows := []model.TrafficLog{
		{Date: timeNow().Format("2006-01-02"), Hour: model.HourDaily, UserID: u.ID, RuleID: r.ID, Direction: "in", Bytes: 100, RawBytes: 50},
		{Date: timeNow().Format("2006-01-02"), Hour: model.HourDaily, UserID: u.ID + 1, RuleID: r.ID + 1, Direction: "out", Bytes: 700, RawBytes: 350},
	}
	if err := a.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	overview, err := s.Overview(ctx, BytesModeScaled)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.RuleBreakdown(ctx, r.ID, BytesModeScaled)
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.UserBreakdown(ctx, u.ID, BytesModeScaled)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Month.Total != 800 || overview.Month.Raw != 400 {
		t.Fatalf("overview month: %+v", overview.Month)
	}
	if rule.Month.Total != 100 || rule.Month.Raw != 50 {
		t.Fatalf("rule month or ownership: %+v", rule.Month)
	}
	if user.Month.Total != 100 || user.Month.Raw != 50 {
		t.Fatalf("user month or ownership: %+v", user.Month)
	}
}

func TestTrafficUserLimitInheritsGroupAndAllowsOverride(t *testing.T) {
	a := newLimitTestApp(t)
	s := NewTrafficService(a)
	group := model.UserGroup{Name: "capped", TrafficLimit: 1000}
	if err := a.DB.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name                                  string
		groupID                               uint64
		limit, used, wantLimit, wantRemaining int64
		wantPercent                           float64
	}{
		{"inherit", group.ID, 0, 250, 1000, 750, 25},
		{"explicit override", group.ID, 2000, 250, 2000, 1750, 12.5},
		{"inherited exhausted", group.ID, 0, 1250, 1000, 0, 125},
		{"no group", 0, 0, 250, 0, -1, 0},
		{"missing group", group.ID + 100, 0, 250, 0, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := model.User{Username: fmt.Sprintf("quota-%d", i), Token: fmt.Sprintf("quota-%d", i), GroupID: tc.groupID, TrafficLimit: tc.limit, TrafficUsed: tc.used}
			if err := a.DB.Create(&u).Error; err != nil {
				t.Fatal(err)
			}
			out, err := s.UserBreakdown(context.Background(), u.ID, BytesModeScaled)
			if err != nil {
				t.Fatal(err)
			}
			if out.Limit != tc.wantLimit || out.Remaining != tc.wantRemaining || out.Percent != tc.wantPercent || out.Used != tc.used {
				t.Fatalf("effective user limit: %+v", out)
			}
		})
	}
}
