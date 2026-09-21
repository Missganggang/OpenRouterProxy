package nodeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

// Engine applies direct TCP and native UDP forwarding rules. Unsupported
// protocols and restrictions fail closed instead of silently changing routing.
type Engine struct {
	mu      sync.Mutex
	bind    string
	closed  bool
	rules   map[uint64]*forwarder
	traffic map[uint64]*ruleTraffic
}

type ruleTraffic struct {
	in, out               atomic.Int64
	pendingIn, pendingOut atomic.Int64
	connections           atomic.Int64
}

type forwarder struct {
	rule        nodeproto.ConfigRule
	fingerprint string
	traffic     *ruleTraffic
	targets     []*forwardTarget
	next        atomic.Uint64
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
	listeners   []net.Listener
	packets     []*net.UDPConn
	ports       []int
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
	return &Engine{bind: strings.TrimSpace(bind), rules: make(map[uint64]*forwarder), traffic: make(map[uint64]*ruleTraffic)}
}

// Apply leaves identical listeners and established connections untouched.
// Changed or rejected rules close their old listeners and connections. Full
// snapshots also remove missing rules; incremental snapshots only touch IDs
// present in Rules or RemovedRuleIDs. Persisted snapshots can be reapplied on
// process startup using this same method.
func (e *Engine) Apply(cfg nodeproto.ConfigResponse) []nodeproto.RuleSyncResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	results := make([]nodeproto.RuleSyncResult, 0, len(cfg.Rules))
	desired := make(map[uint64]bool, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		desired[rule.RuleID] = true
	}
	if cfg.Full {
		for id := range e.rules {
			if !desired[id] {
				e.remove(id)
			}
		}
	}
	for _, id := range cfg.RemovedRuleIDs {
		e.remove(id)
	}
	counts := make(map[uint64]int)
	for _, rule := range cfg.Rules {
		counts[rule.RuleID]++
	}
	for _, rule := range cfg.Rules {
		result := nodeproto.RuleSyncResult{RuleID: rule.RuleID, Status: "normal"}
		group, hasGroup := cfg.DeviceGroupConfig[strconv.FormatUint(rule.InboundGroupID, 10)]
		udp, err := validateDirectRule(rule, group, e.bind)
		if rule.Enable && rule.InboundGroupID != 0 && !hasGroup {
			err = errors.New("缺少入口组配置，拒绝在无法确认组权限时启动规则")
		}
		if e.closed {
			err = errors.New("节点转发引擎已关闭")
		} else if counts[rule.RuleID] > 1 {
			err = errors.New("同一节点收到重复规则视角；当前客户端仅支持单端直连")
		}
		if !rule.Enable && err == nil {
			e.remove(rule.RuleID)
			results = append(results, result)
			continue
		}
		encoded, marshalErr := json.Marshal(struct {
			Rule  nodeproto.ConfigRule
			UDP   bool
			Group map[string]interface{}
		}{rule, udp, group.Config})
		if marshalErr != nil {
			err = fmt.Errorf("规则配置无法编码: %w", marshalErr)
		}
		if err == nil {
			if current := e.rules[rule.RuleID]; current != nil && current.fingerprint == string(encoded) {
				results = append(results, result)
				continue
			}
		}
		e.remove(rule.RuleID)
		if err == nil {
			counter := e.traffic[rule.RuleID]
			if counter == nil {
				counter = &ruleTraffic{}
				e.traffic[rule.RuleID] = counter
			}
			var f *forwarder
			f, err = startForwarder(rule, e.bind, udp, counter)
			if err == nil {
				f.fingerprint = string(encoded)
				e.rules[rule.RuleID] = f
			}
		}
		if err != nil {
			result.Status, result.Error = "failed", err.Error()
		}
		results = append(results, result)
	}
	return results
}

func (e *Engine) remove(id uint64) {
	if f := e.rules[id]; f != nil {
		f.close()
		delete(e.rules, id)
	}
}

