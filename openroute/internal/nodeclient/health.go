package nodeclient

import (
	"github.com/openroute/openroute/internal/nodeproto"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type healthState struct {
	fails, successes int
	down             bool
	retry            time.Time
}

func (f *forwarder) exitAllowed(g nodeproto.DeviceGroupConfig, node uint64) bool {
	raw := stringOption(g.Config["exit_restrict"])
	if value, ok := f.rule.Options["exit_restrict"]; ok {
		raw = stringOption(value)
	}
	ids := []uint64{}
	banDirect := false
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' }) {
		if part == "禁止单端" {
			banDirect = true
			continue
		}
		if id, err := strconv.ParseUint(part, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if node == 1145141919 && banDirect {
		return false
	}
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if id == node {
			return true
		}
	}
	return false
}
func (f *forwarder) updatePeerStatus(cfg nodeproto.ConfigResponse) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	f.peerStatus = map[uint64]nodeproto.GroupPeer{}
	for _, g := range cfg.DeviceGroupConfig {
		for _, p := range g.Peers {
			f.peerStatus[p.NodeID] = p
		}
	}
}

func (f *forwarder) isHealthy(key string) bool {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	h := f.health[key]
	return h == nil || !h.down || time.Now().After(h.retry)
}
func (f *forwarder) markHealth(key string, ok bool, threshold int) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	h := f.health[key]
	if h == nil {
		h = &healthState{}
		f.health[key] = h
	}
	if ok {
		h.successes++
		h.fails = 0
		if h.successes >= max(threshold, 1) {
			h.down = false
		}
	} else {
		h.fails++
		h.successes = 0
		if h.fails >= max(threshold, 1) {
			h.down = true
			h.retry = time.Now().Add(30 * time.Second)
		}
	}
}
func peerKey(id uint64) string { return "peer:" + strconv.FormatUint(id, 10) }
func (f *forwarder) markPeerHealth(id uint64, ok bool, g nodeproto.DeviceGroupConfig) {
	count := g.HealthCheckFailCount
	if count == 0 {
		count = numberOption(f.group.Config["max_fail"])
	}
	if ok {
		count = g.HealthCheckSuccCount
	}
	f.markHealth(peerKey(id), ok, max(count, 1))
	if !ok {
		if seconds := numberOption(f.group.Config["fail_timout_sec"]); seconds > 0 {
			f.healthMu.Lock()
			if h := f.health[peerKey(id)]; h != nil && h.down {
				h.retry = time.Now().Add(time.Duration(seconds) * time.Second)
			}
			f.healthMu.Unlock()
		}
	}
}
func (f *forwarder) orderedPeers(g nodeproto.DeviceGroupConfig, source string) []nodeproto.GroupPeer {
	peers := []nodeproto.GroupPeer{}
	for _, p := range g.Peers {
		f.healthMu.Lock()
		status, known := f.peerStatus[p.NodeID]
		f.healthMu.Unlock()
		if known {
			p.Online = status.Online
			p.CurrentConn = status.CurrentConn
		}
		if p.Host == "" || !p.Online || p.Weight <= 0 || !f.exitAllowed(g, p.NodeID) || !f.isHealthy(peerKey(p.NodeID)) {
			continue
		}
		peers = append(peers, p)
	}
	if len(peers) == 0 {
		return peers
	}
	index := 0
	f.healthMu.Lock()
	active := map[uint64]int{}
	for id, n := range f.peerActive {
		active[id] = n
	}
	f.healthMu.Unlock()
	switch g.Balance {
	case "round_robin":
		f.healthMu.Lock()
		if f.peerWeights == nil {
			f.peerWeights = map[string]int64{}
		}
		total := int64(0)
		best := int64(math.MinInt64)
		for i, p := range peers {
			key := strconv.FormatUint(g.GroupID, 10) + ":" + strconv.FormatUint(p.NodeID, 10)
			weight := int64(max(p.Weight, 1))
			total += weight
			f.peerWeights[key] += weight
			if f.peerWeights[key] > best {
				best = f.peerWeights[key]
				index = i
			}
		}
		key := strconv.FormatUint(g.GroupID, 10) + ":" + strconv.FormatUint(peers[index].NodeID, 10)
		f.peerWeights[key] -= total
		f.healthMu.Unlock()
	case "least_conn":
		sort.SliceStable(peers, func(i, j int) bool {
			return float64(active[peers[i].NodeID]+peers[i].CurrentConn)/float64(max(peers[i].Weight, 1)) < float64(active[peers[j].NodeID]+peers[j].CurrentConn)/float64(max(peers[j].Weight, 1))
		})
	case "hash_ip":
		sort.SliceStable(peers, func(i, j int) bool {
			return peerHashScore(source, peers[i]) < peerHashScore(source, peers[j])
		})
	case "weighted", "random":
		total := 0
		for _, p := range peers {
			total += max(p.Weight, 1)
		}
		position := rand.IntN(total)
		for i, p := range peers {
			if position < max(p.Weight, 1) {
				index = i
				break
			}
			position -= max(p.Weight, 1)
		}
	}
	return append(peers[index:], peers[:index]...)
}
func peerHashScore(source string, p nodeproto.GroupPeer) float64 {
	u := (float64(hashScore(sourceIP(source), strconv.FormatUint(p.NodeID, 10))>>11) + 1) / (float64(uint64(1)<<53) + 1)
	return -math.Log(u) / float64(max(p.Weight, 1))
}
func (f *forwarder) reservePeer(p nodeproto.GroupPeer) bool {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	if p.MaxConn > 0 && f.peerActive[p.NodeID]+p.CurrentConn >= p.MaxConn {
		return false
	}
	f.peerActive[p.NodeID]++
	return true
}
func (f *forwarder) releasePeer(id uint64) {
	f.healthMu.Lock()
	f.peerActive[id]--
	f.healthMu.Unlock()
}
func (f *forwarder) healthLoop() {
	if os.Getenv("HEALTH_CHECK") == "0" {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := map[uint64]time.Time{}
	var lastTargets time.Time
	for {
		select {
		case <-f.ctx.Done():
			return
		case now := <-ticker.C:
			for _, g := range f.cfg.DeviceGroupConfig {
				if !g.HealthCheckEnable || now.Sub(last[g.GroupID]) < time.Duration(max(g.HealthCheckInterval, 1))*time.Second {
					continue
				}
				last[g.GroupID] = now
				path := f.routePath()
				hop := -1
				for i, id := range path {
					if id == g.GroupID {
						hop = i
						break
					}
				}
				if hop < 0 {
					continue
				}
				// Only a node in the preceding group can authenticate this probe.
				previous := f.rule.InboundGroupID
				if hop > 0 {
					previous = path[hop-1]
				}
				if !f.member(previous, f.cfg.NodeID) {
					continue
				}
				for _, peer := range g.Peers {
					if peer.NodeID == f.cfg.NodeID {
						continue
					}
					conn, err := f.dialPeer(peer, g.GroupID, "tcp", "health", path, hop, true)
					if conn != nil {
						conn.Close()
					}
					f.markPeerHealth(peer.NodeID, err == nil, g)
				}
			}
			if f.group.HealthCheckEnable && now.Sub(lastTargets) >= time.Duration(max(f.group.HealthCheckInterval, 1))*time.Second {
				lastTargets = now
				for _, target := range f.targets {
					conn, err := f.dialTargetAddress(f.ctx, "tcp", target.address, time.Duration(max(f.group.HealthCheckTimeout, 1))*time.Second)
					if conn != nil {
						conn.Close()
					}
					count := f.group.HealthCheckFailCount
					if err == nil {
						count = f.group.HealthCheckSuccCount
					}
					f.markHealth(target.address, err == nil, count)
				}
			}
		}
	}
}
