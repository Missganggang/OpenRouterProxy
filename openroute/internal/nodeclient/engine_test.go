package nodeclient

import (
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openroute/openroute/internal/nodeproto"
)

func echoTarget(t *testing.T) nodeproto.ConfigTarget {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	port := tcp.Addr().(*net.TCPAddr).Port
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, source, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = udp.WriteToUDP(buffer[:n], source)
		}
	}()
	return nodeproto.ConfigTarget{Host: "127.0.0.1", Port: port}
}

func directConfig(target nodeproto.ConfigTarget) nodeproto.ConfigResponse {
	return nodeproto.ConfigResponse{Full: true, ConfigVersion: 1, Rules: []nodeproto.ConfigRule{{RuleID: 1, Protocol: "direct", Enable: true, Targets: []nodeproto.ConfigTarget{target}, TargetBalance: "failover"}}}
}

func applyOK(t *testing.T, e *Engine, cfg nodeproto.ConfigResponse) string {
	t.Helper()
	results := e.Apply(cfg)
	for _, result := range results {
		if result.Status != "normal" {
			t.Fatalf("apply failed: %+v", results)
		}
	}
	running := e.RunningRules()
	if len(running) != 1 {
		t.Fatalf("running rules: %+v", running)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(running[0].Port))
}

func exchange(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != payload {
		t.Fatalf("got %q, want %q", response, payload)
	}
}

func TestEngineTCPUDPAndTraffic(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	address := applyOK(t, e, directConfig(echoTarget(t)))
	for _, network := range []string{"tcp", "udp"} {
		conn, err := net.Dial(network, address)
		if err != nil {
			t.Fatal(err)
		}
		exchange(t, conn, "hello-node-"+network)
		_ = conn.Close()
	}
	deadline := time.Now().Add(time.Second)
	var stats nodeproto.ReportStats
	var in, out int64
	for time.Now().Before(deadline) {
		stats = e.Stats()
		for _, delta := range stats.RuleTraffic {
			in += delta.TrafficIn
			out += delta.TrafficOut
		}
		if in == 28 && out == 28 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if in != 28 || out != 28 || stats.NetIn != 28 || stats.NetOut != 28 {
		t.Fatalf("unexpected traffic in=%d out=%d stats=%+v", in, out, stats)
	}
	if next := e.Stats(); len(next.RuleTraffic) != 0 || next.NetIn != 28 {
		t.Fatalf("deltas not drained: %+v", next)
	}
}

func TestEngineKeepsUnchangedConnectionsAndRestoresSnapshot(t *testing.T) {
	cfg := directConfig(echoTarget(t))
	e := NewEngine("127.0.0.1")
	defer e.Close()
	address := applyOK(t, e, cfg)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "before")
	if current := applyOK(t, e, cfg); current != address {
		t.Fatal("unchanged rule changed listening port")
	}
	exchange(t, conn, "after")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection survived engine close")
	}
	var restored nodeproto.ConfigResponse
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	restarted := NewEngine("127.0.0.1")
	defer restarted.Close()
	conn2, err := net.Dial("tcp", applyOK(t, restarted, restored))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	exchange(t, conn2, "restored")
}

func TestEngineRemovalDisableAndIncremental(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	applyOK(t, e, cfg)
	e.Apply(nodeproto.ConfigResponse{Full: false})
	if len(e.RunningRules()) != 1 {
		t.Fatal("empty incremental removed rule")
	}
	e.Apply(nodeproto.ConfigResponse{RemovedRuleIDs: []uint64{1}})
	if len(e.RunningRules()) != 0 {
		t.Fatal("removed rule still running")
	}
	applyOK(t, e, cfg)
	cfg.Rules[0].Enable = false
	if results := e.Apply(cfg); results[0].Status != "normal" {
		t.Fatalf("disable: %+v", results)
	}
	if len(e.RunningRules()) != 0 {
		t.Fatal("disabled rule still running")
	}
	cfg.Rules[0].Enable = true
	applyOK(t, e, cfg)
	e.Apply(nodeproto.ConfigResponse{Full: true})
	if len(e.RunningRules()) != 0 {
		t.Fatal("full snapshot did not remove rule")
	}
}

