package nodeclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

const maxTrafficIncrement int64 = 1 << 50
const maxReportTrafficRows = 10000

// Buckets preserve the time and effective configuration that produced traffic.
type pendingTrafficBucket struct {
	ConfigVersion int64                                `json:"config_version"`
	Timestamp     int64                                `json:"timestamp"`
	Rows          map[string]nodeproto.RuleTrafficItem `json:"rows"`
}

// Outbox is immutable until acknowledged. Pending is read-only legacy state;
// new journals record Buckets, including hour and configuration attribution.
type reportJournal struct {
	Format   int                                  `json:"format,omitempty"`
	Identity string                               `json:"identity"`
	Outbox   *nodeproto.ReportRequest             `json:"outbox,omitempty"`
	Buckets  map[string]*pendingTrafficBucket     `json:"buckets,omitempty"`
	Pending  map[string]nodeproto.RuleTrafficItem `json:"pending,omitempty"`
}

func newID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("cryptographic random source unavailable")
	}
	return hex.EncodeToString(data[:])
}

func reportTrafficKey(row nodeproto.RuleTrafficItem) string {
	return fmt.Sprintf("%d/%s", row.RuleID, row.Direction)
}
func trafficHour(timestamp int64) int64 {
	return time.Unix(timestamp, 0).UTC().Truncate(time.Hour).Unix()
}

func (c *Client) loadReports() error {
	path := filepath.Join(c.config.DataDir, "report-outbox.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved reportJournal
	if json.Unmarshal(data, &saved) != nil {
		return errors.New("report journal is invalid")
	}
	if saved.Identity != c.identity() {
		return os.Rename(path, filepath.Join(c.config.DataDir, "report-outbox.previous-node-"+newID()+".json"))
	}
	if saved.Format > 2 {
		return errors.New("report journal uses an unsupported format")
	}
	if saved.Outbox != nil && (len(saved.Outbox.BatchID) != 32 || saved.Outbox.Stats == nil) {
		return errors.New("report journal contains an invalid in-flight batch")
	}
	c.outbox = saved.Outbox
	if saved.Buckets != nil {
		c.pendingTraffic = saved.Buckets
	}
	for _, bucket := range c.pendingTraffic {
		if bucket == nil || bucket.Timestamp <= 0 || bucket.ConfigVersion < 0 || len(bucket.Rows) > maxReportTrafficRows {
			return errors.New("report journal contains an invalid traffic bucket")
		}
		for key, row := range bucket.Rows {
			if key != reportTrafficKey(row) || row.TrafficIn < 0 || row.TrafficOut < 0 || row.TrafficIn > maxTrafficIncrement || row.TrafficOut > maxTrafficIncrement {
				return errors.New("report journal contains an invalid traffic increment")
			}
		}
	}
	if len(saved.Pending) > 0 {
		// Exact historical attribution was absent from the old format. Retain the
		// best saved evidence and freeze it once, never at reconnect time.
		version, timestamp := int64(0), c.now().Unix()
		if info, err := os.Stat(path); err == nil {
			timestamp = info.ModTime().Unix()
		}
		var prior savedConfig
		if data, err := os.ReadFile(filepath.Join(c.config.DataDir, "config.json")); err == nil && json.Unmarshal(data, &prior) == nil && prior.Identity == c.identity() {
			version = prior.Config.ConfigVersion
		}
		if saved.Outbox != nil {
			version = saved.Outbox.ConfigVersion
			if saved.Outbox.Timestamp > 0 {
				timestamp = saved.Outbox.Timestamp
			}
		}
		for _, row := range saved.Pending {
			if row.TrafficIn < 0 || row.TrafficOut < 0 {
				return errors.New("legacy report journal contains a negative increment")
			}
			c.addTrafficSample(TrafficSample{ConfigVersion: version, Timestamp: timestamp, Item: row})
		}
		c.log.Printf("migrated legacy pending traffic using saved configuration/time metadata; the old journal did not retain exact attribution")
		return c.saveReports()
	}
	return nil
}

func (c *Client) saveReports() error {
	data, err := json.Marshal(reportJournal{Format: 2, Identity: c.identity(), Outbox: c.outbox, Buckets: c.pendingTraffic})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(c.config.DataDir, "report-outbox.json"), data)
}

