package nodeclient

import (
	"crypto/tls"
	"encoding/binary"
	"github.com/openroute/openroute/internal/nodeproto"
	utls "github.com/refraction-networking/utls"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSnapshotTransactionPreservesPortsStreamsAndPolicies(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {Config: map[string]any{"disable_udp": true}}}
	address := applyOK(t, e, cfg)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "before-failure")
	previous := e.EffectiveConfig()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	bad := cloneConfig(cfg)
	bad.ConfigVersion = 2
	bad.DeviceGroupConfig["0"] = nodeproto.DeviceGroupConfig{Config: map[string]any{"disable_udp": false, "blocked_protocol": []string{"http"}}}
	bad.Rules = append(bad.Rules, bad.Rules[0])
	bad.Rules[1].RuleID = 2
	bad.Rules[1].ListenPort = occupied.Addr().(*net.TCPAddr).Port
	results := e.Apply(bad)
	if results[0].Status != "failed" {
		t.Fatal(results)
	}
	if !reflect.DeepEqual(previous, e.EffectiveConfig()) {
		t.Fatal("failed transaction replaced snapshot")
	}
	if current := e.RunningRules()[0].Port; net.JoinHostPort("127.0.0.1", strconv.Itoa(current)) != address {
		t.Fatal("rollback changed previous port")
	}
	exchange(t, conn, "existing-stream-survives")
	fresh, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	exchange(t, fresh, "GET / HTTP/1.1\r\n\r\n")
}
func TestTLSGatewayRotationAndInvalidCertificateRollback(t *testing.T) {
	a, b := pairConfigs(t, "tls")
	entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
	defer entry.Close()
	defer exit.Close()
	applyNode(t, exit, b)
	address := applyOK(t, entry, a)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "original-certificate")
	previous := exit.EffectiveConfig()
	bad := cloneConfig(b)
	bad.ConfigVersion = 2
	bad.Listeners.TLSKeyPEM = "invalid"
	if result := exit.Apply(bad); result[0].Status != "failed" {
		t.Fatal("invalid cert accepted")
	}
	if !reflect.DeepEqual(exit.EffectiveConfig(), previous) {
		t.Fatal("invalid cert replaced effective config")
	}
	exchange(t, conn, "survived-bad-certificate")
	conn.Close()
	cert, key, pin := testCertificate(t)
	b = cloneConfig(b)
	b.Listeners.TLSCertPEM = cert
	b.Listeners.TLSKeyPEM = key
	applyNode(t, exit, b)
	a = cloneConfig(a)
	g := a.DeviceGroupConfig["20"]
	g.Peers[0].TLSPin = pin
	a.DeviceGroupConfig["20"] = g
	address = applyOK(t, entry, a)
	conn, err = net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "rotated-on-same-port")
}
func TestUserAndNodeDisableCloseExistingConnections(t *testing.T) {
	for _, disable := range []string{"user", "node"} {
		t.Run(disable, func(t *testing.T) {
			cfg := directConfig(echoTarget(t))
			cfg.Rules[0].UserID = 7
			e := NewEngine("127.0.0.1")
			defer e.Close()
			conn, err := net.Dial("tcp", applyOK(t, e, cfg))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			exchange(t, conn, "connected")
			if disable == "user" {
				cfg.Rules[0].UserLimits.Disabled = true
			} else {
				cfg.NodeDisabled = true
			}
			applyNode(t, e, cfg)
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err = conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("disabled connection survived")
			}
			if len(e.RunningRules()) > 0 {
				t.Fatal("disabled listener survived")
			}
		})
	}
}
func TestSharedUserConnectionLimitAndQuotaReset(t *testing.T) {
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].UserID = 7
	cfg.Rules[0].InboundMultiplier = 1
	cfg.Rules[0].UserLimits = nodeproto.UserLimits{ConnLimit: 1, TrafficLimit: 100}
	second := cfg.Rules[0]
	second.RuleID = 2
	cfg.Rules = append(cfg.Rules, second)
	e := NewEngine("127.0.0.1")
	defer e.Close()
	applyNode(t, e, cfg)
	ports := e.RunningRules()
	a := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0].Port))
	b := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[1].Port))
	first, _ := net.Dial("tcp", a)
	defer first.Close()
	exchange(t, first, "hello")
	blocked, _ := net.Dial("tcp", b)
	_ = blocked.SetDeadline(time.Now().Add(time.Second))
	blocked.Write([]byte("blocked"))
	if n, _ := blocked.Read(make([]byte, 7)); n > 0 {
		t.Fatal("shared user connection limit bypassed")
	}
	blocked.Close()
	for i := range cfg.Rules {
		cfg.Rules[i].UserLimits.TrafficUsed = 100
	}
	applyNode(t, e, cfg)
	_ = first.SetDeadline(time.Now().Add(time.Second))
	first.Write([]byte("over-quota"))
	if n, _ := first.Read(make([]byte, 10)); n > 0 {
		t.Fatal("new user usage failed to constrain existing connection")
	}
	first.Close()
	for i := range cfg.Rules {
		cfg.Rules[i].UserLimits.TrafficUsed = 0
	}
	applyNode(t, e, cfg)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.users[7].mu.Lock()
		active := e.users[7].connections
		e.users[7].mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fresh, err := net.Dial("tcp", a)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	exchange(t, fresh, "reset-works")
}
func TestRuleAndUserSpeedLimitActuallyThrottle(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].SpeedLimit = 256 * 1024
	cfg.Rules[0].UserID = 7
	cfg.Rules[0].UserLimits.SpeedLimit = 128 * 1024
	address := applyOK(t, e, cfg)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	exchange(t, conn, strings.Repeat("a", 128*1024))
	elapsed := time.Since(started)
	if elapsed < 700*time.Millisecond {
		t.Fatalf("limiter bypassed: %s", elapsed)
	}
}
func TestRuleIPLimitRejectsNewSource(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].IPLimit = 1
	address := applyOK(t, e, cfg)
	first, _ := net.Dial("tcp", address)
	defer first.Close()
	exchange(t, first, "first-ip")
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}
	second, err := dialer.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(time.Second))
	second.Write([]byte("other-ip"))
	if n, _ := second.Read(make([]byte, 8)); n > 0 {
		t.Fatal("new source bypassed IP limit")
	}
}
func TestHTTPPathAndDevicePolicy(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].UserID = 7
	cfg.Rules[0].UserLimits.DeviceLimit = 1
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {Config: map[string]any{"blocked_path": []string{"/admin"}}}}
	address := applyOK(t, e, cfg)
	request := func(path, ua string, want bool) {
		conn, _ := net.Dial("tcp", address)
		defer conn.Close()
		text := "GET " + path + " HTTP/1.1\r\nHost: node.test\r\nUser-Agent: " + ua + "\r\n\r\n"
		if want {
			exchange(t, conn, text)
		} else {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			conn.Write([]byte(text))
			if n, _ := conn.Read(make([]byte, 1024)); n > 0 {
				t.Fatal("blocked request forwarded")
			}
		}
	}
	request("/", "same-device", true)
	request("/again", "same-device", true)
	request("/admin/users", "same-device", false)
	request("/", "new-device", false)
}
func captureClientHello(t *testing.T) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		secure := tls.Client(a, &tls.Config{ServerName: "one.test", InsecureSkipVerify: true})
		_ = secure.Handshake()
	}()
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	header := make([]byte, 5)
	if _, err := io.ReadFull(b, header); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
	if _, err := io.ReadFull(b, body); err != nil {
		t.Fatal(err)
	}
	return body
}
func TestFragmentedClientHelloStableDevice(t *testing.T) {
	one, two := captureClientHello(t), captureClientHello(t)
	if reflect.DeepEqual(one, two) {
		t.Fatal("expected distinct TLS randoms")
	}
	if helloFingerprint(one) == "" || helloFingerprint(one) != helloFingerprint(two) {
		t.Fatal("TLS fingerprint changes with client random/session/key share")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		for _, fragment := range [][]byte{one[:15], one[15:]} {
			header := []byte{22, 3, 1, byte(len(fragment) >> 8), byte(len(fragment))}
			a.Write(header)
			a.Write(fragment)
		}
	}()
	_ = b.SetDeadline(time.Now().Add(time.Second))
	conn, info, err := inspectStream(b)
	if err != nil {
		t.Fatal(err)
	}
	if info.sni != "one.test" {
		t.Fatalf("fragmented SNI=%q", info.sni)
	}
	replayed := make([]byte, len(one)+10)
	if _, err = io.ReadFull(conn, replayed); err != nil {
		t.Fatal(err)
	}
}
func TestBrowserDeviceFingerprintIgnoresGREASE(t *testing.T) {
	capture := func() []byte {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		go func() {
			secure := utls.UClient(a, &utls.Config{ServerName: "one.test", InsecureSkipVerify: true}, utls.HelloChrome_Auto)
			_ = secure.Handshake()
		}()
		_ = b.SetReadDeadline(time.Now().Add(time.Second))
		header := make([]byte, 5)
		if _, err := io.ReadFull(b, header); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
		if _, err := io.ReadFull(b, body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	first := helloFingerprint(capture())
	for range 4 {
		if next := helloFingerprint(capture()); first == "" || next != first {
			t.Fatal("browser GREASE/random extension order changes device signature")
		}
	}
}
func TestNativeUDPIntegrityAndReplay(t *testing.T) {
	aead, _ := udpAEAD("secret-one")
	other, _ := udpAEAD("secret-two")
	id := [16]byte{7}
	packet, _ := sealUDP(aead, 3, id, 1, false, 2, []byte("payload"))
	if _, _, _, _, _, _, err := openUDP(other, packet); err == nil {
		t.Fatal("wrong UDP key accepted")
	}
	packet[35] ^= 1
	if _, _, _, _, _, _, err := openUDP(aead, packet); err == nil {
		t.Fatal("unauthenticated sequence modification accepted")
	}
}
func TestHealthProbesRecoverTargetAndExitRestrictions(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	target := echoTarget(t)
	cfg := directConfig(target)
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {GroupID: 0, HealthCheckEnable: true, HealthCheckInterval: 1, HealthCheckTimeout: 1, HealthCheckSuccCount: 1}}
	applyOK(t, e, cfg)
	f := e.rules[1]
	address := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	f.markHealth(address, false, 1)
	deadline := time.Now().Add(3 * time.Second)
	for !f.isHealthy(address) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.isHealthy(address) {
		t.Fatal("active probe did not recover target")
	}
	cfg.Rules[0].Options = map[string]any{"exit_restrict": "禁止单端"}
	address = applyOK(t, e, cfg)
	conn, _ := net.Dial("tcp", address)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	conn.Write([]byte("must-not-fallback"))
	if n, _ := conn.Read(make([]byte, 20)); n > 0 {
		t.Fatal("direct-exit restriction bypassed")
	}
}
func TestInboundTLSCertificateValidationIsTransactional(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	address := applyOK(t, e, cfg)
	previous := e.EffectiveConfig()
	cfg.Rules[0].Options = map[string]any{"tls": map[string]any{"cert": []string{"invalid-cert"}, "key": []string{"invalid-key"}}}
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {Config: map[string]any{"tls_inbound_policy": 1}}}
	if results := e.Apply(cfg); results[0].Status != "failed" {
		t.Fatal("invalid TLS termination certificate accepted")
	}
	if !reflect.DeepEqual(previous, e.EffectiveConfig()) {
		t.Fatal("bad inbound cert changed snapshot")
	}
	conn, _ := net.Dial("tcp", address)
	defer conn.Close()
	exchange(t, conn, "old-config-kept")
}
func TestTLSStripAppliesDecryptedHTTPPolicy(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cert, key, _ := testCertificate(t)
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].Options = map[string]any{"tls": map[string]any{"cert": []string{cert}, "key": []string{key}}}
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {Config: map[string]any{"tls_inbound_policy": 1, "blocked_path": []string{"/private"}}}}
	address := applyOK(t, e, cfg)
	for _, path := range []string{"/public", "/private/data"} {
		conn, err := tls.Dial("tcp", address, &tls.Config{ServerName: "node.test", InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		request := "GET " + path + " HTTP/1.1\r\nHost: node.test\r\n\r\n"
		if path == "/public" {
			exchange(t, conn, request)
		} else {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			conn.Write([]byte(request))
			if n, _ := conn.Read(make([]byte, 1024)); n > 0 {
				t.Fatal("encrypted blocked path bypassed policy after TLS stripping")
			}
		}
		conn.Close()
	}
}
func TestPeerWeightedBalanceAndLimits(t *testing.T) {
	f := &forwarder{health: map[string]*healthState{}, peerActive: map[uint64]int{}, peerStatus: map[uint64]nodeproto.GroupPeer{}}
	g := nodeproto.DeviceGroupConfig{GroupID: 1, Balance: "round_robin", Peers: []nodeproto.GroupPeer{{NodeID: 1, Host: "one", Weight: 3, Online: true}, {NodeID: 2, Host: "two", Weight: 1, Online: true}}}
	counts := map[uint64]int{}
	for range 40 {
		counts[f.orderedPeers(g, "client")[0].NodeID]++
	}
	if counts[1] != 30 || counts[2] != 10 {
		t.Fatal(counts)
	}
	g.Balance = "least_conn"
	f.peerActive[1] = 9
	if got := f.orderedPeers(g, "client")[0].NodeID; got != 2 {
		t.Fatal("least_conn ignored weighted current load")
	}
	g.Balance = "hash_ip"
	first := f.orderedPeers(g, "127.0.0.1:1")[0].NodeID
	if second := f.orderedPeers(g, "127.0.0.1:2")[0].NodeID; first != second {
		t.Fatal("hash_ip depends on source port")
	}
	g.Peers[0].MaxConn = 9
	if f.reservePeer(g.Peers[0]) {
		t.Fatal("max connection ceiling ignored")
	}
	g.Config = map[string]any{"exit_restrict": "2,禁止单端"}
	peers := f.orderedPeers(g, "client")
	if len(peers) != 1 || peers[0].NodeID != 2 || f.exitAllowed(g, 1145141919) {
		t.Fatal("exit restriction failed")
	}
}
