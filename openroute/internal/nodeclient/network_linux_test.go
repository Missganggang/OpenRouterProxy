//go:build linux

package nodeclient

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"syscall"
	"testing"
	"time"
)

func assertSocketPolicy(t *testing.T, conn syscall.Conn, mark int, iface string) {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var actualMark int
	var actualInterface string
	var inspectErr error
	err = raw.Control(func(fd uintptr) {
		actualMark, inspectErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
		if inspectErr == nil {
			actualInterface, inspectErr = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		}
	})
	if err != nil || inspectErr != nil {
		t.Fatalf("socket inspect: %v %v", err, inspectErr)
	}
	if actualMark != mark || actualInterface != iface {
		t.Fatalf("socket mark=%d device=%q; expected %d/%q", actualMark, actualInterface, mark, iface)
	}
}
func TestLinuxTunnelSocketMarkAndDeviceIncludeReturnTraffic(t *testing.T) {
	clearNetworkEnv(t)
	policy := dialPolicy{Interface: "lo", Mark: 0x4f52, MarkSet: true, Bind4: "127.0.0.1"}
	if err := validateSocketPolicy(policy); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("SO_MARK/SO_BINDTODEVICE require capabilities: %v", err)
		}
		t.Fatal(err)
	}
	tcp, err := listenTunnelTCP("127.0.0.1:0", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	assertSocketPolicy(t, tcp.(*net.TCPListener), int(policy.Mark), "lo")
	accepted := make(chan net.Conn, 1)
	go func() { conn, _ := tcp.Accept(); accepted <- conn }()
	conn, err := dialWithPolicy(context.Background(), "tcp", tcp.Addr().String(), policy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertSocketPolicy(t, conn.(*net.TCPConn), int(policy.Mark), "lo")
	peer := <-accepted
	if peer == nil {
		t.Fatal("accept failed")
	}
	defer peer.Close()
	assertSocketPolicy(t, peer.(*net.TCPConn), int(policy.Mark), "lo")
	udp, err := listenTunnelUDP("127.0.0.1:0", policy)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	assertSocketPolicy(t, udp, int(policy.Mark), "lo")
	udpConn, err := dialWithPolicy(context.Background(), "udp", udp.LocalAddr().String(), policy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer udpConn.Close()
	assertSocketPolicy(t, udpConn.(*net.UDPConn), int(policy.Mark), "lo")
	udpConn.Write([]byte("private"))
	buffer := make([]byte, 32)
	_ = udp.SetReadDeadline(time.Now().Add(time.Second))
	n, source, err := udp.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	udp.WriteToUDP(buffer[:n], source)
	_ = udpConn.SetReadDeadline(time.Now().Add(time.Second))
	if n, err = udpConn.Read(buffer); err != nil || string(buffer[:n]) != "private" {
		t.Fatalf("marked UDP return failed: %q %v", buffer[:n], err)
	}
}
func TestLinuxTunnelDoesNotInheritTargetMarkWhenDedicatedSourceIsSet(t *testing.T) {
	clearNetworkEnv(t)
	t.Setenv("OUTBOUND_FWMARK", "0x4f52")
	t.Setenv("TUNNEL_BIND_OUTBOUND_4", "127.0.0.1")
	cfg, err := loadNetworkConfig("127.0.0.1")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("SO_MARK requires capabilities: %v", err)
		}
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	for _, tc := range []struct {
		policy dialPolicy
		mark   int
	}{{cfg.Target, 0x4f52}, {cfg.Tunnel, 0}} {
		conn, err := dialWithPolicy(context.Background(), "tcp", listener.Addr().String(), tc.policy, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		assertSocketPolicy(t, conn.(*net.TCPConn), tc.mark, "")
		conn.Close()
	}
}
func TestLinuxMissingTunnelInterfaceDoesNotFallBack(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := dialWithPolicy(context.Background(), "tcp", listener.Addr().String(), dialPolicy{Interface: "missing-openroute-nic"}, time.Second)
	if conn != nil {
		conn.Close()
		t.Fatal("missing dedicated NIC silently fell back")
	}
	if err == nil {
		t.Fatal("missing NIC accepted")
	}
}