func (c *Client) addTrafficSample(sample TrafficSample) {
	row := sample.Item
	if row.TrafficIn == 0 && row.TrafficOut == 0 {
		return
	}
	if row.TrafficIn < 0 || row.TrafficOut < 0 {
		c.log.Printf("ignored invalid negative traffic sample for rule %d", row.RuleID)
		return
	}
	when := sample.Timestamp
	if when <= 0 {
		when = c.now().Unix()
	}
	when = trafficHour(when)
	base := fmt.Sprintf("%d/%d", when, sample.ConfigVersion)
	key := reportTrafficKey(row)
	for row.TrafficIn > 0 || row.TrafficOut > 0 {
		bucket := c.pendingTraffic[base]
		if bucket == nil {
			bucket = &pendingTrafficBucket{ConfigVersion: sample.ConfigVersion, Timestamp: when, Rows: make(map[string]nodeproto.RuleTrafficItem)}
			c.pendingTraffic[base] = bucket
		}
		pending, exists := bucket.Rows[key]
		if !exists && len(bucket.Rows) >= maxReportTrafficRows || pending.TrafficIn == maxTrafficIncrement && row.TrafficIn > 0 || pending.TrafficOut == maxTrafficIncrement && row.TrafficOut > 0 {
			c.pendingTraffic[base+"/"+newID()] = bucket
			delete(c.pendingTraffic, base)
			continue
		}
		in, out := min(row.TrafficIn, maxTrafficIncrement-pending.TrafficIn), min(row.TrafficOut, maxTrafficIncrement-pending.TrafficOut)
		pending.RuleID, pending.Direction = row.RuleID, row.Direction
		pending.TrafficIn += in
		pending.TrafficOut += out
		bucket.Rows[key] = pending
		row.TrafficIn -= in
		row.TrafficOut -= out
	}
}

func (c *Client) captureTraffic() nodeproto.ReportStats {
	var stats nodeproto.ReportStats
	if collector, ok := c.engine.(interface {
		CollectTraffic() (nodeproto.ReportStats, []TrafficSample)
	}); ok {
		var samples []TrafficSample
		stats, samples = collector.CollectTraffic()
		for _, sample := range samples {
			c.addTrafficSample(sample)
		}
	} else {
		stats = c.engine.Stats()
		when, version := c.now().Unix(), c.effectiveConfig().ConfigVersion
		for _, row := range stats.RuleTraffic {
			c.addTrafficSample(TrafficSample{ConfigVersion: version, Timestamp: when, Item: row})
		}
	}
	stats.RuleTraffic = nil
	return stats
}

func (c *Client) checkpointTraffic() {
	c.captureTraffic()
	if err := c.saveReports(); err != nil {
		c.log.Printf("persist sampled traffic: %s", c.safe(err))
	}
}

func (c *Client) nextTrafficBucket() (string, *pendingTrafficBucket) {
	keys := make([]string, 0, len(c.pendingTraffic))
	for key := range c.pendingTraffic {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := c.pendingTraffic[keys[i]], c.pendingTraffic[keys[j]]
		if a.Timestamp != b.Timestamp {
			return a.Timestamp < b.Timestamp
		}
		if a.ConfigVersion != b.ConfigVersion {
			return a.ConfigVersion < b.ConfigVersion
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		bucket := c.pendingTraffic[key]
		// Current synchronization results cannot accompany old traffic versions.
		if len(c.results) > 0 && bucket.ConfigVersion != c.configVersion {
			continue
		}
		return key, bucket
	}
	return "", nil
}

func (c *Client) report(ctx context.Context) error {
	stats := c.captureTraffic()
	if c.outbox == nil {
		version, when := c.configVersion, c.now().Unix()
		key, bucket := c.nextTrafficBucket()
		if bucket != nil {
			version, when = bucket.ConfigVersion, bucket.Timestamp
			stats.RuleTraffic = make([]nodeproto.RuleTrafficItem, 0, len(bucket.Rows))
			for _, row := range bucket.Rows {
				stats.RuleTraffic = append(stats.RuleTraffic, row)
			}
			sort.Slice(stats.RuleTraffic, func(i, j int) bool {
				a, b := stats.RuleTraffic[i], stats.RuleTraffic[j]
				if a.RuleID != b.RuleID {
					return a.RuleID < b.RuleID
				}
				return a.Direction < b.Direction
			})
			delete(c.pendingTraffic, key)
		}
		stats.NetIn, stats.NetOut = c.lastMetrics.NetInSpeed, c.lastMetrics.NetOutSpeed
		c.outbox = &nodeproto.ReportRequest{BatchID: newID(), NodeID: c.nodeID, ConfigVersion: version, Stats: &stats, Timestamp: when}
		if version == c.configVersion {
			c.outbox.Results, c.outbox.Error = c.results, c.configError
			c.results = nil
		}
	}
	// Persist every bucket plus the frozen batch before any network request.
	if err := c.saveReports(); err != nil {
		return fmt.Errorf("persist report journal: %w", err)
	}
	var response nodeproto.ReportResponse
	if err := c.request(ctx, "POST", "/api/node/report", c.outbox, &response); err != nil {
		return err
	}
	if response.BatchID != c.outbox.BatchID || response.Accepted < len(c.outbox.Results) {
		return errors.New("panel did not acknowledge the complete report batch")
	}
	c.outbox = nil
	return c.saveReports()
}

func (c *Client) flushReports(ctx context.Context) error {
	for attempt := 0; attempt < 32; attempt++ {
		if err := c.report(ctx); err != nil {
			return err
		}
		if len(c.pendingTraffic) == 0 && len(c.results) == 0 {
			return nil
		}
	}
	return nil
}
