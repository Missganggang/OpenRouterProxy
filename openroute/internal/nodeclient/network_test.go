package nodeclient

import (
	"context"
	"errors"
	"github.com/openroute/openroute/internal/nodeproto"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func clearNetworkEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"BIND_INBOUND", "BIND_OUTBOUND_4", "BIND_OUTBOUND_6", "OUTBOUND_FWMARK", "TUNNEL_BIND_INBOUND", "TUNNEL_BIND_OUTBOUND_4", "TUNNEL_BIND_OUTBOUND_6", "TUNNEL_FWMARK", "TUNNEL_INTERFACE"} {
		t.Setenv(key, "")
	}
}
func observedTarget(t *testing.T) (nodeproto.ConfigTarget, <-chan string, <-chan string) {
	t.Helper()
	tcpSeen, udpSeen := make(chan string, 32), make(chan string, 32)
	var tcp net.Listener
	var udp *net.UDPConn
	var err error
	port := 0
	for {
		tcp, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port = tcp.Addr().(*net.TCPAddr).Port
		udp, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if err == nil {
			break
		}
		tcp.Close()
	}
	t.Cleanup(func() { tcp.Close(); udp.Close() })
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			select {
			case tcpSeen <- sourceIP(conn.RemoteAddr().String()):
			default:
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, source, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			select {
			case udpSeen <- source.IP.String():
			default:
			}
			udp.WriteToUDP(buffer[:n], source)
		}
	}()
	return nodeproto.ConfigTarget{Host: "127.0.0.1", Port: port}, tcpSeen, udpSeen
}
func expectSource(t *testing.T, observed <-chan string, want string) {
	t.Helper()
	select {
	case source := <-observed:
		if source != want {
			t.Fatalf("source=%s, want %s", source, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no connection reached target")
	}
}

func TestDialIPCandidatesTriesReachableAddressAfterTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	addresses := []net.IPAddr{{IP: net.ParseIP("2001:db8::1")}, {IP: net.ParseIP("192.0.2.1")}}
	want, peer := net.Pipe()
	defer want.Close()
	defer peer.Close()
	attempts := 0
	conn, err := dialIPCandidates(ctx, addresses, func(attemptCtx context.Context, remote net.IPAddr) (net.Conn, error) {
		attempts++
		if !remote.IP.Equal(addresses[attempts-1].IP) {
			t.Fatalf("unexpected candidate: %v", remote)
		}
		attemptDeadline, ok := attemptCtx.Deadline()
		if !ok || attemptDeadline.After(deadline) {
			t.Fatal("attempt escaped the original total deadline")
		}
		if attempts == 1 {
			if !attemptDeadline.Before(deadline) {
				t.Fatal("first address consumed the entire timeout")
			}
			<-attemptCtx.Done() // A controlled blackhole; no external network.
			return nil, attemptCtx.Err()
		}
		if attemptCtx.Err() != nil {
			t.Fatal("reachable alternative has no remaining time")
		}
		return want, nil
	})
	if err != nil || conn != want || attempts != 2 {
		t.Fatalf("fallback failed: conn=%v attempts=%d err=%v", conn, attempts, err)
	}
	if ctx.Err() != nil {
		t.Fatal("first timeout canceled the complete dial operation")
	}
}

func TestDialIPCandidatesPreservesTotalDeadlineAndCancellation(t *testing.T) {
	addresses := []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}, {IP: net.ParseIP("192.0.2.2")}, {IP: net.ParseIP("192.0.2.3")}}
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()
		deadline, _ := ctx.Deadline()
		attempts := 0
		_, err := dialIPCandidates(ctx, addresses, func(attemptCtx context.Context, _ net.IPAddr) (net.Conn, error) {
			attempts++
			attemptDeadline, ok := attemptCtx.Deadline()
			if !ok || attemptDeadline.After(deadline) {
				t.Fatal("attempt extended the total timeout")
			}
			<-attemptCtx.Done()
			return nil, attemptCtx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || attempts != len(addresses) {
			t.Fatalf("attempts=%d err=%v", attempts, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		attempts := 0
		_, err := dialIPCandidates(ctx, addresses, func(attemptCtx context.Context, _ net.IPAddr) (net.Conn, error) {
			attempts++
			cancel()
			<-attemptCtx.Done()
			return nil, attemptCtx.Err()
		})
		if !errors.Is(err, context.Canceled) || attempts != 1 {
			t.Fatalf("cancellation should prevent further attempts: attempts=%d err=%v", attempts, err)
		}
	})
}

func TestLegacyInboundAddressListAndTunnelOverride(t *testing.T) {
	clearNetworkEnv(t)
	for _, tunnelBind := range []string{"", "127.0.0.4"} {
		t.Run("tunnel="+tunnelBind, func(t *testing.T) {
			t.Setenv("TUNNEL_BIND_INBOUND", tunnelBind)
			a, b := pairConfigs(t, "direct")
			a.Rules[0].ListenPort = testPort(t)
			gatewayHost := "127.0.0.2"
			if tunnelBind != "" {
				gatewayHost = tunnelBind
			}
			for key, group := range a.DeviceGroupConfig {
				for i := range group.Peers {
					group.Peers[i].Host = gatewayHost
				}
				a.DeviceGroupConfig[key] = group
			}
			b.DeviceGroupConfig = a.DeviceGroupConfig
			entry, exit := NewEngine("127.0.0.1, 127.0.0.2"), NewEngine("127.0.0.1, 127.0.0.2")
			defer entry.Close()
			defer exit.Close()
			applyNode(t, exit, b)
			applyNode(t, entry, a)
			for _, bind := range []string{"127.0.0.1", "127.0.0.2"} {
				address := net.JoinHostPort(bind, strconv.Itoa(a.Rules[0].ListenPort))
				for _, network := range []string{"tcp", "udp"} {
					conn, err := net.Dial(network, address)
					if err != nil {
						t.Fatal(err)
					}
					exchange(t, conn, "legacy-bind-"+bind+"-"+network)
					conn.Close()
				}
			}
			for _, kind := range []string{"direct", "udp"} {
				seen := map[string]bool{}
				for _, gateway := range exit.gateways {
					if gateway.kind == kind {
						seen[sourceIP(gateway.address)] = true
					}
				}
				if tunnelBind == "" {
					if len(seen) != 2 || !seen["127.0.0.1"] || !seen["127.0.0.2"] {
						t.Fatalf("%s gateways did not inherit all rule addresses: %v", kind, seen)
					}
				} else if len(seen) != 1 || !seen[tunnelBind] {
					t.Fatalf("%s gateways ignored dedicated bind: %v", kind, seen)
				}
			}
		})
	}
}

func TestLegacyInboundInterfaceExpandsToWorkingListeners(t *testing.T) {
	clearNetworkEnv(t)
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		e := NewEngine(iface.Name)
		defer e.Close()
		cfg := directConfig(echoTarget(t))
		applyNode(t, e, cfg)
		f := e.rules[1]
		if len(f.listeners) == 0 || len(f.listeners) != len(f.network.RuleBind) {
			t.Fatalf("interface did not expand to listeners: %+v", f.network.RuleBind)
		}
		for _, listener := range f.listeners {
			for _, network := range []string{"tcp", "udp"} {
				conn, err := net.Dial(network, listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				exchange(t, conn, "interface-"+iface.Name)
				conn.Close()
			}
		}
		return
	}
	t.Skip("no active loopback interface")
}

func TestTunnelDialPolicyInheritanceAndIsolation(t *testing.T) {
	clearNetworkEnv(t)
	t.Setenv("BIND_OUTBOUND_4", "127.0.0.3")
	target, tcpSeen, udpSeen := observedTarget(t)
	address := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	for _, dedicated := range []bool{false, true} {
		t.Run(strconv.FormatBool(dedicated), func(t *testing.T) {
			if dedicated {
				t.Setenv("TUNNEL_BIND_OUTBOUND_6", "::1")
			}
			cfg, err := loadNetworkConfig("127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			f := &forwarder{network: cfg}
			want := "127.0.0.3"
			if dedicated {
				want = "127.0.0.1"
			}
			for _, network := range []string{"tcp", "udp"} {
				conn, err := f.dialTunnelAddress(context.Background(), network, address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				exchange(t, conn, "tunnel-source")
				conn.Close()
				if network == "tcp" {
					expectSource(t, tcpSeen, want)
				} else {
					expectSource(t, udpSeen, want)
				}
			}
			conn, err := f.dialTargetAddress(context.Background(), "tcp", address, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			exchange(t, conn, "target-source")
			conn.Close()
			expectSource(t, tcpSeen, "127.0.0.3")
		})
	}
}
func TestPrivateTunnelListenersAndTargetSourceAreIndependent(t *testing.T) {
	clearNetworkEnv(t)
	t.Setenv("BIND_OUTBOUND_4", "127.0.0.3")
	t.Setenv("TUNNEL_BIND_OUTBOUND_4", "127.0.0.2")
	t.Setenv("TUNNEL_BIND_INBOUND", "127.0.0.4")
	target, tcpSeen, udpSeen := observedTarget(t)
	a, b := pairConfigs(t, "direct")
	a.Rules[0].Targets = []nodeproto.ConfigTarget{target}
	b.Rules[0].Targets = a.Rules[0].Targets
	for key, g := range a.DeviceGroupConfig {
		for i := range g.Peers {
			g.Peers[i].Host = "127.0.0.4"
		}
		a.DeviceGroupConfig[key] = g
	}
	b.DeviceGroupConfig = a.DeviceGroupConfig
	entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
	defer entry.Close()
	defer exit.Close()
	applyNode(t, exit, b)
	address := applyOK(t, entry, a)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "dedicated-tcp")
	expectSource(t, tcpSeen, "127.0.0.3")
	found := false
	for _, s := range exit.gateways {
		if s.kind != "direct" {
			continue
		}
		if sourceIP(s.listener.Addr().String()) != "127.0.0.4" {
			t.Fatal("gateway inherited public rule bind")
		}
		s.mu.Lock()
		for c := range s.conns {
			if sourceIP(c.RemoteAddr().String()) == "127.0.0.2" {
				found = true
			}
		}
		s.mu.Unlock()
	}
	conn.Close()
	if !found {
		t.Fatal("TCP tunnel did not use dedicated source")
	}
	conn, err = net.Dial("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, conn, "dedicated-native-udp")
	expectSource(t, udpSeen, "127.0.0.3")
	found = false
	for _, s := range exit.gateways {
		if s.packet == nil {
			continue
		}
		if sourceIP(s.packet.LocalAddr().String()) != "127.0.0.4" {
			t.Fatal("UDP reply socket is not pinned to private listener IP")
		}
		s.mu.Lock()
		for _, session := range s.sessions {
			if session.remote.IP.String() == "127.0.0.2" {
				found = true
			}
		}
		s.mu.Unlock()
	}
	conn.Close()
	if !found {
		t.Fatal("UDP tunnel did not use dedicated source")
	}
}
func TestReverseConnectionUsesDedicatedTunnelSource(t *testing.T) {
	clearNetworkEnv(t)
	t.Setenv("BIND_OUTBOUND_4", "127.0.0.3")
	t.Setenv("TUNNEL_BIND_OUTBOUND_4", "127.0.0.2")
	t.Setenv("TUNNEL_BIND_INBOUND", "127.0.0.4")
	target, tcpSeen, _ := observedTarget(t)
	a, b := pairConfigs(t, "tls")
	a.Rules[0].ReverseEnable = true
	b.Rules[0].ReverseEnable = true
	a.Rules[0].Targets = []nodeproto.ConfigTarget{target}
	b.Rules[0].Targets = a.Rules[0].Targets
	for key, g := range a.DeviceGroupConfig {
		for i := range g.Peers {
			g.Peers[i].Host = "127.0.0.4"
		}
		a.DeviceGroupConfig[key] = g
	}
	b.DeviceGroupConfig = a.DeviceGroupConfig
	entry, exit := NewEngine("127.0.0.1"), NewEngine("127.0.0.1")
	defer entry.Close()
	defer exit.Close()
	address := applyOK(t, entry, a)
	applyNode(t, exit, b)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exchange(t, conn, "reverse-private-source")
	expectSource(t, tcpSeen, "127.0.0.3")
	found := false
	for _, s := range entry.gateways {
		if s.kind != "reverse" {
			continue
		}
		s.mu.Lock()
		for c := range s.conns {
			if sourceIP(c.RemoteAddr().String()) == "127.0.0.2" {
				found = true
			}
		}
		s.mu.Unlock()
	}
	if !found {
		t.Fatal("reverse connection used target/public source")
	}
}
func TestHealthTargetAndTunnelFollowTheirOwnPolicies(t *testing.T) {
	clearNetworkEnv(t)
	t.Setenv("BIND_OUTBOUND_4", "127.0.0.3")
	t.Setenv("TUNNEL_BIND_OUTBOUND_4", "127.0.0.2")
	target, tcpSeen, _ := observedTarget(t)
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(target)
	cfg.DeviceGroupConfig = map[string]nodeproto.DeviceGroupConfig{"0": {HealthCheckEnable: true, HealthCheckInterval: 1, HealthCheckTimeout: 1, HealthCheckSuccCount: 1}}
	applyOK(t, e, cfg)
	expectSource(t, tcpSeen, "127.0.0.3")
	e.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seen := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		var req tunnelRequest
		if readMessage(conn, &req) == nil && req.Action == "probe" {
			seen <- sourceIP(conn.RemoteAddr().String())
			writeMessage(conn, tunnelReply{})
		}
	}()
	a, _ := pairConfigs(t, "direct")
	g := a.DeviceGroupConfig["20"]
	g.HealthCheckEnable = true
	g.HealthCheckInterval = 1
	g.HealthCheckTimeout = 1
	g.Peers[0].DirectPort = listener.Addr().(*net.TCPAddr).Port
	a.DeviceGroupConfig["20"] = g
	e = NewEngine("127.0.0.1")
	defer e.Close()
	applyOK(t, e, a)
	expectSource(t, seen, "127.0.0.2")
}
func TestHostnameChoosesSourceAfterIPv6Resolution(t *testing.T) {
	clearNetworkEnv(t)
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer listener.Close()
	ips, err := net.LookupIP("localhost")
	if err != nil {
		t.Fatal(err)
	}
	has6 := false
	for _, ip := range ips {
		if ip.Equal(net.IPv6loopback) {
			has6 = true
		}
	}
	if !has6 {
		t.Skip("localhost has no IPv6 entry")
	}
	p := dialPolicy{Bind4: "127.0.0.3", Bind6: "::1"}
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := listener.Accept(); accepted <- conn }()
	conn, err := dialWithPolicy(context.Background(), "tcp6", net.JoinHostPort("localhost", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)), p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-accepted
	defer peer.Close()
	if sourceIP(peer.RemoteAddr().String()) != "::1" {
		t.Fatal("hostname lost IPv6 source binding")
	}
	udp, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	conn, err = dialWithPolicy(context.Background(), "udp6", net.JoinHostPort("localhost", strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port)), p, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("v6"))
	_ = udp.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 10)
	_, source, err := udp.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !source.IP.Equal(net.IPv6loopback) {
		t.Fatal("UDP hostname lost IPv6 source binding")
	}
}
func TestInvalidNetworkEnvironmentKeepsWorkingConfiguration(t *testing.T) {
	clearNetworkEnv(t)
	e := NewEngine("127.0.0.1")
	defer e.Close()
	cfg := directConfig(echoTarget(t))
	address := applyOK(t, e, cfg)
	for _, tc := range []struct{ key, value string }{{"BIND_OUTBOUND_4", "::1"}, {"BIND_OUTBOUND_6", "127.0.0.1"}, {"TUNNEL_BIND_INBOUND", "eth0"}, {"TUNNEL_BIND_OUTBOUND_4", "127.0.0.1,127.0.0.2"}, {"TUNNEL_FWMARK", "4294967296"}, {"TUNNEL_INTERFACE", "missing-openroute-nic"}} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			cfg.ConfigVersion++
			result := e.Apply(cfg)
			if len(result) == 0 || result[0].Status != "failed" || !strings.Contains(result[0].Error, tc.key) {
				t.Fatalf("bad environment accepted: %+v", result)
			}
			conn, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			exchange(t, conn, "old-working-route")
		})
	}
}
