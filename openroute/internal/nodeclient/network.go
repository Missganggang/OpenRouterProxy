package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Source addresses select an IP, while Interface constrains the actual Linux
// egress device. A mark still needs an administrator-provided policy route.
type dialPolicy struct {
	Bind4, Bind6, Interface string
	Mark                    uint32
	MarkSet                 bool
}
type networkConfig struct {
	RuleBind, TunnelBind []string
	Target, Tunnel       dialPolicy
}

// ValidateNetworkEnvironment checks local options, platform support and socket
// rights without sending packets. DNS follows the system resolver settings.
func ValidateNetworkEnvironment(bindInbound string) error {
	_, err := loadNetworkConfig(bindInbound)
	return err
}
func loadNetworkConfig(bindInbound string) (networkConfig, error) {
	var cfg networkConfig
	var err error
	if bindInbound == "" {
		bindInbound = "0.0.0.0"
	}
	cfg.RuleBind, err = bindAddresses(bindInbound)
	if err != nil {
		return cfg, err
	}
	cfg.TunnelBind = cfg.RuleBind
	if value := os.Getenv("TUNNEL_BIND_INBOUND"); value != "" {
		var address string
		address, err = literalBind("TUNNEL_BIND_INBOUND", value, 0)
		if err != nil {
			return cfg, err
		}
		cfg.TunnelBind = []string{address}
	}
	cfg.Target, err = readDialPolicy("BIND_OUTBOUND_4", "BIND_OUTBOUND_6", "OUTBOUND_FWMARK", "")
	if err != nil {
		return cfg, err
	}
	// One dedicated option selects a complete independent tunnel policy.
	dedicated := false
	for _, key := range []string{"TUNNEL_BIND_OUTBOUND_4", "TUNNEL_BIND_OUTBOUND_6", "TUNNEL_FWMARK", "TUNNEL_INTERFACE"} {
		if os.Getenv(key) != "" {
			dedicated = true
		}
	}
	cfg.Tunnel = cfg.Target
	if dedicated {
		cfg.Tunnel, err = readDialPolicy("TUNNEL_BIND_OUTBOUND_4", "TUNNEL_BIND_OUTBOUND_6", "TUNNEL_FWMARK", "TUNNEL_INTERFACE")
		if err != nil {
			return cfg, err
		}
	}
	for _, policy := range []dialPolicy{cfg.Target, cfg.Tunnel} {
		if err = validateSocketPolicy(policy); err != nil {
			return cfg, err
		}
	}
	if cfg.Tunnel.Interface != "" {
		iface, lookupErr := net.InterfaceByName(cfg.Tunnel.Interface)
		if lookupErr != nil {
			return cfg, fmt.Errorf("TUNNEL_INTERFACE: %w", lookupErr)
		}
		addresses := append([]string{cfg.Tunnel.Bind4, cfg.Tunnel.Bind6}, cfg.TunnelBind...)
		for _, value := range addresses {
			if value == "" {
				continue
			}
			ip := net.ParseIP(value)
			if ip.IsUnspecified() {
				continue
			}
			if !interfaceHasIP(iface, ip) {
				return cfg, fmt.Errorf("tunnel address %s does not belong to interface %s", value, iface.Name)
			}
		}
	}
	return cfg, nil
}
func literalBind(name, value string, family int) (string, error) {
	ip := net.ParseIP(value)
	if ip == nil {
		return "", fmt.Errorf("%s must be one literal IP address (interface names and lists are unsupported)", name)
	}
	if family == 4 && ip.To4() == nil {
		return "", fmt.Errorf("%s requires an IPv4 address", name)
	}
	if family == 6 && ip.To4() != nil {
		return "", fmt.Errorf("%s requires an IPv6 address", name)
	}
	return ip.String(), nil
}
func readDialPolicy(bind4, bind6, mark, iface string) (dialPolicy, error) {
	var p dialPolicy
	var err error
	if value := os.Getenv(bind4); value != "" {
		p.Bind4, err = literalBind(bind4, value, 4)
		if err != nil {
			return p, err
		}
	}
	if value := os.Getenv(bind6); value != "" {
		p.Bind6, err = literalBind(bind6, value, 6)
		if err != nil {
			return p, err
		}
	}
	if value := os.Getenv(mark); value != "" {
		n, parseErr := strconv.ParseUint(value, 0, 32)
		if parseErr != nil {
			return p, fmt.Errorf("%s must be an unsigned 32-bit mark: %w", mark, parseErr)
		}
		p.Mark = uint32(n)
		p.MarkSet = true
	}
	if iface != "" {
		p.Interface = os.Getenv(iface)
		if p.Interface != "" {
			if strings.ContainsAny(p.Interface, " ,\t\r\n") {
				return p, fmt.Errorf("%s must be one interface name", iface)
			}
			if _, err = net.InterfaceByName(p.Interface); err != nil {
				return p, fmt.Errorf("%s: %w", iface, err)
			}
		}
	}
	return p, nil
}
func interfaceHasIP(iface *net.Interface, ip net.IP) bool {
	addresses, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		local, _, err := net.ParseCIDR(address.String())
		if err == nil && local.Equal(ip) {
			return true
		}
	}
	return false
}

