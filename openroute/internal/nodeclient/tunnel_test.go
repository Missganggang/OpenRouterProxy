package nodeclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"github.com/openroute/openroute/internal/nodeproto"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testPortPool struct {
	sync.Mutex
	initialized                 bool
	offset, next                int
	ephemeralLow, ephemeralHigh int
}

func testPort(t *testing.T) int {
	t.Helper()
	// The production engine opens listeners from port numbers, so the helper
	// cannot retain the probe sockets. Never recycle a number within this test
	// process, and keep candidates out of the kernel's outbound ephemeral pool.
	// This also avoids collisions with the :0 echo servers used by these tests.
	const firstPort, portCount = 10000, 65536 - 10000
	testPortPool.Lock()
	defer testPortPool.Unlock()
	if !testPortPool.initialized {
		offset, err := rand.Int(rand.Reader, big.NewInt(portCount))
		if err != nil {
			t.Fatal(err)
		}
		testPortPool.offset = int(offset.Int64())
		// Conservative fallback covers standard Linux, Windows and macOS ranges.
		testPortPool.ephemeralLow, testPortPool.ephemeralHigh = 32768, 65535
		if data, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
			var low, high int
			if _, err := fmt.Sscanf(string(data), "%d %d", &low, &high); err == nil && low >= 1 && high <= 65535 && low <= high {
				testPortPool.ephemeralLow, testPortPool.ephemeralHigh = low, high
			}
		}
		testPortPool.initialized = true
	}
	for testPortPool.next < portCount {
		port := firstPort + (testPortPool.offset+testPortPool.next)%portCount
		testPortPool.next++
		if port >= testPortPool.ephemeralLow && port <= testPortPool.ephemeralHigh {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		udp, udpErr := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if udpErr != nil {
			ln.Close()
			continue
		}
		udp.Close()
		ln.Close()
		return port
	}
	t.Fatal("no unused TCP/UDP test port outside the ephemeral range")
	return 0
}
func testCertificate(t *testing.T) (string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"node.test", "one.test", "two.test"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := x509.MarshalPKCS8PrivateKey(key)
	sum := sha256.Sum256(der)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})), hex.EncodeToString(sum[:])
}
func testNode(t *testing.T, id uint64) (nodeproto.ConfigResponse, nodeproto.GroupPeer) {
	t.Helper()
	cert, key, pin := testCertificate(t)
	cfg := nodeproto.ConfigResponse{NodeID: id, Full: true, ConfigVersion: 1, Listeners: nodeproto.NodeListeners{DirectPort: testPort(t), WsPort: testPort(t), TlsPort: testPort(t), UdpPort: testPort(t), RevPort: testPort(t), TLSCertPEM: cert, TLSKeyPEM: key}}
	l := cfg.Listeners
	return cfg, nodeproto.GroupPeer{NodeID: id, Host: "127.0.0.1", DirectPort: l.DirectPort, WsPort: l.WsPort, TlsPort: l.TlsPort, UdpPort: l.UdpPort, RevPort: l.RevPort, TLSPin: pin, Weight: 1, Online: true}
}
func applyNode(t *testing.T, e *Engine, cfg nodeproto.ConfigResponse) {
	t.Helper()
	for _, r := range e.Apply(cfg) {
		if r.Status != "normal" {
			t.Fatalf("node %d apply: %+v", cfg.NodeID, r)
		}
	}
}
func pairConfigs(t *testing.T, protocol string) (nodeproto.ConfigResponse, nodeproto.ConfigResponse) {
	a, pa := testNode(t, 1)
	b, pb := testNode(t, 2)
	rule := nodeproto.ConfigRule{RuleID: 1, UserID: 7, InboundGroupID: 10, OutboundGroupID: 20, Protocol: protocol, Enable: true, Targets: []nodeproto.ConfigTarget{echoTarget(t)}, TunnelToken: "test-authorized-rule-secret-012345", InboundMultiplier: 1, OutboundMultiplier: 1}
	groups := map[string]nodeproto.DeviceGroupConfig{"10": {GroupID: 10, NodeIDs: []uint64{1}, Peers: []nodeproto.GroupPeer{pa}}, "20": {GroupID: 20, NodeIDs: []uint64{2}, Peers: []nodeproto.GroupPeer{pb}, Balance: "failover"}}
	a.DeviceGroupConfig = groups
	b.DeviceGroupConfig = groups
	a.Rules = []nodeproto.ConfigRule{rule}
	rule.IsOutbound = true
	b.Rules = []nodeproto.ConfigRule{rule}
	return a, b
}
func TestTunnelProtocolsNativeUDPAndUOT(t *testing.T) {
	for _, protocol := range []string{"direct", "ws", "http", "tls"} {
		for _, uot := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/uot=%v", protocol, uot), func(t *testing.T) {
				a, b := pairConfigs(t, protocol)
				a.Rules[0].Options = map[string]any{"udp_over_tcp": uot}
				b.Rules[0].Options = a.Rules[0].Options
				entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
				defer entry.Close()
				defer exit.Close()
				applyNode(t, exit, b)
				address := applyOK(t, entry, a)
				for _, network := range []string{"tcp", "udp"} {
					conn, err := net.Dial(network, address)
					if err != nil {
						t.Fatal(err)
					}
					exchange(t, conn, network+"-through-"+protocol)
					exchange(t, conn, "second-datagram")
					conn.Close()
				}
				deadline := time.Now().Add(time.Second)
				for exit.Stats().NetIn == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if exit.Stats().NetIn == 0 {
					t.Fatal("traffic bypassed exit")
				}
			})
		}
	}
}
func TestTunnelTLSFingerprintsAndPins(t *testing.T) {
	for _, profile := range []string{"chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq"} {
		t.Run(profile, func(t *testing.T) {
			a, b := pairConfigs(t, "tls")
			opts := map[string]any{"tls": map[string]any{"chfp": profile, "sni": "node.test", "alpn": []string{"http/1.1"}}}
			a.Rules[0].Options = opts
			b.Rules[0].Options = opts
			entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
			defer entry.Close()
			defer exit.Close()
			applyNode(t, exit, b)
			address := applyOK(t, entry, a)
			probe, release, routeErr := entry.rules[1].openRoute("tcp", "127.0.0.1:1234", nil)
			if routeErr != nil {
				t.Fatal(routeErr)
			}
			probe.Close()
			release()
			conn, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			exchange(t, conn, "fingerprinted")
		})
	}
	t.Run("wrong-pin", func(t *testing.T) {
		a, b := pairConfigs(t, "tls")
		entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
		defer entry.Close()
		defer exit.Close()
		applyNode(t, exit, b)
		a = cloneConfig(a)
		g := a.DeviceGroupConfig["20"]
		g.Peers[0].TLSPin = strings.Repeat("0", 64)
		a.DeviceGroupConfig["20"] = g
		address := applyOK(t, entry, a)
		conn, _ := net.Dial("tcp", address)
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		conn.Write([]byte("must-not-pass"))
		if n, _ := conn.Read(make([]byte, 20)); n != 0 {
			t.Fatal("TLS pin bypassed")
		}
	})
}
func TestTunnelCustomCamouflage(t *testing.T) {
	a, b := pairConfigs(t, "ws")
	opts := map[string]any{"ws": map[string]any{"request": "GET /custom HTTP/1.5\r\nX-Custom: yes\r\n\r\n", "response": "HTTP/1.5 200 CUSTOM\r\n\r\n"}}
	a.Rules[0].Options = opts
	b.Rules[0].Options = opts
	entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
	defer entry.Close()
	defer exit.Close()
	applyNode(t, exit, b)
	conn, err := net.Dial("tcp", applyOK(t, entry, a))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "custom-template")
}
func TestTunnelReverseTCPAndUDP(t *testing.T) {
	for _, protocol := range []string{"direct", "ws", "http", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			a, b := pairConfigs(t, protocol)
			options := map[string]any{"ws": map[string]any{"host": "reverse.test", "path": "/reverse/tunnel"}, "tls": map[string]any{"chfp": "chrome", "sni": "node.test"}}
			a.Rules[0].Options = options
			b.Rules[0].Options = options
			a.Rules[0].ReverseEnable = true
			b.Rules[0].ReverseEnable = true
			a.Rules[0].ReversePort = testPort(t)
			b.Rules[0].ReversePort = a.Rules[0].ReversePort
			entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
			defer entry.Close()
			defer exit.Close()
			address := applyOK(t, entry, a)
			applyNode(t, exit, b)
			for _, network := range []string{"tcp", "udp", "tcp"} {
				conn, err := net.Dial(network, address)
				if err != nil {
					t.Fatal(err)
				}
				exchange(t, conn, "reverse-"+network)
				conn.Close()
			}
			if exit.Stats().NetIn == 0 {
				t.Fatal("reverse traffic missed exit")
			}
		})
	}
}
func TestReverseTLSPinAndHTTPPathAreEnforced(t *testing.T) {
	for _, protocol := range []string{"tls", "http"} {
		t.Run(protocol, func(t *testing.T) {
			a, b := pairConfigs(t, protocol)
			a.Rules[0].ReverseEnable = true
			b.Rules[0].ReverseEnable = true
			b = cloneConfig(b)
			if protocol == "tls" {
				g := b.DeviceGroupConfig["10"]
				g.Peers[0].TLSPin = strings.Repeat("0", 64)
				b.DeviceGroupConfig["10"] = g
			} else {
				a.Rules[0].Options = map[string]any{"ws": map[string]any{"path": "/expected"}}
				b.Rules[0].Options = map[string]any{"ws": map[string]any{"path": "/wrong"}}
			}
			entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
			defer entry.Close()
			defer exit.Close()
			address := applyOK(t, entry, a)
			applyNode(t, exit, b)
			conn, _ := net.Dial("tcp", address)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
			conn.Write([]byte("must-not-forward"))
			if n, _ := conn.Read(make([]byte, 20)); n > 0 {
				t.Fatal("reverse authentication policy bypassed")
			}
			if exit.Stats().NetIn != 0 {
				t.Fatal("invalid reverse transport forwarded traffic")
			}
		})
	}
}
func TestOneNodeBothRolesTunnel(t *testing.T) {
	cfg, peer := testNode(t, 1)
	rule := nodeproto.ConfigRule{RuleID: 1, Enable: true, Protocol: "tls", InboundGroupID: 10, OutboundGroupID: 20, TunnelToken: "local-both-roles-authorized-secret", Targets: []nodeproto.ConfigTarget{echoTarget(t)}}
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"10": {GroupID: 10, NodeIDs: []uint64{1}, Peers: []nodeproto.GroupPeer{peer}}, "20": {GroupID: 20, NodeIDs: []uint64{1}, Peers: []nodeproto.GroupPeer{peer}}}
	cfg.Rules = []nodeproto.ConfigRule{rule}
	rule.IsOutbound = true
	cfg.Rules = append(cfg.Rules, rule)
	e := NewEngine("127.0.0.1")
	defer e.Close()
	conn, err := net.Dial("tcp", applyOK(t, e, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "both-roles-on-one-node")
	stats := e.Stats()
	if len(stats.RuleTraffic) != 2 {
		t.Fatalf("expected both traffic directions: %+v", stats)
	}
}
func TestTunnelThreeHopWithMux(t *testing.T) {
	a, b := pairConfigs(t, "direct")
	c, pc := testNode(t, 3)
	d, pd := testNode(t, 4)
	groups := a.DeviceGroupConfig
	g := groups["10"]
	g.Config = map[string]any{"disable_udp": true}
	groups["10"] = g
	groups["30"] = nodeproto.DeviceGroupConfig{GroupID: 30, NodeIDs: []uint64{3}, Peers: []nodeproto.GroupPeer{pc}}
	groups["40"] = nodeproto.DeviceGroupConfig{GroupID: 40, NodeIDs: []uint64{4}, Peers: []nodeproto.GroupPeer{pd}}
	a.Rules[0].ChainGroups = []uint64{20, 30, 40}
	b.Rules[0].ChainGroups = a.Rules[0].ChainGroups
	c.Rules = append([]nodeproto.ConfigRule(nil), b.Rules...)
	d.Rules = append([]nodeproto.ConfigRule(nil), b.Rules...)
	c.DeviceGroupConfig = groups
	d.DeviceGroupConfig = groups
	engines := []*Engine{NewEngine("127.0.0.1"), NewEngine("127.0.0.1"), NewEngine("127.0.0.1"), NewEngine("127.0.0.1")}
	for _, e := range engines {
		defer e.Close()
	}
	applyNode(t, engines[3], d)
	applyNode(t, engines[2], c)
	applyNode(t, engines[1], b)
	address := applyOK(t, engines[0], a)
	for range 3 {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		exchange(t, conn, "chain-through-three-exits")
		conn.Close()
	}
	for _, e := range engines[1:] {
		if e.Stats().NetIn == 0 {
			t.Fatal("chain bypassed hop")
		}
	}
	f := engines[0].rules[1]
	f.muxMu.Lock()
	n := len(f.muxSessions)
	f.muxMu.Unlock()
	if n != 1 {
		t.Fatalf("first-hop mux sessions=%d", n)
	}
}
func TestTunnelRejectsUnauthorizedAndReplay(t *testing.T) {
	a, b := pairConfigs(t, "direct")
	entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
	defer entry.Close()
	defer exit.Close()
	applyNode(t, exit, b)
	applyOK(t, entry, a)
	f := entry.rules[1]
	req := newTunnelRequest(f, "tcp", "127.0.0.1:5", []uint64{20}, 0)
	req.Action = "probe"
	signRequest(req, f.rule.TunnelToken)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(b.Listeners.DirectPort))
	for i := 0; i < 2; i++ {
		conn, _ := net.Dial("tcp", address)
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		writeMessage(conn, req)
		var reply tunnelReply
		err := readMessage(conn, &reply)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if (i == 0) != (reply.Error == "") {
			t.Fatalf("replay response %d: %+v", i, reply)
		}
	}
	req = newTunnelRequest(f, "tcp", "127.0.0.1:5", []uint64{999}, 0)
	conn, _ := net.Dial("tcp", address)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	writeMessage(conn, req)
	var reply tunnelReply
	readMessage(conn, &reply)
	if reply.Error == "" {
		t.Fatal("arbitrary route accepted")
	}
}
func TestTunnelExitFailover(t *testing.T) {
	a, b := pairConfigs(t, "direct")
	g := a.DeviceGroupConfig["20"]
	healthy := g.Peers[0]
	dead := healthy
	dead.NodeID = 99
	dead.DirectPort = testPort(t)
	g.Peers = []nodeproto.GroupPeer{dead}
	g.NodeIDs = []uint64{99}
	g.FailoverGroupID = 30
	a.DeviceGroupConfig["20"] = g
	a.DeviceGroupConfig["30"] = nodeproto.DeviceGroupConfig{GroupID: 30, NodeIDs: []uint64{2}, Peers: []nodeproto.GroupPeer{healthy}}
	b.DeviceGroupConfig = a.DeviceGroupConfig
	entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
	defer entry.Close()
	defer exit.Close()
	applyNode(t, exit, b)
	conn, err := net.Dial("tcp", applyOK(t, entry, a))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "failover-group")
}
func TestSNIChildrenRouteRealTLS(t *testing.T) {
	cert, key, _ := testCertificate(t)
	certificate, _ := tls.X509KeyPair([]byte(cert), []byte(key))
	target := func(label string) nodeproto.ConfigTarget {
		ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					buf := make([]byte, 4)
					if _, err := io.ReadFull(conn, buf); err == nil {
						conn.Write([]byte(label))
					}
				}()
			}
		}()
		return nodeproto.ConfigTarget{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port}
	}
	e := NewEngine("127.0.0.1")
	defer e.Close()
	parent := nodeproto.ConfigRule{RuleID: 1, Protocol: "direct", Enable: true, InboundGroupID: 10}
	child := parent
	child.RuleID = 2
	child.IsSubRule = true
	child.ParentID = 1
	child.SNI = "one.test"
	child.Targets = []nodeproto.ConfigTarget{target("ONE!")}
	other := child
	other.RuleID = 3
	other.SNI = "two.test"
	other.Targets = []nodeproto.ConfigTarget{target("TWO!")}
	cfg := nodeproto.ConfigResponse{Full: true, Rules: []nodeproto.ConfigRule{parent, child, other}, DeviceGroupConfig: map[string]nodeproto.DeviceGroupConfig{"10": {Config: map[string]any{"tls_inbound_policy": 2, "tls_reject_empty_sni": true, "disable_udp": true}}}}
	applyNode(t, e, cfg)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(e.RunningRules()[0].Port))
	for _, name := range []string{"one.test", "two.test"} {
		conn, err := tls.Dial("tcp", address, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte("test"))
		buf := make([]byte, 4)
		_, err = io.ReadFull(conn, buf)
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(buf) != strings.ToUpper(name[:3])+"!" {
			t.Fatalf("wrong SNI target: %q", buf)
		}
	}
}