func (e *Engine) RunningRules() []nodeproto.RunningRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]nodeproto.RunningRule, 0, len(e.rules))
	for id, f := range e.rules {
		result = append(result, nodeproto.RunningRule{RuleID: id, Port: f.ports[0], Status: "running", Conn: int(f.traffic.connections.Load())})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RuleID < result[j].RuleID })
	return result
}

// Stats returns cumulative NetIn/NetOut and drains per-rule traffic deltas.
// The caller must retain the returned deltas until the panel accepts them.
func (e *Engine) Stats() nodeproto.ReportStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	var result nodeproto.ReportStats
	for id, counter := range e.traffic {
		result.NetIn += counter.in.Load()
		result.NetOut += counter.out.Load()
		result.CurrentConn += int(counter.connections.Load())
		in, out := counter.pendingIn.Swap(0), counter.pendingOut.Swap(0)
		if in != 0 || out != 0 {
			result.RuleTraffic = append(result.RuleTraffic, nodeproto.RuleTrafficItem{RuleID: id, TrafficIn: in, TrafficOut: out})
		}
	}
	sort.Slice(result.RuleTraffic, func(i, j int) bool { return result.RuleTraffic[i].RuleID < result.RuleTraffic[j].RuleID })
	return result
}

func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for id := range e.rules {
		e.remove(id)
	}
}

func validateDirectRule(rule nodeproto.ConfigRule, group nodeproto.DeviceGroupConfig, bind string) (bool, error) {
	if rule.RuleID == 0 {
		return false, errors.New("规则 ID 不能为空")
	}
	if !rule.Enable {
		return false, nil
	}
	if net.ParseIP(bind) == nil {
		return false, errors.New("BIND_INBOUND 必须是一个 IPv4 或 IPv6 地址")
	}
	if rule.Protocol != "direct" || rule.OutboundGroupID != 0 || rule.IsOutbound {
		return false, errors.New("当前节点客户端仅支持无出口组的 direct TCP/UDP 转发；隧道及出口节点尚不支持")
	}
	if rule.ReverseEnable || rule.ReverseGroupID != 0 || len(rule.ChainGroups) != 0 || rule.IsSubRule || rule.ParentID != 0 || rule.SNI != "" || len(rule.Shaping) != 0 {
		return false, errors.New("当前节点客户端尚不支持反向隧道、链式出口、SNI 分流或整流")
	}
	if group.FailoverGroupID != 0 {
		return false, errors.New("当前节点客户端尚不支持设备组故障转移")
	}
	if rule.SpeedLimit != 0 || rule.IPLimit != 0 {
		return false, errors.New("当前节点客户端尚不支持速度或 IP 数限制，拒绝运行以避免绕过限制")
	}
	if rule.ConnLimit < 0 {
		return false, errors.New("连接数限制不能为负数")
	}
	if rule.ListenPort < 0 || rule.ListenPort > 65535 || rule.ListenPortEnd < 0 || rule.ListenPortEnd > 65535 || (rule.ListenPortEnd != 0 && rule.ListenPortEnd < rule.ListenPort) {
		return false, errors.New("监听端口或端口范围无效")
	}
	if rule.ListenPortEnd > rule.ListenPort && (rule.ListenPort == 0 || rule.ListenPortEnd-rule.ListenPort >= 256) {
		return false, errors.New("端口范围需要固定起始端口，且最多包含 256 个端口")
	}
	switch rule.TargetBalance {
	case "", "failover", "round_robin", "least_conn", "weighted", "hash_ip":
	default:
		return false, fmt.Errorf("不支持目标选择策略 %q", rule.TargetBalance)
	}
	active := 0
	if len(rule.Targets) > 1024 {
		return false, errors.New("每条规则最多支持 1024 个目标")
	}
	for _, target := range rule.Targets {
		if target.Status == "down" {
			continue
		}
		if strings.TrimSpace(target.Host) == "" || target.Port < 1 || target.Port > 65535 {
			return false, errors.New("目标地址或端口无效")
		}
		if target.Weight > 1000000 {
			return false, errors.New("目标权重不能超过 1000000")
		}
		if target.Status != "" && target.Status != "up" {
			return false, fmt.Errorf("未知目标状态 %q", target.Status)
		}
		active++
	}
	if active == 0 {
		return false, errors.New("规则没有可用的目标地址")
	}
	udp := true
	for _, options := range []map[string]interface{}{group.Config, rule.Options} {
		for key, value := range options {
			switch key {
			case "disable_udp":
				flag, ok := value.(bool)
				if !ok {
					return false, errors.New("disable_udp 必须是布尔值")
				}
				if flag {
					udp = false
				}
			case "protocol", "max_fail", "fail_timout_sec":
				// Tunnel-only settings have no effect when no outbound group exists.
			default:
				if nonzeroOption(value) {
					return false, fmt.Errorf("当前 direct 客户端尚不支持配置项 %s，拒绝忽略该设置", key)
				}
			}
		}
	}
	return udp, nil
}

