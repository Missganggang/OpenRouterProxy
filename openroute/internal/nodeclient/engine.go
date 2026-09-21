package nodeclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hashicorp/yamux"
	"github.com/openroute/openroute/internal/nodeproto"
	"hash/fnv"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	mu        sync.Mutex
	bind      string
	closed    bool
	rules     map[uint64]*forwarder
	traffic   map[trafficKey]*ruleTraffic
	users     map[uint64]*usageState
	effective nodeproto.ConfigResponse
	gateways  map[string]*tunnelServer
	reverse   map[uint64]chan net.Conn
	workers   sync.WaitGroup
	trafficMu sync.Mutex
	samples   map[trafficSampleKey]*nodeproto.RuleTrafficItem
}
type trafficKey struct {
	id        uint64
	direction string
}
type ruleTraffic struct{ in, out, pendingIn, pendingOut, connections atomic.Int64 }
type forwarder struct {
	engine              *Engine
	rule                nodeproto.ConfigRule
	views               []nodeproto.ConfigRule
	cfg                 nodeproto.ConfigResponse
	network             networkConfig
	group               nodeproto.DeviceGroupConfig
	inbound, outbound   bool
	fingerprint         string
	traffic, outTraffic *ruleTraffic
	targets             []*forwardTarget
	next                atomic.Uint64
	ctx                 context.Context
	cancel              context.CancelFunc
	mu                  sync.Mutex
	closed              bool
	connections         map[net.Conn]struct{}
	listeners           []net.Listener
	packets             []*net.UDPConn
	ports               []int
	usage, user         *usageState
	healthMu            sync.Mutex
	health              map[string]*healthState
	peerActive          map[uint64]int
	peerStatus          map[uint64]nodeproto.GroupPeer
	peerWeights         map[string]int64
	muxMu               sync.Mutex
	muxSessions         map[uint64]*yamux.Session
}
type forwardTarget struct {
	address string
	weight  uint64
	active  atomic.Int64
}

func NewEngine(bind string) *Engine {
	if strings.TrimSpace(bind) == "" {
		bind = "0.0.0.0"
	}
	return &Engine{bind: bind, rules: map[uint64]*forwarder{}, traffic: map[trafficKey]*ruleTraffic{}, users: map[uint64]*usageState{}, gateways: map[string]*tunnelServer{}, reverse: map[uint64]chan net.Conn{}}
}
func cloneConfig(cfg nodeproto.ConfigResponse) nodeproto.ConfigResponse {
	data, _ := json.Marshal(cfg)
	var copy nodeproto.ConfigResponse
	_ = json.Unmarshal(data, &copy)
	return copy
}
func (e *Engine) EffectiveConfig() nodeproto.ConfigResponse {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneConfig(e.effective)
}