func TestEngineRejectsUnsupportedWithoutBypassing(t *testing.T) {
	target := echoTarget(t)
	tests := []struct {
		name string
		edit func(*nodeproto.ConfigResponse)
	}{
		{"tunnel", func(c *nodeproto.ConfigResponse) { c.Rules[0].Protocol = "tls" }},
		{"outbound", func(c *nodeproto.ConfigResponse) { c.Rules[0].OutboundGroupID = 2 }},
		{"speed", func(c *nodeproto.ConfigResponse) { c.Rules[0].SpeedLimit = 1024 }},
		{"ip", func(c *nodeproto.ConfigResponse) { c.Rules[0].IPLimit = 1 }},
		{"chain", func(c *nodeproto.ConfigResponse) { c.Rules[0].ChainGroups = []uint64{2} }},
		{"reverse", func(c *nodeproto.ConfigResponse) { c.Rules[0].ReverseEnable = true }},
		{"sni", func(c *nodeproto.ConfigResponse) { c.Rules[0].SNI = "example.com" }},
		{"option", func(c *nodeproto.ConfigResponse) { c.Rules[0].Options = map[string]interface{}{"udp_over_tcp": true} }},
		{"group_acl", func(c *nodeproto.ConfigResponse) {
			c.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {Config: map[string]interface{}{"allowed_host": []string{".example.com"}}}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine("127.0.0.1")
			defer e.Close()
			cfg := directConfig(target)
			address := applyOK(t, e, cfg)
			tc.edit(&cfg)
			results := e.Apply(cfg)
			if results[0].Status != "failed" || results[0].Error == "" {
				t.Fatalf("unsupported rule reported success: %+v", results)
			}
			if len(e.RunningRules()) != 0 {
				t.Fatal("previous unrestricted rule still running")
			}
			conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Fatal("rejected rule still accepting connections")
			}
		})
	}
}

func TestEngineConnectionLimitAndUDPDisabled(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].ConnLimit = 1
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {Config: map[string]interface{}{"disable_udp": true}}}
	address := applyOK(t, e, cfg)
	first, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	exchange(t, first, "held")
	second, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetDeadline(time.Now().Add(time.Second))
	_, _ = second.Write([]byte("blocked"))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection limit was bypassed")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("UDP should not be listening: %v", err)
	}
	udp.Close()
}

func TestEngineUDPSessionsKeepClientsSeparate(t *testing.T) {
	e := NewEngine("127.0.0.1")
	defer e.Close()
	address := applyOK(t, e, directConfig(echoTarget(t)))
	first, err := net.Dial("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := net.Dial("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for i := 0; i < 3; i++ {
		exchange(t, first, "first-client")
		exchange(t, second, "other-client")
	}
}

func TestEngineTCPFailoverAndPortConflict(t *testing.T) {
	target := echoTarget(t)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	cfg := directConfig(target)
	cfg.Rules[0].Targets = append([]nodeproto.ConfigTarget{{Host: "127.0.0.1", Port: closedPort}}, cfg.Rules[0].Targets...)
	e := NewEngine("127.0.0.1")
	defer e.Close()
	address := applyOK(t, e, cfg)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "fallback-target")
	cfg.Rules[0].ListenPort = target.Port
	result := e.Apply(cfg)
	if result[0].Status != "failed" || !strings.Contains(result[0].Error, "TCP") {
		t.Fatalf("port conflict incorrectly accepted: %+v", result)
	}
}

func TestEngineRollsBackTCPWhenUDPPortInUse(t *testing.T) {
	occupied, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.LocalAddr().(*net.UDPAddr).Port
	cfg := directConfig(echoTarget(t))
	cfg.Rules[0].ListenPort = port
	e := NewEngine("127.0.0.1")
	defer e.Close()
	result := e.Apply(cfg)
	if result[0].Status != "failed" || !strings.Contains(result[0].Error, "UDP") {
		t.Fatalf("UDP conflict incorrectly accepted: %+v", result)
	}
	probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("partially started TCP listener leaked: %v", err)
	}
	probe.Close()
}

func TestEngineTargetSelectionAndTCPHalfClose(t *testing.T) {
	makeTarget := func(label string, weight int) nodeproto.ConfigTarget {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go func() {
					defer conn.Close()
					// The target waits for request EOF. A proxy that closes both
					// directions on client EOF loses this response.
					payload, err := io.ReadAll(conn)
					if err == nil {
						_, _ = conn.Write(append([]byte(label), payload...))
					}
				}()
			}
		}()
		return nodeproto.ConfigTarget{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Weight: weight}
	}
	a, b := makeTarget("A", 3), makeTarget("B", 1)
	for _, tc := range []struct{ balance, want string }{{"round_robin", "ABAB"}, {"weighted", "AAAB"}} {
		t.Run(tc.balance, func(t *testing.T) {
			e := NewEngine("127.0.0.1")
			defer e.Close()
			cfg := directConfig(a)
			cfg.Rules[0].Targets = []nodeproto.ConfigTarget{a, b}
			cfg.Rules[0].TargetBalance = tc.balance
			address := applyOK(t, e, cfg)
			var labels string
			for i := 0; i < 4; i++ {
				conn, err := net.Dial("tcp", address)
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, err = conn.Write([]byte("payload"))
				if err != nil {
					t.Fatal(err)
				}
				if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				response, err := io.ReadAll(conn)
				conn.Close()
				if err != nil {
					t.Fatal(err)
				}
				if len(response) != 8 || string(response[1:]) != "payload" {
					t.Fatalf("lost half-close response: %q", response)
				}
				labels += string(response[0])
			}
			if labels != tc.want {
				t.Fatalf("target selection got %q, want %q", labels, tc.want)
			}
		})
	}
}