func nonzeroOption(v interface{}) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		for _, key := range rv.MapKeys() {
			if nonzeroOption(rv.MapIndex(key).Interface()) {
				return true
			}
		}
		return false
	case reflect.Slice, reflect.Array:
		return rv.Len() != 0
	default:
		return !rv.IsZero()
	}
}

func startForwarder(rule nodeproto.ConfigRule, bind string, udp bool, counter *ruleTraffic) (*forwarder, error) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &forwarder{rule: rule, traffic: counter, ctx: ctx, cancel: cancel, connections: make(map[net.Conn]struct{})}
	for _, t := range rule.Targets {
		if t.Status == "down" {
			continue
		}
		weight := t.Weight
		if weight <= 0 {
			weight = 1
		}
		f.targets = append(f.targets, &forwardTarget{address: net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), weight: uint64(weight)})
	}
	end := rule.ListenPortEnd
	if end < rule.ListenPort {
		end = rule.ListenPort
	}
	for port := rule.ListenPort; port <= end; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(port)))
		if err != nil {
			f.close()
			return nil, fmt.Errorf("TCP 监听失败: %w", err)
		}
		f.listeners = append(f.listeners, listener)
		actualPort := listener.Addr().(*net.TCPAddr).Port
		f.ports = append(f.ports, actualPort)
		if udp {
			packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(bind), Port: actualPort})
			if err != nil {
				f.close()
				return nil, fmt.Errorf("UDP 监听失败: %w", err)
			}
			f.packets = append(f.packets, packet)
		}
	}
	for _, listener := range f.listeners {
		go f.serveTCP(listener)
	}
	for _, packet := range f.packets {
		go f.serveUDP(packet)
	}
	return f, nil
}

func (f *forwarder) close() {
	f.cancel()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for _, listener := range f.listeners {
		_ = listener.Close()
	}
	for _, packet := range f.packets {
		_ = packet.Close()
	}
	for connection := range f.connections {
		_ = connection.Close()
	}
}

func (f *forwarder) reserve() bool {
	for {
		current := f.traffic.connections.Load()
		if f.rule.ConnLimit > 0 && current >= int64(f.rule.ConnLimit) {
			return false
		}
		if f.traffic.connections.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (f *forwarder) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		_ = conn.Close()
		return false
	}
	f.connections[conn] = struct{}{}
	return true
}

func (f *forwarder) untrack(conn net.Conn) {
	_ = conn.Close()
	f.mu.Lock()
	delete(f.connections, conn)
	f.mu.Unlock()
}