// Apply treats a snapshot as a transaction: any validation, bind or TLS
// configuration failure restores the old listeners and keeps the complete old
// effective snapshot. Existing streams close only after a successful commit.
func (e *Engine) Apply(in nodeproto.ConfigResponse) []nodeproto.RuleSyncResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	cfg := cloneConfig(in)
	failed := func(err error) []nodeproto.RuleSyncResult {
		results := []nodeproto.RuleSyncResult{}
		seen := map[uint64]bool{}
		for _, r := range cfg.Rules {
			if !seen[r.RuleID] {
				seen[r.RuleID] = true
				results = append(results, nodeproto.RuleSyncResult{RuleID: r.RuleID, Status: "failed", Error: err.Error()})
			}
		}
		if len(results) == 0 {
			results = append(results, nodeproto.RuleSyncResult{Status: "failed", Error: err.Error()})
		}
		return results
	}
	if e.closed {
		return failed(errors.New("engine is closed"))
	}
	if cfg.NodeDisabled {
		for id, f := range e.rules {
			f.close()
			delete(e.rules, id)
		}
		for key, s := range e.gateways {
			s.close()
			delete(e.gateways, key)
		}
		e.closeReverse()
		results := []nodeproto.RuleSyncResult{}
		for _, r := range cfg.Rules {
			results = append(results, nodeproto.RuleSyncResult{RuleID: r.RuleID, Status: "normal"})
		}
		cfg.Rules = nil
		cfg.Full = true
		e.effective = cfg
		return results
	}
	if !cfg.Full {
		merged := cloneConfig(e.effective)
		merged.ConfigVersion = cfg.ConfigVersion
		removed := map[uint64]bool{}
		for _, id := range cfg.RemovedRuleIDs {
			removed[id] = true
		}
		for _, r := range cfg.Rules {
			removed[r.RuleID] = true
		}
		merged.Rules = nil
		for _, r := range e.effective.Rules {
			if !removed[r.RuleID] {
				merged.Rules = append(merged.Rules, r)
			}
		}
		merged.Rules = append(merged.Rules, cfg.Rules...)
		if merged.DeviceGroupConfig == nil {
			merged.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{}
		}
		for key, g := range cfg.DeviceGroupConfig {
			merged.DeviceGroupConfig[key] = g
		}
		if cfg.NodeID != 0 {
			merged.NodeID = cfg.NodeID
		}
		if cfg.Listeners != (nodeproto.NodeListeners{}) {
			merged.Listeners = cfg.Listeners
		}
		cfg = merged
	}
	networkCfg, networkErr := loadNetworkConfig(e.bind)
	if networkErr != nil {
		return failed(networkErr)
	}
	byID := map[uint64][]nodeproto.ConfigRule{}
	for _, r := range cfg.Rules {
		byID[r.RuleID] = append(byID[r.RuleID], r)
	}
	ids := make([]uint64, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	candidates := map[uint64]*forwarder{}
	created := []*forwarder{}
	paused := []*forwarder{}
	rollback := func(err error) []nodeproto.RuleSyncResult {
		for _, f := range created {
			f.close()
		}
		for _, f := range paused {
			if restoreErr := f.startListeners(); restoreErr != nil {
				err = fmt.Errorf("%w; restoring rule %d: %v", err, f.rule.RuleID, restoreErr)
			}
		}
		return failed(err)
	}
	// Validate the complete set before touching any listener.
	for _, id := range ids {
		for _, r := range byID[id] {
			if r.UserLimits.Disabled {
				r.Enable = false
			}
			g, exists := cfg.DeviceGroupConfig[strconv.FormatUint(r.InboundGroupID, 10)]
			if r.Enable && r.InboundGroupID != 0 && !exists {
				return failed(fmt.Errorf("rule %d: missing inbound group policy", id))
			}
			if err := validateRule(r, g, cfg, e.bind); err != nil {
				return failed(fmt.Errorf("rule %d: %w", id, err))
			}
		}
	}
	for _, id := range ids {
		views := byID[id]
		r := views[0]
		inbound, outbound := false, false
		for _, v := range views {
			if v.IsOutbound {
				outbound = true
			} else {
				inbound = true
				r = v
			}
		}
		if !r.Enable || r.UserLimits.Disabled {
			continue
		}
		fingerprint := nodeproto.RuleConfigHash(cfg, r) + fmt.Sprint(inbound, outbound, networkCfg)
		old := e.rules[id]
		if old != nil && old.fingerprint == fingerprint {
			candidates[id] = old
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		f := &forwarder{engine: e, rule: r, views: views, cfg: cfg, group: cfg.DeviceGroupConfig[strconv.FormatUint(r.InboundGroupID, 10)], inbound: inbound, outbound: outbound, fingerprint: fingerprint, ctx: ctx, cancel: cancel, connections: map[net.Conn]struct{}{}, usage: newUsage(), health: map[string]*healthState{}, peerActive: map[uint64]int{}}
		f.network = networkCfg
		f.traffic = e.counter(id, "inbound")
		f.outTraffic = e.counter(id, "outbound")
		if r.UserID > 0 {
			if e.users[r.UserID] == nil {
				e.users[r.UserID] = newUsage()
			}
			f.user = e.users[r.UserID]
		}
		if old != nil {
			f.usage = old.usage
			old.pauseListeners()
			paused = append(paused, old)
		}
		for _, target := range r.Targets {
			if target.Status != "down" {
				f.targets = append(f.targets, &forwardTarget{address: net.JoinHostPort(target.Host, strconv.Itoa(target.Port)), weight: uint64(max(target.Weight, 1))})
			}
		}
		created = append(created, f)
		if err := f.startListeners(); err != nil {
			return rollback(fmt.Errorf("rule %d: %w", id, err))
		}
		candidates[id] = f
	}
	if err := e.configureGateways(cfg, networkCfg); err != nil {
		return rollback(err)
	}
	for id, old := range e.rules {
		if candidates[id] != old {
			old.close()
		}
	}
	e.rules = candidates
	for id, f := range candidates {
		f.updatePeerStatus(cfg)
		if f.user != nil {
			f.user.mu.Lock()
			f.user.syncUsed(byID[id][0].UserLimits.TrafficUsed)
			f.user.mu.Unlock()
		}
	}
	for _, f := range created {
		f.spawn(f.healthLoop)
		if f.rule.ReverseEnable && f.outbound {
			f.spawn(f.maintainReverse)
		}
	}
	cfg.Full = true
	cfg.RemovedRuleIDs = nil
	e.effective = cfg
	results := make([]nodeproto.RuleSyncResult, 0, len(ids))
	for _, id := range ids {
		results = append(results, nodeproto.RuleSyncResult{RuleID: id, Status: "normal"})
	}
	return results
}
func (e *Engine) counter(id uint64, direction string) *ruleTraffic {
	key := trafficKey{id, direction}
	if e.traffic[key] == nil {
		e.traffic[key] = &ruleTraffic{}
	}
	return e.traffic[key]
}
func (e *Engine) RunningRules() []nodeproto.RunningRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []nodeproto.RunningRule{}
	for id, f := range e.rules {
		port := f.rule.ListenPort
		if len(f.ports) > 0 {
			port = f.ports[0]
		}
		if !f.inbound {
			port = f.cfg.Listeners.DirectPort
			protocol := f.rule.Protocol
			for _, group := range f.routePath() {
				if f.member(group, f.cfg.NodeID) {
					protocol, _ = f.transportConfig(group)
					break
				}
			}
			switch protocol {
			case "ws", "http":
				port = f.cfg.Listeners.WsPort
			case "tls":
				port = f.cfg.Listeners.TlsPort
			}
		}
		out = append(out, nodeproto.RunningRule{RuleID: id, Port: port, Status: "running", Conn: int(f.traffic.connections.Load() + f.outTraffic.connections.Load()), ConfigHash: nodeproto.RuleConfigHash(f.cfg, f.rule)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleID < out[j].RuleID })
	return out
}
func (e *Engine) Stats() nodeproto.ReportStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.trafficMu.Lock()
	defer e.trafficMu.Unlock()
	return e.statsLocked()
}
func (e *Engine) statsLocked() nodeproto.ReportStats {
	var out nodeproto.ReportStats
	for key, c := range e.traffic {
		out.NetIn += c.in.Load()
		out.NetOut += c.out.Load()
		out.CurrentConn += int(c.connections.Load())
		in, down := c.pendingIn.Swap(0), c.pendingOut.Swap(0)
		if in != 0 || down != 0 {
			out.RuleTraffic = append(out.RuleTraffic, nodeproto.RuleTrafficItem{RuleID: key.id, Direction: key.direction, TrafficIn: in, TrafficOut: down})
		}
	}
	sort.Slice(out.RuleTraffic, func(i, j int) bool {
		a, b := out.RuleTraffic[i], out.RuleTraffic[j]
		if a.RuleID == b.RuleID {
			return a.Direction < b.Direction
		}
		return a.RuleID < b.RuleID
	})
	e.samples = nil
	return out
}
func (e *Engine) Close() {
	e.mu.Lock()
	e.closed = true
	for id, f := range e.rules {
		f.close()
		delete(e.rules, id)
	}
	for key, s := range e.gateways {
		s.close()
		delete(e.gateways, key)
	}
	e.closeReverse()
	e.mu.Unlock()
	e.workers.Wait()
}
func (e *Engine) closeReverse() {
	for id, queue := range e.reverse {
		for {
			select {
			case c := <-queue:
				c.Close()
			default:
				goto drained
			}
		}
	drained:
		delete(e.reverse, id)
	}
}
func bindAddresses(bind string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(ip net.IP) {
		address := ip.String()
		if !seen[address] {
			seen[address] = true
			out = append(out, address)
		}
	}
	for _, part := range strings.Split(bind, ",") {
		part = strings.TrimSpace(part)
		if ip := net.ParseIP(part); ip != nil {
			add(ip)
			continue
		}
		iface, err := net.InterfaceByName(part)
		if err != nil {
			return nil, fmt.Errorf("BIND_INBOUND: invalid bind address/interface %q", part)
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("BIND_INBOUND: %w", err)
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil {
				add(ip)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("BIND_INBOUND: no usable bind addresses")
	}
	return out, nil
}
func validateRule(r nodeproto.ConfigRule, g nodeproto.DeviceGroupConfig, cfg nodeproto.ConfigResponse, bind string) error {
	if r.RuleID == 0 {
		return errors.New("rule ID is required")
	}
	if !r.Enable {
		return nil
	}
	if os.Getenv("NYA_PROXY") != "" {
		return errors.New("NYA_PROXY external proxy is not supported by this node client")
	}
	if _, err := bindAddresses(bind); err != nil {
		return err
	}
	switch r.Protocol {
	case "direct", "ws", "http", "tls":
	default:
		return fmt.Errorf("unsupported tunnel protocol %q", r.Protocol)
	}
	if r.OutboundGroupID != 0 || r.IsOutbound || len(r.ChainGroups) > 0 || r.ReverseEnable {
		if len(r.TunnelToken) < 16 {
			return errors.New("authorized tunnel key is missing")
		}
	}
	if r.ListenPort < 0 || r.ListenPort > 65535 || r.ListenPortEnd < 0 || r.ListenPortEnd > 65535 || (r.ListenPortEnd != 0 && r.ListenPortEnd < r.ListenPort) {
		return errors.New("invalid listener port range")
	}
	if r.ListenPortEnd > r.ListenPort && (r.ListenPort == 0 || r.ListenPortEnd-r.ListenPort >= 256) {
		return errors.New("port range requires a fixed start and at most 256 ports")
	}
	if r.SpeedLimit < 0 || r.IPLimit < 0 || r.ConnLimit < 0 {
		return errors.New("limits must not be negative")
	}
	switch r.TargetBalance {
	case "", "failover", "round_robin", "least_conn", "weighted", "hash_ip":
	default:
		return errors.New("unknown target selection strategy")
	}
	hasChildren := false
	for _, child := range cfg.Rules {
		if child.ParentID == r.RuleID && child.IsSubRule {
			hasChildren = true
		}
	}
	if !hasChildren && len(r.Targets) == 0 {
		return errors.New("no configured target")
	}
	for _, target := range r.Targets {
		if strings.TrimSpace(target.Host) == "" || target.Port <= 0 || target.Port > 65535 {
			return errors.New("invalid target address")
		}
		if !allowedHost(target.Host, g.Config) {
			return fmt.Errorf("target %s is denied by group policy", target.Host)
		}
	}
	if r.IsSubRule && (r.ParentID == 0 || r.SNI == "") {
		return errors.New("SNI sub-rule requires a parent and name")
	}
	if len(r.ChainGroups) > 3 {
		return errors.New("at most three chained groups are supported")
	}
	if len(r.ChainGroups) == 1 {
		return errors.New("chain requires two or three groups")
	}
	if len(r.ChainGroups) > 0 && r.ReverseEnable {
		return errors.New("reverse and chained routing cannot be combined")
	}
	if len(r.ChainGroups) > 0 && !boolOption(g.Config["disable_udp"]) {
		return errors.New("chained rules require disable_udp")
	}
	for _, id := range r.ChainGroups {
		if _, ok := cfg.DeviceGroupConfig[strconv.FormatUint(id, 10)]; !ok {
			return fmt.Errorf("missing chained group %d", id)
		}
		if cfg.DeviceGroupConfig[strconv.FormatUint(id, 10)].FailoverGroupID != 0 {
			return errors.New("chained routing does not support failover groups")
		}
	}
	for _, options := range []map[string]any{g.Config, r.Options} {
		if profile := stringOption(mapOption(options["tls"])["chfp"]); profile != "" {
			if _, ok := tlsProfiles[profile]; !ok {
				return errors.New("unknown TLS fingerprint")
			}
		}
	}
	if r.ReverseEnable && r.ReverseGroupID > 0 {
		if _, ok := cfg.DeviceGroupConfig[strconv.FormatUint(r.ReverseGroupID, 10)]; !ok {
			return errors.New("missing reverse exit group")
		}
	}
	if r.OutboundGroupID != 0 {
		if _, ok := cfg.DeviceGroupConfig[strconv.FormatUint(r.OutboundGroupID, 10)]; !ok {
			return errors.New("missing outbound group")
		}
	}
	if numberOption(g.Config["tls_inbound_policy"]) == 1 {
		tlsOptions := mapOption(r.Options["tls"])
		if len(listOption(tlsOptions["cert"])) == 0 || len(listOption(tlsOptions["key"])) == 0 {
			return errors.New("TLS termination requires rule certificate and key")
		}
		if _, err := tls.X509KeyPair([]byte(strings.Join(listOption(tlsOptions["cert"]), "\n")), []byte(strings.Join(listOption(tlsOptions["key"]), "\n"))); err != nil {
			return fmt.Errorf("TLS termination certificate: %w", err)
		}
	}
	for _, s := range r.Shaping {
		switch s {
		case 1, 2, 3, 4, 2000:
		default:
			return fmt.Errorf("unknown shaping option %d", s)
		}
	}
	return nil
}
func (f *forwarder) startListeners() error {
	if !f.inbound || f.rule.IsSubRule {
		return nil
	}
	addresses := f.network.RuleBind
	previousPorts := append([]int(nil), f.ports...)
	f.ports = nil
	end := max(f.rule.ListenPortEnd, f.rule.ListenPort)
	for _, bind := range addresses {
		for port := f.rule.ListenPort; port <= end; port++ {
			listenPort := port
			if listenPort == 0 && len(previousPorts) > len(f.ports) {
				listenPort = previousPorts[len(f.ports)]
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(listenPort)))
			if err != nil {
				f.pauseListeners()
				return fmt.Errorf("TCP listen: %w", err)
			}
			f.listeners = append(f.listeners, ln)
			actual := ln.Addr().(*net.TCPAddr).Port
			f.ports = append(f.ports, actual)
			if !boolOption(f.group.Config["disable_udp"]) && !boolOption(f.rule.Options["disable_udp"]) {
				udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(bind), Port: actual})
				if err != nil {
					f.pauseListeners()
					return fmt.Errorf("UDP listen: %w", err)
				}
				f.packets = append(f.packets, udp)
			}
		}
	}
	for _, ln := range f.listeners {
		f.spawn(func() { f.serveTCP(ln) })
	}
	for _, udp := range f.packets {
		f.spawn(func() { f.serveUDP(udp) })
	}
	return nil
}
func (f *forwarder) pauseListeners() {
	for _, ln := range f.listeners {
		ln.Close()
	}
	for _, udp := range f.packets {
		udp.Close()
	}
	f.listeners = nil
	f.packets = nil
}
func (f *forwarder) close() {
	f.cancel()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.pauseListeners()
	for c := range f.connections {
		c.Close()
	}
}
func (f *forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		c.Close()
		return false
	}
	f.connections[c] = struct{}{}
	return true
}
func (f *forwarder) spawn(fn func()) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.engine.workers.Add(1)
	go func() { defer f.engine.workers.Done(); fn() }()
	return true
}
func (f *forwarder) untrack(c net.Conn) {
	c.Close()
	f.mu.Lock()
	delete(f.connections, c)
	f.mu.Unlock()
}
func (f *forwarder) orderedTargets(source string) []*forwardTarget {
	result := append([]*forwardTarget(nil), f.targets...)
	if len(result) == 0 {
		return nil
	}
	index := 0
	switch f.rule.TargetBalance {
	case "round_robin":
		index = int((f.next.Add(1) - 1) % uint64(len(result)))
	case "least_conn":
		sort.SliceStable(result, func(i, j int) bool { return result[i].active.Load() < result[j].active.Load() })
	case "hash_ip":
		sort.SliceStable(result, func(i, j int) bool {
			return hashScore(sourceIP(source), result[i].address) > hashScore(sourceIP(source), result[j].address)
		})
	case "weighted":
		var total uint64
		for _, target := range result {
			total += target.weight
		}
		position := (f.next.Add(1) - 1) % total
		for i, target := range result {
			if position < target.weight {
				index = i
				break
			}
			position -= target.weight
		}
	}
	return append(result[index:], result[:index]...)
}
func hashScore(a, b string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(a + "\x00" + b))
	return h.Sum64()
}
func (f *forwarder) dialTarget(network, source string) (net.Conn, *forwardTarget, error) {
	if containsShape(f.rule.Shaping, 2000) {
		network += "4"
	}
	var last error = errors.New("no healthy target")
	for _, target := range f.orderedTargets(source) {
		if !f.isHealthy(target.address) {
			continue
		}
		if containsShape(f.rule.Shaping, 2000) && strings.Contains(target.address, "]:") {
			continue
		}
		conn, err := f.dialTargetAddress(f.ctx, network, target.address, 5*time.Second)
		if err != nil {
			f.markHealth(target.address, false, 0)
			last = err
			continue
		}
		f.markHealth(target.address, true, 0)
		target.active.Add(1)
		return conn, target, nil
	}
	return nil, nil, last
}
func containsShape(list []int, value int) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
func (f *forwarder) serveTCP(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if !f.track(conn) {
			return
		}
		f.spawn(func() { f.forwardTCP(conn) })
	}
}
func (f *forwarder) forwardTCP(raw net.Conn) {
	defer f.untrack(raw)
	conn := raw
	info := streamInfo{}
	if f.needsInspection() {
		var err error
		conn, info, err = inspectStream(conn)
		if err != nil {
			return
		}
	}
	selected := f
	if numberOption(f.group.Config["tls_inbound_policy"]) == 2 {
		selected = f.engine.subRule(f.rule.RuleID, info.sni)
		if selected == nil {
			return
		}
	}
	if selected != f {
		if !selected.track(raw) {
			return
		}
		defer selected.untrack(raw)
	}
	if err := selected.enforcePolicy(info); err != nil {
		return
	}
	var err error
	conn, err = selected.terminateTLS(conn)
	if err != nil {
		return
	}
	policy := selected.group.Config
	if numberOption(policy["tls_inbound_policy"]) == 1 && (len(listOption(policy["blocked_path"])) > 0 || len(listOption(policy["blocked_protocol"])) > 0 || containsShape(selected.rule.Shaping, 3) || containsShape(selected.rule.Shaping, 4) || selected.rule.UserLimits.DeviceLimit > 0) {
		var inner streamInfo
		conn, inner, err = inspectStream(conn)
		if err != nil || selected.enforcePolicy(inner) != nil {
			return
		}
		if inner.device != "" {
			info.device += "/" + inner.device
		}
	}
	admission, err := selected.admit(raw.RemoteAddr().String(), info.device)
	if err != nil {
		return
	}
	defer admission.close()
	out, release, err := selected.openRoute("tcp", raw.RemoteAddr().String(), nil)
	if err != nil {
		return
	}
	defer release()
	if !selected.track(out) {
		return
	}
	defer selected.untrack(out)
	selected.relay(conn, out, false, false)
}
func (e *Engine) subRule(parent uint64, sni string) *forwarder {
	e.mu.Lock()
	defer e.mu.Unlock()
	var chosen *forwarder
	for _, f := range e.rules {
		if f.rule.IsSubRule && f.rule.ParentID == parent && hostMatches(sni, f.rule.SNI) {
			if chosen == nil || len(f.rule.SNI) > len(chosen.rule.SNI) {
				chosen = f
			}
		}
	}
	return chosen
}
func (f *forwarder) relay(in, out net.Conn, outbound, udp bool) {
	counter := f.traffic
	if outbound {
		counter = f.outTraffic
	}
	counter.connections.Add(1)
	defer counter.connections.Add(-1)
	done := make(chan struct{}, 1)
	go func() {
		_, err := io.Copy(&accountWriter{f: f, writer: out, counter: counter, upload: true, outbound: outbound, udp: udp}, in)
		closeWrite(out)
		if err != nil {
			out.Close()
			in.Close()
		}
		done <- struct{}{}
	}()
	_, err := io.Copy(&accountWriter{f: f, writer: in, counter: counter, outbound: outbound, udp: udp}, out)
	closeWrite(in)
	if err != nil {
		out.Close()
		in.Close()
	}
	<-done
}
func closeWrite(conn net.Conn) {
	if writer, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = writer.CloseWrite()
	}
}

