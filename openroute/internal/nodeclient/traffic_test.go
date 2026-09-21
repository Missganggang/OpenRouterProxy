package nodeclient

import (
	"net"
	"testing"
	"time"
)

func TestTrafficSamplesKeepForwardingVersionAcrossApplyAndClose(t *testing.T) {
	e := NewEngine("127.0.0.1")
	cfg := directConfig(echoTarget(t))
	cfg.ConfigVersion = 11
	address := applyOK(t, e, cfg)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "old-version")
	conn.Close()
	cfg.ConfigVersion = 12
	cfg.Rules[0].TargetBalance = "round_robin"
	address = applyOK(t, e, cfg)
	conn, err = net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "new-version-data")
	conn.Close()
	e.Close()
	stats, samples := e.CollectTraffic()
	if len(samples) != 2 {
		t.Fatalf("samples=%+v", samples)
	}
	totals := map[int64]int64{}
	for _, s := range samples {
		if s.Item.Direction != "inbound" || s.Item.RuleID != 1 || s.Timestamp%3600 != 0 || time.Since(time.Unix(s.Timestamp, 0)) > 2*time.Hour {
			t.Fatalf("bad attribution: %+v", s)
		}
		totals[s.ConfigVersion] += s.Item.TrafficIn + s.Item.TrafficOut
	}
	if totals[11] != int64(len("old-version")*2) || totals[12] != int64(len("new-version-data")*2) {
		t.Fatalf("version totals=%v", totals)
	}
	if stats.NetIn+stats.NetOut != totals[11]+totals[12] || len(stats.RuleTraffic) != 0 {
		t.Fatalf("summary=%+v", stats)
	}
	if duplicate := e.Stats(); len(duplicate.RuleTraffic) != 0 {
		t.Fatalf("sample drain duplicated legacy deltas: %+v", duplicate)
	}
}