func (f *forwarder) orderedTargets(source string) []*forwardTarget {
	result := append([]*forwardTarget(nil), f.targets...)
	index := 0
	switch f.rule.TargetBalance {
	case "round_robin":
		index = int((f.next.Add(1) - 1) % uint64(len(result)))
	case "least_conn":
		sort.SliceStable(result, func(i, j int) bool { return result[i].active.Load() < result[j].active.Load() })
	case "hash_ip":
		host, _, err := net.SplitHostPort(source)
		if err == nil {
			source = host
		}
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(source))
		index = int(hash.Sum64() % uint64(len(result)))
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

func (f *forwarder) dial(network, source string) (net.Conn, *forwardTarget, error) {
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, target := range f.orderedTargets(source) {
		conn, err := dialer.DialContext(f.ctx, network, target.address)
		if err != nil {
			lastErr = err
			continue
		}
		target.active.Add(1)
		return conn, target, nil
	}
	return nil, nil, lastErr
}

func (f *forwarder) serveTCP(listener net.Listener) {
	for {
		in, err := listener.Accept()
		if err != nil {
			return
		}
		if !f.reserve() {
			_ = in.Close()
			continue
		}
		if !f.track(in) {
			f.traffic.connections.Add(-1)
			return
		}
		go f.forwardTCP(in)
	}
}

func (f *forwarder) forwardTCP(in net.Conn) {
	defer f.traffic.connections.Add(-1)
	defer f.untrack(in)
	out, target, err := f.dial("tcp", in.RemoteAddr().String())
	if err != nil {
		return
	}
	defer target.active.Add(-1)
	if !f.track(out) {
		return
	}
	defer f.untrack(out)
	done := make(chan struct{}, 1)
	go func() {
		_, copyErr := io.Copy(&trafficWriter{writer: out, total: &f.traffic.in, pending: &f.traffic.pendingIn}, in)
		if tcp, ok := out.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		if copyErr != nil {
			_ = out.Close()
			_ = in.Close()
		}
		done <- struct{}{}
	}()
	_, copyErr := io.Copy(&trafficWriter{writer: in, total: &f.traffic.out, pending: &f.traffic.pendingOut}, out)
	if tcp, ok := in.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	if copyErr != nil {
		_ = out.Close()
		_ = in.Close()
	}
	<-done
}

type trafficWriter struct {
	writer         io.Writer
	total, pending *atomic.Int64
}

func (w *trafficWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.total.Add(int64(n))
	w.pending.Add(int64(n))
	return n, err
}

// A UDP client address gets a connected target socket, preserving reply routing
// between concurrent clients. Idle sessions expire after one minute.
func (f *forwarder) serveUDP(listener *net.UDPConn) {
	var mu sync.Mutex
	sessions := make(map[string]net.Conn)
	buffer := make([]byte, 65535)
	for {
		n, source, err := listener.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		key := source.String()
		mu.Lock()
		conn := sessions[key]
		count := len(sessions)
		mu.Unlock()
		if conn == nil {
			if count >= 4096 || !f.reserve() {
				continue
			}
			var target *forwardTarget
			conn, target, err = f.dial("udp", key)
			if err != nil {
				f.traffic.connections.Add(-1)
				continue
			}
			if !f.track(conn) {
				target.active.Add(-1)
				f.traffic.connections.Add(-1)
				return
			}
			mu.Lock()
			sessions[key] = conn
			mu.Unlock()
			go func(conn net.Conn, target *forwardTarget, source *net.UDPAddr, key string) {
				defer f.traffic.connections.Add(-1)
				defer target.active.Add(-1)
				defer f.untrack(conn)
				defer func() {
					mu.Lock()
					if sessions[key] == conn {
						delete(sessions, key)
					}
					mu.Unlock()
				}()
				responses := make([]byte, 65535)
				for {
					_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
					n, err := conn.Read(responses)
					if err != nil {
						return
					}
					n, err = listener.WriteToUDP(responses[:n], source)
					f.traffic.out.Add(int64(n))
					f.traffic.pendingOut.Add(int64(n))
					if err != nil {
						return
					}
				}
			}(conn, target, source, key)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		written, err := conn.Write(buffer[:n])
		f.traffic.in.Add(int64(written))
		f.traffic.pendingIn.Add(int64(written))
		if err != nil {
			_ = conn.Close()
		}
	}
}
