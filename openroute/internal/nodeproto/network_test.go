package nodeproto

import "testing"

func TestNetworkAddressAndPortValidation(t *testing.T) {
	for _, host := range []string{"", "10.88.0.2", "fd88::2", "exit.example.com", "EXIT.EXAMPLE.COM."} {
		n := NodeNetworkConfig{ConnectHost: host, ConnectTlsPort: 65535}
		if err := n.Normalize(); err != nil {
			t.Fatalf("valid host %q: %v", host, err)
		}
	}
	for _, host := range []string{"https://10.88.0.2", "10.88.0.2:28080", "10.88.0.0/24", "0.0.0.0", "::", "ff02::1", "[fd88::2]", "fe80::2%eth1", "bad name", "-bad.example", "a;cmd"} {
		if _, err := NormalizeConnectHost(host); err == nil {
			t.Errorf("invalid host %q accepted", host)
		}
	}
	for _, n := range []NodeNetworkConfig{{DirectPort: -1}, {ConnectUdpPort: 65536}, {RevPort: 65536}} {
		if n.Normalize() == nil {
			t.Error("invalid port accepted")
		}
	}
}
