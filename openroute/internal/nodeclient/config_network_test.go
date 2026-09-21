package nodeclient

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openroute/openroute/internal/nodeproto"
	"gopkg.in/yaml.v3"
)

func TestNetworkConfigFileOverridesAndExplicitClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	input := "base-url: https://panel.example\ntoken: secret\nconnect-host: 10.0.0.2\ndirect-port: 2001\nws-port: 2002\ntls-port: 2003\nudp-port: 2004\nrev-port: 2005\nconnect-direct-port: 3001\nconnect-ws-port: 3002\nconnect-tls-port: 3003\nconnect-udp-port: 3004\nconnect-rev-port: 3005\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	want := nodeproto.NodeNetworkConfig{ConnectHost: "10.0.0.2", DirectPort: 2001, WsPort: 2002, TlsPort: 2003, UdpPort: 2004, RevPort: 2005, ConnectDirectPort: 3001, ConnectWsPort: 3002, ConnectTlsPort: 3003, ConnectUdpPort: 3004, ConnectRevPort: 3005}
	cfg, err := LoadConfig(path, "", "")
	if err != nil || cfg.Network != want {
		t.Fatalf("network YAML not loaded: %+v, %v", cfg.Network, err)
	}
	cfg, err = LoadConfigWithOverrides(path, "", "", ConfigOverrides{"connect-host": "", "ws-port": 0, "connect-tls-port": 443})
	want.ConnectHost, want.WsPort, want.ConnectTlsPort = "", 0, 443
	if err != nil || cfg.Network != want {
		t.Fatalf("explicit clear or unrelated field retention failed: %+v, %v", cfg.Network, err)
	}
	for _, fields := range []ConfigOverrides{{"connect-host": "https://10.0.0.2"}, {"connect-host": "10.0.0.2:80"}, {"connect-host": "0.0.0.0"}, {"connect-ws-port": 65536}, {"tls-port": -1}, {"unknown-port": 23}} {
		if _, err := LoadConfigWithOverrides(path, "", "", fields); err == nil {
			t.Errorf("invalid network override accepted: %v", fields)
		}
	}
}

func TestWriteConfigPreservesUnknownFieldsCommentsAndNetwork(t *testing.T) {
	t.Setenv("UUID", "preserved-machine-uuid")
	dir := t.TempDir()
	path, target := filepath.Join(dir, "config.yml"), filepath.Join(dir, "config.new.yml")
	input := "# operator comment\nbase-url: https://old.example\ntoken: old-secret\nconnect-host: &peer 10.0.0.2 # private line\nws-port: 2333\nconnect-tls-port: 8443\nis-outbound: true\nuse-ech: true\ncustom:\n  endpoint: *peer\n  nested: [a, b, 17]\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(path, target, "https://new.example/", "new-secret", ConfigOverrides{"connect-host": " Example.INTERNAL ", "connect-tls-port": 0, "is-outbound": false}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["base-url"] != "https://new.example" || saved["token"] != "new-secret" || saved["machine-id"] != "preserved-machine-uuid" || saved["is-outbound"] != false || saved["use-ech"] != true || saved["ws-port"] != 2333 || saved["connect-tls-port"] != 0 {
		t.Fatal("credential, identity, role or existing configuration lost")
	}
	custom, ok := saved["custom"].(map[string]any)
	if !ok || custom["endpoint"] != "example.internal" || !reflect.DeepEqual(custom["nested"], []any{"a", "b", 17}) {
		t.Fatal("unknown fields or YAML aliases lost")
	}
	for _, text := range []string{"operator comment", "private line", "&peer", "*peer"} {
		if !strings.Contains(string(data), text) {
			t.Errorf("lost YAML metadata: %s", text)
		}
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != input {
		t.Fatal("source file was changed while writing a distinct target")
	}
	if err := WriteConfig(path, target, "", "", ConfigOverrides{"rev-port": 70000}); err == nil {
		t.Fatal("invalid config was written")
	}
	afterFailure, _ := os.ReadFile(target)
	if string(afterFailure) != string(data) {
		t.Fatal("validation failure changed the existing target")
	}
}

func TestWriteConfigInitializesWithoutNetworkOrIdentitySideEffects(t *testing.T) {
	dir := t.TempDir()
	path, target := filepath.Join(dir, "absent.yml"), filepath.Join(dir, "ready.yml")
	if err := WriteConfig(path, target, "https://panel.invalid", "secret", ConfigOverrides{"udp-port": 12000}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(target, "", "")
	if err != nil || cfg.Network.UdpPort != 12000 {
		t.Fatalf("initialized configuration invalid: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "ready.yml" {
		t.Fatal("write-config created runtime state")
	}
}

func TestNetworkConfigurationRejectsMalformedYAML(t *testing.T) {
	for _, tail := range []string{"ws-port: nope\n", "ws-port: -1\n", "connect-host: https://peer\n", "ws-port: 2\nws-port: 3\n", "---\nws-port: 443\n"} {
		t.Run(strings.ReplaceAll(tail, "\n", ";"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte("base-url: https://panel.example\ntoken: secret\n"+tail), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path, "", ""); err == nil {
				t.Fatal("malformed network YAML silently accepted")
			}
		})
	}
}

func TestClientAlwaysReportsLocalNetwork(t *testing.T) {
	for _, network := range []nodeproto.NodeNetworkConfig{{}, {ConnectHost: "10.0.0.2", WsPort: 2002, ConnectWsPort: 3002, RevPort: 2005, ConnectRevPort: 3005}} {
		t.Run(network.ConnectHost, func(t *testing.T) {
			var registered, heartbeated atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/node/register":
					var req nodeproto.RegisterRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Network == nil || *req.Network != network {
						t.Error("registration did not include explicit local network")
					}
					registered.Store(true)
					json.NewEncoder(w).Encode(nodeproto.RegisterResponse{NodeID: 7, Config: nodeproto.ConfigResponse{Full: true, ConfigVersion: 1}})
				case "/api/node/heartbeat":
					var req nodeproto.HeartbeatRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Network == nil || *req.Network != network {
						t.Error("heartbeat did not include explicit local network")
					}
					heartbeated.Store(true)
					json.NewEncoder(w).Encode(nodeproto.HeartbeatResponse{ConfigVersion: 1})
				case "/api/node/report":
					var req nodeproto.ReportRequest
					json.NewDecoder(r.Body).Decode(&req)
					json.NewEncoder(w).Encode(nodeproto.ReportResponse{BatchID: req.BatchID, Accepted: len(req.Results)})
				case "/api/node/tasks":
					json.NewEncoder(w).Encode(nodeproto.TasksResponse{})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client, err := NewClient(Config{BaseURL: server.URL, Token: "secret", DataDir: t.TempDir(), Network: network}, "test", log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			defer client.engine.Close()
			if err := client.register(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := client.heartbeat(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !registered.Load() || !heartbeated.Load() {
				t.Fatal("network report sequence incomplete")
			}
		})
	}
}
