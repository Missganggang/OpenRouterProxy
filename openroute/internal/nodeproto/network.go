package nodeproto

import (
	"fmt"
	"net"
	"strings"
)

// NodeNetworkConfig describes local overrides advertised by a node. Zero ports
// inherit panel/default listeners; connect ports only change the peer endpoint,
// allowing an external NAT port to differ from the local listening port.
type NodeNetworkConfig struct {
	ConnectHost       string `json:"connect_host" yaml:"connect-host"`
	DirectPort        int    `json:"direct_port" yaml:"direct-port"`
	WsPort            int    `json:"ws_port" yaml:"ws-port"`
	TlsPort           int    `json:"tls_port" yaml:"tls-port"`
	UdpPort           int    `json:"udp_port" yaml:"udp-port"`
	RevPort           int    `json:"rev_port" yaml:"rev-port"`
	ConnectDirectPort int    `json:"connect_direct_port" yaml:"connect-direct-port"`
	ConnectWsPort     int    `json:"connect_ws_port" yaml:"connect-ws-port"`
	ConnectTlsPort    int    `json:"connect_tls_port" yaml:"connect-tls-port"`
	ConnectUdpPort    int    `json:"connect_udp_port" yaml:"connect-udp-port"`
	ConnectRevPort    int    `json:"connect_rev_port" yaml:"connect-rev-port"`
}

func (n *NodeNetworkConfig) Normalize() error {
	host, err := NormalizeConnectHost(n.ConnectHost)
	if err != nil {
		return err
	}
	n.ConnectHost = host
	for name, value := range map[string]int{
		"direct-port": n.DirectPort, "ws-port": n.WsPort, "tls-port": n.TlsPort, "udp-port": n.UdpPort, "rev-port": n.RevPort,
		"connect-direct-port": n.ConnectDirectPort, "connect-ws-port": n.ConnectWsPort, "connect-tls-port": n.ConnectTlsPort, "connect-udp-port": n.ConnectUdpPort, "connect-rev-port": n.ConnectRevPort,
	} {
		if value < 0 || value > 65535 {
			return fmt.Errorf("%s must be 0 (inherit) or a port from 1 to 65535", name)
		}
	}
	return nil
}

// NormalizeConnectHost accepts a single reachable-address identifier, never a
// URL, CIDR, wildcard bind address or host:port pair.
func NormalizeConnectHost(value string) (string, error) {
	host := strings.TrimSpace(value)
	if host == "" {
		return "", nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return "", fmt.Errorf("connect-host must identify one peer, not a wildcard or multicast address")
		}
		return ip.String(), nil
	}
	if len(host) > 253 || strings.ContainsAny(host, ":/[]%\\ \t\r\n") {
		return "", fmt.Errorf("connect-host must be a bare IP address or hostname without a port or URL scheme")
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid connect-host hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid connect-host hostname")
			}
		}
	}
	return strings.ToLower(host), nil
}
