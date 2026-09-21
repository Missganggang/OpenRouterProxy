package nodeclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

// Tunnel messages identify only an authorized rule and its configured route.
// A caller can never supply a destination host or port.
type tunnelRequest struct {
	RuleID    uint64   `json:"rule"`
	NodeID    uint64   `json:"node"`
	Action    string   `json:"action"`
	Network   string   `json:"network"`
	Source    string   `json:"source"`
	Path      []uint64 `json:"path"`
	Hop       int      `json:"hop"`
	Timestamp int64    `json:"time"`
	Nonce     string   `json:"nonce"`
	MAC       string   `json:"mac"`
}
type tunnelReply struct {
	Error string `json:"error,omitempty"`
}
type tunnelServer struct {
	engine                     *Engine
	kind, address, fingerprint string
	listener                   net.Listener
	packet                     *net.UDPConn
	mu                         sync.Mutex
	closed                     bool
	conns                      map[net.Conn]bool
	nonces                     map[string]int64
	sessions                   map[string]*udpTunnelSession
	certificate                atomic.Pointer[tls.Certificate]
}

func (e *Engine) configureGateways(cfg nodeproto.ConfigResponse) error {
	addresses, err := bindAddresses(e.bind)
	if err != nil {
		return err
	}
	wants := map[string]*tunnelServer{}
	ports := map[string]int{"direct": cfg.Listeners.DirectPort, "ws": cfg.Listeners.WsPort, "tls": cfg.Listeners.TlsPort, "udp": cfg.Listeners.UdpPort, "reverse": cfg.Listeners.RevPort}
	for _, r := range cfg.Rules {
		if r.Enable && !r.UserLimits.Disabled && r.ReverseEnable && !r.IsOutbound && r.ReversePort > 0 {
			ports["reverse:"+strconv.FormatUint(r.RuleID, 10)] = r.ReversePort
		}
	}
	for _, port := range ports {
		if port < 0 || port > 65535 {
			return errors.New("invalid gateway port")
		}
	}
	var cert tls.Certificate
	if ports["tls"] > 0 || cfg.Listeners.TLSCertPEM != "" || cfg.Listeners.TLSKeyPEM != "" {
		cert, err = tls.X509KeyPair([]byte(cfg.Listeners.TLSCertPEM), []byte(cfg.Listeners.TLSKeyPEM))
		if err != nil {
			return fmt.Errorf("tunnel TLS certificate: %w", err)
		}
	}
	for kind, port := range ports {
		kind = strings.SplitN(kind, ":", 2)[0]
		if port == 0 {
			continue
		}
		if port < 0 || port > 65535 {
			return errors.New("invalid gateway port")
		}
		for _, bind := range addresses {
			address := net.JoinHostPort(bind, strconv.Itoa(port))
			key := kind + ":" + address
			if wants[key] != nil {
				continue
			}
			fingerprint := key
			if old := e.gateways[key]; old != nil && old.fingerprint == fingerprint {
				wants[key] = old
				continue
			}
			s := &tunnelServer{engine: e, kind: kind, address: address, fingerprint: fingerprint, conns: map[net.Conn]bool{}, nonces: map[string]int64{}, sessions: map[string]*udpTunnelSession{}}
			if len(cert.Certificate) > 0 {
				s.certificate.Store(&cert)
			}
			if kind == "udp" {
				var addr *net.UDPAddr
				addr, err = net.ResolveUDPAddr("udp", address)
				if err == nil {
					s.packet, err = net.ListenUDP("udp", addr)
				}
			} else {
				s.listener, err = net.Listen("tcp", address)
				if err == nil && kind == "tls" {
					s.certificate.Store(&cert)
					s.listener = tls.NewListener(s.listener, &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.certificate.Load(), nil }, MinVersion: tls.VersionTLS12})
				}
			}
			if err != nil {
				for key, added := range wants {
					if e.gateways[key] != added {
						added.close()
					}
				}
				return fmt.Errorf("%s gateway: %w", kind, err)
			}
			wants[key] = s
		}
	}
	for key, old := range e.gateways {
		if wants[key] != old {
			old.close()
		}
	}
	for key, s := range wants {
		if (s.kind == "tls" || s.kind == "reverse") && len(cert.Certificate) > 0 {
			s.certificate.Store(&cert)
		}
		if e.gateways[key] != s {
			if s.packet != nil {
				s.spawn(s.serveNativeUDP)
			} else {
				s.spawn(s.serve)
			}
		}
	}
	e.gateways = wants
	return nil
}
func (s *tunnelServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
	}
	if s.packet != nil {
		s.packet.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	for _, session := range s.sessions {
		session.conn.Close()
	}
}
func (s *tunnelServer) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.conns) >= 4096 {
		conn.Close()
		return false
	}
	s.conns[conn] = true
	return true
}
func (s *tunnelServer) untrack(conn net.Conn) {
	conn.Close()
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}
func (s *tunnelServer) spawn(fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.engine.workers.Add(1)
	go func() { defer s.engine.workers.Done(); fn() }()
	return true
}
func (s *tunnelServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if s.track(conn) {
			s.spawn(func() { defer s.untrack(conn); s.handle(conn) })
		}
	}
}
func writeMessage(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16384 {
		return errors.New("tunnel message too large")
	}
	header := []byte{byte(len(data) >> 8), byte(len(data))}
	if _, err = writeFull(w, header); err != nil {
		return err
	}
	_, err = writeFull(w, data)
	return err
}
func readMessage(r io.Reader, value any) error {
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(size[:]))
	if n == 0 || n > 16384 {
		return errors.New("invalid tunnel message length")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func writeFull(w io.Writer, p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := w.Write(p)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
func signRequest(req *tunnelRequest, token string) {
	req.MAC = ""
	data, _ := json.Marshal(req)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(data)
	req.MAC = hex.EncodeToString(mac.Sum(nil))
}
func newTunnelRequest(f *forwarder, network, source string, path []uint64, hop int) *tunnelRequest {
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	req := &tunnelRequest{RuleID: f.rule.RuleID, NodeID: f.cfg.NodeID, Action: "open", Network: network, Source: source, Path: path, Hop: hop, Timestamp: time.Now().Unix(), Nonce: hex.EncodeToString(nonce)}
	signRequest(req, f.rule.TunnelToken)
	return req
}
func (s *tunnelServer) authenticate(req tunnelRequest) (*forwarder, error) {
	s.engine.mu.Lock()
	f := s.engine.rules[req.RuleID]
	s.engine.mu.Unlock()
	if f == nil || len(f.rule.TunnelToken) < 16 {
		return nil, errors.New("unknown tunnel rule")
	}
	supplied, err := hex.DecodeString(req.MAC)
	if err != nil {
		return nil, errors.New("invalid tunnel authentication")
	}
	copy := req
	signRequest(&copy, f.rule.TunnelToken)
	expected, _ := hex.DecodeString(copy.MAC)
	if !hmac.Equal(supplied, expected) || len(req.Nonce) != 32 || time.Since(time.Unix(req.Timestamp, 0)).Abs() > time.Minute {
		return nil, errors.New("invalid tunnel authentication")
	}
	s.mu.Lock()
	for nonce, expires := range s.nonces {
		if expires < time.Now().Unix() {
			delete(s.nonces, nonce)
		}
	}
	key := strconv.FormatUint(req.RuleID, 10) + ":" + req.Nonce
	_, replay := s.nonces[key]
	if len(s.nonces) >= 65536 {
		s.mu.Unlock()
		return nil, errors.New("tunnel authentication capacity reached")
	}
	s.nonces[key] = time.Now().Add(2 * time.Minute).Unix()
	s.mu.Unlock()
	if replay {
		return nil, errors.New("replayed tunnel request")
	}
	if req.Action == "reverse" {
		path := f.routePath()
		if !f.inbound || !f.rule.ReverseEnable || len(path) != 1 || !f.member(path[0], req.NodeID) {
			return nil, errors.New("unauthorized reverse peer")
		}
		return f, nil
	}
	if req.Action != "open" && req.Action != "probe" && req.Action != "mux" {
		return nil, errors.New("invalid tunnel action")
	}
	if !f.outbound || !f.validPath(req.Path) || req.Hop < 0 || req.Hop >= len(req.Path) || !f.member(req.Path[req.Hop], f.cfg.NodeID) {
		return nil, errors.New("unauthorized tunnel route")
	}
	previous := f.rule.InboundGroupID
	if req.Hop > 0 {
		previous = req.Path[req.Hop-1]
	}
	if !f.member(previous, req.NodeID) {
		return nil, errors.New("unauthorized tunnel peer")
	}
	if req.Network != "tcp" && req.Network != "udp" {
		return nil, errors.New("invalid tunnel network")
	}
	if req.Network == "udp" && (boolOption(f.group.Config["disable_udp"]) || boolOption(f.rule.Options["disable_udp"]) || len(f.rule.ChainGroups) > 0) {
		return nil, errors.New("UDP disabled for rule")
	}
	return f, nil
}
func (f *forwarder) member(group, node uint64) bool {
	if node == 0 {
		return false
	}
	g := f.cfg.DeviceGroupConfig[strconv.FormatUint(group, 10)]
	for _, id := range g.NodeIDs {
		if id == node {
			return true
		}
	}
	for _, p := range g.Peers {
		if p.NodeID == node {
			return true
		}
	}
	return false
}
func (f *forwarder) routePath() []uint64 {
	if len(f.rule.ChainGroups) > 0 {
		return append([]uint64(nil), f.rule.ChainGroups...)
	}
	if f.rule.ReverseEnable && f.rule.ReverseGroupID > 0 {
		return []uint64{f.rule.ReverseGroupID}
	}
	if f.rule.OutboundGroupID > 0 {
		return []uint64{f.rule.OutboundGroupID}
	}
	return nil
}
func (f *forwarder) validPath(path []uint64) bool {
	expected := f.routePath()
	if len(expected) != len(path) {
		return false
	}
	for i, id := range expected {
		if path[i] == id {
			continue
		}
		if len(expected) > 1 {
			return false
		}
		seen := map[uint64]bool{}
		for id != 0 && !seen[id] {
			seen[id] = true
			id = f.cfg.DeviceGroupConfig[strconv.FormatUint(id, 10)].FailoverGroupID
			if id == path[i] {
				break
			}
		}
		if id != path[i] {
			return false
		}
	}
	return true
}
func (s *tunnelServer) handle(raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
	conn := raw
	var requestHeader string
	var err error
	reverseProtocol := "direct"
	if s.kind == "reverse" {
		reader := bufio.NewReaderSize(raw, 16384)
		first, peekErr := reader.Peek(1)
		if peekErr != nil {
			return
		}
		conn = &bufferedConn{Conn: raw, reader: reader}
		if first[0] == 22 {
			if s.certificate.Load() == nil {
				return
			}
			secure := tls.Server(conn, &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.certificate.Load(), nil }, MinVersion: tls.VersionTLS12})
			if secure.Handshake() != nil {
				return
			}
			conn = secure
			reverseProtocol = "tls"
		} else if first[0] > 64 {
			conn, requestHeader, err = readCamouflage(conn)
			if err != nil {
				return
			}
			reverseProtocol = "http"
		}
	}
	if s.kind == "ws" {
		conn, requestHeader, err = readCamouflage(conn)
		if err != nil {
			return
		}
	}
	var req tunnelRequest
	if readMessage(conn, &req) != nil {
		return
	}
	if (req.Action == "reverse") != (s.kind == "reverse") {
		return
	}
	f, err := s.authenticate(req)
	if err != nil {
		_ = writeMessage(conn, tunnelReply{Error: err.Error()})
		return
	}
	if s.kind == "ws" {
		protocol, opts := f.transportConfig(req.Path[req.Hop])
		if (protocol != "ws" && protocol != "http") || validateCamouflageRequest(requestHeader, opts, protocol) != nil {
			return
		}
		if err = writeCamouflageResponse(conn, opts, requestHeader); err != nil {
			return
		}
	}
	if req.Action == "reverse" {
		if s.kind != "reverse" {
			return
		}
		protocol, opts := f.transportConfig(f.routePath()[0])
		switch protocol {
		case "tls":
			if reverseProtocol != "tls" {
				return
			}
		case "ws", "http":
			if reverseProtocol != "http" || validateCamouflageRequest(requestHeader, opts, protocol) != nil {
				return
			}
			if writeCamouflageResponse(conn, opts, requestHeader) != nil {
				return
			}
		case "direct":
			if reverseProtocol != "direct" {
				return
			}
		default:
			return
		}
		if writeMessage(conn, tunnelReply{}) != nil {
			return
		}
		_ = conn.SetDeadline(time.Time{})
		s.acceptReverse(f, conn, req.NodeID)
		return
	}
	if s.kind == "reverse" {
		return
	}
	if req.Action == "probe" {
		_ = writeMessage(conn, tunnelReply{})
		return
	}
	if req.Action == "mux" {
		s.serveMux(f, conn)
		return
	}
	f.serveTunnel(conn, req)
}
func validateCamouflageRequest(header string, opts map[string]any, protocol string) error {
	ws := mapOption(opts["ws"])
	if custom := stringOption(ws["request"]); custom != "" {
		if custom != header {
			return errors.New("unexpected camouflage request")
		}
		return nil
	}
	request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(header)))
	if err != nil {
		return err
	}
	path := stringOption(ws["path"])
	if path == "" {
		path = "/"
	}
	if request.Method != "GET" || request.URL.RequestURI() != path {
		return errors.New("unexpected camouflage path")
	}
	if host := stringOption(ws["host"]); host != "" && !strings.EqualFold(request.Host, host) {
		return errors.New("unexpected camouflage host")
	}
	upgrade := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	if (protocol == "ws") != upgrade {
		return errors.New("unexpected camouflage upgrade")
	}
	return nil
}
func (f *forwarder) serveTunnel(conn net.Conn, req tunnelRequest) {
	out, release, err := f.openAt(req.Network, req.Source, req.Path, req.Hop+1)
	if err != nil {
		_ = writeMessage(conn, tunnelReply{Error: err.Error()})
		return
	}
	defer release()
	if !f.track(out) {
		return
	}
	defer f.untrack(out)
	if writeMessage(conn, tunnelReply{}) != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if req.Network == "udp" {
		f.relayPackets(&framedPacketConn{Conn: conn}, out, true)
	} else {
		f.relay(conn, out, true, false)
	}
}
func (f *forwarder) openRoute(network, source string, path []uint64) (net.Conn, func(), error) {
	if path == nil {
		path = f.routePath()
	}
	if f.rule.ReverseEnable && len(path) > 0 {
		return f.openReverse(network, source, path)
	}
	return f.openAt(network, source, path, 0)
}
func (f *forwarder) openAt(network, source string, path []uint64, hop int) (net.Conn, func(), error) {
	if hop >= len(path) {
		if len(path) == 0 && !f.exitAllowed(f.group, 1145141919) {
			return nil, func() {}, errors.New("direct exit denied by policy")
		}
		conn, target, err := f.dialTarget(network, source)
		if err != nil {
			return nil, func() {}, err
		}
		return conn, func() { target.active.Add(-1) }, nil
	}
	groupID := path[hop]
	if hop == 0 && len(path) == 1 {
		group := f.cfg.DeviceGroupConfig[strconv.FormatUint(groupID, 10)]
		raw := stringOption(group.Config["exit_restrict"])
		if value, ok := f.rule.Options["exit_restrict"]; ok {
			raw = stringOption(value)
		}
		onlyDirect := true
		for _, p := range group.Peers {
			if f.exitAllowed(group, p.NodeID) {
				onlyDirect = false
			}
		}
		if onlyDirect && strings.Contains(raw, "1145141919") && f.exitAllowed(group, 1145141919) {
			conn, target, err := f.dialTarget(network, source)
			if err != nil {
				return nil, func() {}, err
			}
			return conn, func() { target.active.Add(-1) }, nil
		}
	}
	seen := map[uint64]bool{}
	last := errors.New("no available exit")
	for groupID != 0 && !seen[groupID] {
		seen[groupID] = true
		group, ok := f.cfg.DeviceGroupConfig[strconv.FormatUint(groupID, 10)]
		if !ok {
			return nil, func() {}, errors.New("missing route group")
		}
		candidatePath := append([]uint64(nil), path...)
		candidatePath[hop] = groupID
		for _, peer := range f.orderedPeers(group, source) {
			if !f.reservePeer(peer) {
				continue
			}
			conn, err := f.dialPeer(peer, groupID, network, source, candidatePath, hop, false)
			if err != nil {
				f.releasePeer(peer.NodeID)
				f.markPeerHealth(peer.NodeID, false, group)
				last = err
				continue
			}
			f.markPeerHealth(peer.NodeID, true, group)
			var once sync.Once
			return conn, func() { once.Do(func() { f.releasePeer(peer.NodeID) }) }, nil
		}
		if len(path) > 1 {
			break
		}
		groupID = group.FailoverGroupID
	}
	return nil, func() {}, last
}
func (f *forwarder) transportConfig(group uint64) (string, map[string]any) {
	opts := map[string]any{}
	for k, v := range f.group.Config {
		opts[k] = v
	}
	for k, v := range f.cfg.DeviceGroupConfig[strconv.FormatUint(group, 10)].Config {
		opts[k] = v
	}
	for k, v := range f.rule.Options {
		opts[k] = v
	}
	protocol := f.rule.Protocol
	if p := stringOption(opts["protocol"]); p != "" {
		protocol = p
	}
	return protocol, opts
}
func stringOption(v any) string { s, _ := v.(string); return s }
func (f *forwarder) dialPeer(peer nodeproto.GroupPeer, group uint64, network, source string, path []uint64, hop int, probe bool) (net.Conn, error) {
	if len(f.rule.ChainGroups) > 0 && hop == 0 && !probe {
		return f.dialMux(peer, group, network, source, path, hop)
	}
	return f.dialPeerBase(peer, group, network, source, path, hop, probe, false)
}
func (f *forwarder) dialPeerBase(peer nodeproto.GroupPeer, group uint64, network, source string, path []uint64, hop int, probe, mux bool) (net.Conn, error) {
	protocol, opts := f.transportConfig(group)
	if network == "udp" && !probe && !boolOption(opts["udp_over_tcp"]) {
		return f.dialNativeUDP(peer, source, path, hop)
	}
	port := peer.DirectPort
	switch protocol {
	case "ws", "http":
		port = peer.WsPort
	case "tls":
		port = peer.TlsPort
	case "direct":
	default:
		return nil, errors.New("unsupported peer transport")
	}
	if port <= 0 {
		return nil, errors.New("peer transport port is missing")
	}
	dialCtx := f.ctx
	timeout := 8 * time.Second
	if probe {
		seconds := f.cfg.DeviceGroupConfig[strconv.FormatUint(group, 10)].HealthCheckTimeout
		if seconds <= 0 {
			seconds = 3
		}
		timeout = time.Duration(seconds) * time.Second
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(f.ctx, timeout)
		defer cancel()
	}
	conn, err := outboundDial(dialCtx, "tcp", net.JoinHostPort(peer.Host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	cancelConn := conn
	stopCancel := context.AfterFunc(dialCtx, func() { cancelConn.Close() })
	defer stopCancel()
	fail := func(err error) (net.Conn, error) { conn.Close(); return nil, err }
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if protocol == "tls" {
		secure, tlsErr := f.securePeer(conn, peer, mapOption(opts["tls"]))
		if tlsErr != nil {
			return fail(tlsErr)
		}
		conn = secure
	}
	if protocol == "ws" || protocol == "http" {
		if err = writeCamouflageRequest(conn, opts, protocol, peer.Host); err != nil {
			return fail(err)
		}
	}
	req := newTunnelRequest(f, network, source, path, hop)
	if probe {
		req.Action = "probe"
		signRequest(req, f.rule.TunnelToken)
	}
	if mux {
		req.Action = "mux"
		signRequest(req, f.rule.TunnelToken)
	}
	if err = writeMessage(conn, req); err != nil {
		return fail(err)
	}
	if protocol == "ws" || protocol == "http" {
		var header string
		conn, header, err = readCamouflage(conn)
		if err != nil {
			return fail(err)
		}
		if expected := stringOption(mapOption(opts["ws"])["response"]); expected != "" && header != expected {
			return fail(errors.New("unexpected tunnel camouflage response"))
		}
	}
	var reply tunnelReply
	if err = readMessage(conn, &reply); err != nil {
		return fail(err)
	}
	if reply.Error != "" {
		return fail(errors.New(reply.Error))
	}
	_ = conn.SetDeadline(time.Time{})
	if network == "udp" && !probe {
		conn = &framedPacketConn{Conn: conn}
	}
	return conn, nil
}
func readCamouflage(conn net.Conn) (net.Conn, string, error) {
	r := bufio.NewReaderSize(conn, 16384)
	var header bytes.Buffer
	for header.Len() < 16384 {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return conn, "", err
		}
		if header.Len()+len(line) > 16384 {
			return conn, "", errors.New("tunnel HTTP header too large")
		}
		header.Write(line)
		if string(line) == "\r\n" {
			return &bufferedConn{Conn: conn, reader: r}, header.String(), nil
		}
	}
	return conn, "", errors.New("tunnel HTTP header too large")
}
func writeCamouflageRequest(conn net.Conn, opts map[string]any, protocol, peerHost string) error {
	ws := mapOption(opts["ws"])
	header := stringOption(ws["request"])
	if header == "" {
		host := stringOption(ws["host"])
		if host == "" {
			host = peerHost
		}
		path := stringOption(ws["path"])
		if path == "" {
			path = "/"
		}
		if strings.ContainsAny(host+path, "\r\n") {
			return errors.New("invalid HTTP camouflage")
		}
		header = "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\n"
		if protocol == "ws" {
			header += "Connection: Upgrade\r\nUpgrade: websocket\r\n"
		}
		header += "\r\n"
	}
	if !strings.HasSuffix(header, "\r\n\r\n") || len(header) > 16384 {
		return errors.New("invalid camouflage request template")
	}
	_, err := writeFull(conn, []byte(header))
	return err
}
func writeCamouflageResponse(conn net.Conn, opts map[string]any, request string) error {
	header := stringOption(mapOption(opts["ws"])["response"])
	if header == "" {
		if strings.Contains(strings.ToLower(request), "upgrade: websocket") {
			header = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
		} else {
			header = "HTTP/1.1 200 OK\r\n\r\n"
		}
	}
	if !strings.HasSuffix(header, "\r\n\r\n") || len(header) > 16384 {
		return errors.New("invalid camouflage response template")
	}
	_, err := writeFull(conn, []byte(header))
	return err
}

// Each Write is exactly one datagram, regardless of TCP segmentation/coalescing.
type framedPacketConn struct {
	net.Conn
	writeMu sync.Mutex
}

func (c *framedPacketConn) Read(p []byte) (int, error) {
	var size [2]byte
	if _, err := io.ReadFull(c.Conn, size[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(size[:]))
	if n > len(p) {
		return 0, io.ErrShortBuffer
	}
	_, err := io.ReadFull(c.Conn, p[:n])
	return n, err
}
func (c *framedPacketConn) Write(p []byte) (int, error) {
	if len(p) > 65507 {
		return 0, errors.New("UDP datagram too large")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(p)))
	if _, err := writeFull(c.Conn, header[:]); err != nil {
		return 0, err
	}
	return writeFull(c.Conn, p)
}
func (f *forwarder) relayPackets(in, out net.Conn, outbound bool) {
	counter := f.traffic
	if outbound {
		counter = f.outTraffic
	}
	counter.connections.Add(1)
	defer counter.connections.Add(-1)
	done := make(chan struct{}, 1)
	pump := func(dst, src net.Conn, upload bool) {
		buffer := make([]byte, 65535)
		for {
			_ = src.SetReadDeadline(time.Now().Add(time.Minute))
			n, err := src.Read(buffer)
			if err != nil {
				break
			}
			writer := accountWriter{f: f, writer: dst, counter: counter, upload: upload, outbound: outbound, udp: true}
			if _, err = writer.Write(buffer[:n]); err != nil {
				break
			}
		}
		in.Close()
		out.Close()
	}
	go func() { pump(out, in, true); done <- struct{}{} }()
	pump(in, out, false)
	<-done
}