// Resolve names before choosing the source family; an AAAA-only hostname must
// never silently ignore BIND_OUTBOUND_6 by taking the IPv4 configuration.
func dialWithPolicy(ctx context.Context, network, address string, p dialPolicy, timeout time.Duration) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6", "udp", "udp4", "udp6":
	default:
		return nil, fmt.Errorf("unsupported network %q", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addresses := []net.IPAddr{}
	if ip := net.ParseIP(host); ip != nil {
		addresses = append(addresses, net.IPAddr{IP: ip})
	} else {
		addresses, err = net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	candidates := make([]net.IPAddr, 0, len(addresses))
	for _, remote := range addresses {
		is4 := remote.IP.To4() != nil
		if strings.HasSuffix(network, "4") && !is4 || strings.HasSuffix(network, "6") && is4 {
			continue
		}
		candidates = append(candidates, remote)
	}
	baseDialer := net.Dialer{KeepAlive: 30 * time.Second}
	if err := configureSocketPolicy(&baseDialer, p); err != nil {
		return nil, err
	}
	return dialIPCandidates(ctx, candidates, func(attemptCtx context.Context, remote net.IPAddr) (net.Conn, error) {
		source := p.Bind6
		if remote.IP.To4() != nil {
			source = p.Bind4
		}
		dialer := baseDialer
		if source != "" {
			ip := net.ParseIP(source)
			if strings.HasPrefix(network, "udp") {
				dialer.LocalAddr = &net.UDPAddr{IP: ip}
			} else {
				dialer.LocalAddr = &net.TCPAddr{IP: ip}
			}
		}
		remoteHost := remote.IP.String()
		if remote.Zone != "" {
			remoteHost += "%" + remote.Zone
		}
		return dialer.DialContext(attemptCtx, network, net.JoinHostPort(remoteHost, port))
	})
}

// Divide the remaining total deadline between untried addresses. A blackholed
// first address must not consume the time reserved for reachable alternatives.
func dialIPCandidates(ctx context.Context, addresses []net.IPAddr, attempt func(context.Context, net.IPAddr) (net.Conn, error)) (net.Conn, error) {
	var last error = errors.New("no destination matches the requested address family")
	for i, remote := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx := ctx
		cancel := func() {}
		if deadline, ok := ctx.Deadline(); ok {
			now := time.Now()
			attemptDeadline := now.Add(deadline.Sub(now) / time.Duration(len(addresses)-i))
			attemptCtx, cancel = context.WithDeadline(ctx, attemptDeadline)
		}
		conn, dialErr := attempt(attemptCtx, remote)
		cancel()
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}
func (f *forwarder) dialTargetAddress(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
	return dialWithPolicy(ctx, network, address, f.network.Target, timeout)
}
func (f *forwarder) dialTunnelAddress(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
	return dialWithPolicy(ctx, network, address, f.network.Tunnel, timeout)
}
func tunnelListenConfig(p dialPolicy) (net.ListenConfig, error) {
	dialer := net.Dialer{}
	if err := configureSocketPolicy(&dialer, p); err != nil {
		return net.ListenConfig{}, err
	}
	return net.ListenConfig{Control: func(network, address string, raw syscall.RawConn) error {
		if dialer.Control != nil {
			return dialer.Control(network, address, raw)
		}
		return nil
	}}, nil
}
func listenTunnelTCP(address string, p dialPolicy) (net.Listener, error) {
	cfg, err := tunnelListenConfig(p)
	if err != nil {
		return nil, err
	}
	return cfg.Listen(context.Background(), "tcp", address)
}
func listenTunnelUDP(address string, p dialPolicy) (*net.UDPConn, error) {
	cfg, err := tunnelListenConfig(p)
	if err != nil {
		return nil, err
	}
	conn, err := cfg.ListenPacket(context.Background(), "udp", address)
	if err != nil {
		return nil, err
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close()
		return nil, errors.New("unexpected UDP listener type")
	}
	return udp, nil
}