type accountWriter struct {
	f                     *forwarder
	writer                io.Writer
	counter               *ruleTraffic
	upload, outbound, udp bool
}

func (w *accountWriter) Write(data []byte) (int, error) {
	if !w.outbound {
		if !w.udp {
			if err := w.f.usage.wait(w.f.ctx, w.f.rule.SpeedLimit, len(data)); err != nil {
				return 0, err
			}
			if w.f.user != nil {
				if err := w.f.user.wait(w.f.ctx, w.f.rule.UserLimits.SpeedLimit, len(data)); err != nil {
					return 0, err
				}
			}
		}
		if w.f.user != nil {
			if err := w.f.user.consume(len(data), w.f.rule.UserLimits, w.f.trafficMultiplier()); err != nil {
				return 0, err
			}
		}
	}
	n, err := w.writer.Write(data)
	w.f.recordTraffic(w.counter, w.upload, n)
	return n, err
}
func (f *forwarder) serveUDP(listener *net.UDPConn) {
	var mu sync.Mutex
	sessions := map[string]net.Conn{}
	buffer := make([]byte, 65535)
	for {
		n, source, err := listener.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		key := source.String()
		mu.Lock()
		conn, count := sessions[key], len(sessions)
		mu.Unlock()
		if conn == nil {
			if count >= 4096 {
				continue
			}
			admission, err := f.admit(key, sourceIP(key))
			if err != nil {
				continue
			}
			var release func()
			conn, release, err = f.openRoute("udp", key, nil)
			if err != nil {
				admission.close()
				continue
			}
			if !f.track(conn) {
				release()
				admission.close()
				return
			}
			f.traffic.connections.Add(1)
			mu.Lock()
			sessions[key] = conn
			mu.Unlock()
			serve := func(conn net.Conn, source *net.UDPAddr, key string) {
				defer admission.close()
				defer release()
				defer f.traffic.connections.Add(-1)
				defer f.untrack(conn)
				defer func() { mu.Lock(); delete(sessions, key); mu.Unlock() }()
				responses := make([]byte, 65535)
				for {
					_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
					n, err := conn.Read(responses)
					if err != nil {
						return
					}
					if f.user != nil {
						if f.user.consume(n, f.rule.UserLimits, f.trafficMultiplier()) != nil {
							return
						}
					}
					n, err = listener.WriteToUDP(responses[:n], source)
					f.recordTraffic(f.traffic, false, n)
					if err != nil {
						return
					}
				}
			}
			sessionConn, sessionSource, sessionKey := conn, source, key
			if !f.spawn(func() { serve(sessionConn, sessionSource, sessionKey) }) {
				f.untrack(conn)
				release()
				admission.close()
				f.traffic.connections.Add(-1)
				return
			}
		}
		if f.user != nil {
			if f.user.consume(n, f.rule.UserLimits, f.trafficMultiplier()) != nil {
				conn.Close()
				continue
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		written, err := conn.Write(buffer[:n])
		f.recordTraffic(f.traffic, true, written)
		if err != nil {
			conn.Close()
		}
	}
}
