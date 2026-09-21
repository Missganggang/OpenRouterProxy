package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/openroute/openroute/internal/nodeclient"
	"github.com/openroute/openroute/internal/nodeproto"
)

func TestNetworkCLIOverridesFileAndPreservesExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("base-url: https://panel.example\ntoken: secret\nconnect-host: 10.0.0.1\nws-port: 1111\ntls-port: 2222\nconnect-tls-port: 3333\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts, err := parseOptions([]string{"-c", path, "--connect-host=", "--ws-port=0", "--connect-tls-port", "443"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeclient.LoadConfigWithOverrides(opts.path, opts.baseURL, opts.token, opts.overrides)
	want := nodeproto.NodeNetworkConfig{TlsPort: 2222, ConnectTlsPort: 443}
	if err != nil || cfg.Network != want {
		t.Fatalf("CLI precedence failed: %+v, %v", cfg.Network, err)
	}
}

func TestNetworkCLIAllSupportedPorts(t *testing.T) {
	opts, err := parseOptions([]string{"--connect-host", "10.0.0.2", "--direct-port=1001", "--ws-port=1002", "--tls-port=1003", "--udp-port=1004", "--rev-port=1005", "--connect-direct-port=2001", "--connect-ws-port=2002", "--connect-tls-port=2003", "--connect-udp-port=2004", "--connect-rev-port=2005"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeclient.LoadConfigWithOverrides(filepath.Join(t.TempDir(), "absent.yml"), "https://panel.example", "secret", opts.overrides)
	want := nodeproto.NodeNetworkConfig{ConnectHost: "10.0.0.2", DirectPort: 1001, WsPort: 1002, TlsPort: 1003, UdpPort: 1004, RevPort: 1005, ConnectDirectPort: 2001, ConnectWsPort: 2002, ConnectTlsPort: 2003, ConnectUdpPort: 2004, ConnectRevPort: 2005}
	if err != nil || cfg.Network != want {
		t.Fatalf("CLI flags missing: %+v, %v", cfg.Network, err)
	}
}

func TestNetworkCLIPortsAlwaysUseDecimal(t *testing.T) {
	opts, err := parseOptions([]string{"--ws-port=00123", "--tls-port", "08", "--connect-udp-port=00009"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeclient.LoadConfigWithOverrides(filepath.Join(t.TempDir(), "absent.yml"), "https://panel.example", "secret", opts.overrides)
	if err != nil || cfg.Network.WsPort != 123 || cfg.Network.TlsPort != 8 || cfg.Network.ConnectUdpPort != 9 {
		t.Fatalf("leading zero changed decimal port semantics: %+v, %v", cfg.Network, err)
	}
	if _, err := parseOptions([]string{"--ws-port=0x50"}, io.Discard); err == nil {
		t.Fatal("hexadecimal CLI port accepted")
	}
}

func TestCheckValidatesNetworkAndUnknownArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yml")
	base := []string{"-c", path, "-u", "https://panel.invalid", "-t", "secret", "--check"}
	for _, flags := range [][]string{{"--connect-host=https://peer"}, {"--connect-host=10.0.0.1:8443"}, {"--connect-host=0.0.0.0"}, {"--udp-port=-1"}, {"--connect-rev-port=65536"}, {"--unknown-port=123"}, {"--write-config="}, {"extra"}} {
		if err := run(append(append([]string{}, base...), flags...), io.Discard); err == nil {
			t.Errorf("check silently accepted %v", flags)
		}
	}
	var output bytes.Buffer
	if err := run(append(base, "--connect-host=10.0.0.2", "--tls-port=443"), &output); err != nil || output.String() != "configuration valid\n" {
		t.Fatalf("valid offline check failed: %v", err)
	}
}

func TestOfflineCommandsRejectInvalidNetworkEnvironment(t *testing.T) {
	for _, key := range []string{"BIND_INBOUND", "BIND_OUTBOUND_4", "BIND_OUTBOUND_6", "OUTBOUND_FWMARK", "TUNNEL_BIND_INBOUND", "TUNNEL_BIND_OUTBOUND_4", "TUNNEL_BIND_OUTBOUND_6", "TUNNEL_FWMARK", "TUNNEL_INTERFACE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "not-a-valid-network-option")
			dir := t.TempDir()
			source, target := filepath.Join(dir, "absent.yml"), filepath.Join(dir, "ready.yml")
			base := []string{"-c", source, "-u", "https://panel.invalid", "-t", "secret"}
			if err := run(append(append([]string{}, base...), "--check"), io.Discard); err == nil {
				t.Fatal("check silently ignored invalid network environment")
			}
			if err := run(append(base, "--write-config", target), io.Discard); err == nil {
				t.Fatal("write-config silently ignored invalid network environment")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("invalid environment produced a configuration file")
			}
		})
	}
}

func TestWriteConfigCLIStaysOfflineAndPreservesRole(t *testing.T) {
	t.Setenv("UUID", "installer-uuid")
	dir := t.TempDir()
	path, target := filepath.Join(dir, "absent.yml"), filepath.Join(dir, "ready.yml")
	args := []string{"-c", path, "-u", "https://panel.invalid", "-t", "secret", "--write-config", target, "--is-outbound=true", "--connect-host=10.0.0.2", "--tls-port=8443"}
	if err := run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeclient.LoadConfig(target, "", "")
	if err != nil || cfg.Network.ConnectHost != "10.0.0.2" || cfg.Network.TlsPort != 8443 || cfg.MachineID != "installer-uuid" {
		t.Fatalf("installer CLI configuration not retained: %+v, %v", cfg.Network, err)
	}
	data, _ := os.ReadFile(target)
	if !bytes.Contains(data, []byte("is-outbound: true")) {
		t.Fatal("compatibility role missing")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "ready.yml" {
		t.Fatal("write-config created runtime files")
	}
}
