package nodeclient

import (
	"github.com/openroute/openroute/internal/nodeproto"
	"sort"
	"time"
)

// TrafficSample freezes attribution when bytes are actually forwarded, before
// a later configuration change, disconnect, retry or wall-clock hour boundary.
type TrafficSample struct {
	ConfigVersion int64
	Timestamp     int64
	Item          nodeproto.RuleTrafficItem
}
type trafficSampleKey struct {
	ruleID        uint64
	direction     string
	version, hour int64
}

func (f *forwarder) recordTraffic(counter *ruleTraffic, upload bool, n int) {
	if n <= 0 {
		return
	}
	e := f.engine
	e.trafficMu.Lock()
	defer e.trafficMu.Unlock()
	if upload {
		counter.in.Add(int64(n))
		counter.pendingIn.Add(int64(n))
	} else {
		counter.out.Add(int64(n))
		counter.pendingOut.Add(int64(n))
	}
	direction := "inbound"
	if counter == f.outTraffic {
		direction = "outbound"
	}
	key := trafficSampleKey{f.rule.RuleID, direction, f.cfg.ConfigVersion, time.Now().UTC().Truncate(time.Hour).Unix()}
	if e.samples == nil {
		e.samples = map[trafficSampleKey]*nodeproto.RuleTrafficItem{}
	}
	item := e.samples[key]
	if item == nil {
		item = &nodeproto.RuleTrafficItem{RuleID: f.rule.RuleID, Direction: direction}
		e.samples[key] = item
	}
	if upload {
		item.TrafficIn += int64(n)
	} else {
		item.TrafficOut += int64(n)
	}
}

// CollectTraffic is the durable client's atomic sampling API. The returned
// summary excludes RuleTraffic because the samples provide its attribution.
// Stats remains available for legacy consumers; using either API drains both.
func (e *Engine) CollectTraffic() (nodeproto.ReportStats, []TrafficSample) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trafficMu.Lock()
	defer e.trafficMu.Unlock()
	samples := make([]TrafficSample, 0, len(e.samples))
	for key, item := range e.samples {
		samples = append(samples, TrafficSample{ConfigVersion: key.version, Timestamp: key.hour, Item: *item})
	}
	sort.Slice(samples, func(i, j int) bool {
		a, b := samples[i], samples[j]
		if a.Timestamp != b.Timestamp {
			return a.Timestamp < b.Timestamp
		}
		if a.ConfigVersion != b.ConfigVersion {
			return a.ConfigVersion < b.ConfigVersion
		}
		if a.Item.RuleID != b.Item.RuleID {
			return a.Item.RuleID < b.Item.RuleID
		}
		return a.Item.Direction < b.Item.Direction
	})
	stats := e.statsLocked()
	stats.RuleTraffic = nil
	return stats, samples
}
