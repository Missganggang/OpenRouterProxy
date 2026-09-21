package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
)

func TestReportedPrivateNetworkRegistrationAndClear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a, node := newConfigSnapshotFixture(t)
	if err := a.DB.Model(node).Updates(map[string]interface{}{"connect_host": "203.0.113.9", "direct_port": 31080}).Error; err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(a)
	router := gin.New()
	router.POST("/register", registry.RegisterHandler)
	router.POST("/heartbeat", registry.HeartbeatHandler)
	call := func(path string, input interface{}) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Node-Token", node.Token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	local := nodeproto.NodeNetworkConfig{ConnectHost: "10.88.0.2", DirectPort: 29080, UdpPort: 29083, ConnectDirectPort: 40080, ConnectUdpPort: 40083, ConnectRevPort: 40084}
	version := a.ConfigVersion()
	rec := call("/register", RegisterRequest{Token: node.Token, Network: &local})
	if rec.Code != 200 {
		t.Fatalf("registration status %d", rec.Code)
	}
	if a.ConfigVersion() != version+1 {
		t.Fatal("network change did not publish configuration")
	}
	var registered RegisterResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Config.Listeners.DirectPort != 29080 || registered.Config.Listeners.UdpPort != 29083 {
		t.Fatal("client local listeners not applied")
	}
	peer := registered.Config.DeviceGroupConfig["2"].Peers[0]
	if peer.Host != "10.88.0.2" || peer.DirectPort != 40080 || peer.UdpPort != 40083 || peer.RevPort != 40084 {
		t.Fatalf("incorrect private/NAT endpoint: %+v", peer)
	}
	var stored model.Node
	a.DB.First(&stored, node.ID)
	if stored.ConnectHost != "203.0.113.9" || stored.DirectPort != 31080 || stored.LocalNetwork() != local {
		t.Fatal("client overwrote administrator settings")
	}
	for _, network := range []*nodeproto.NodeNetworkConfig{&local, nil} {
		rec = call("/heartbeat", HeartbeatRequest{NodeID: node.ID, Network: network})
		if rec.Code != 200 || a.ConfigVersion() != version+1 {
			t.Fatal("unchanged or legacy heartbeat reset network or changed version")
		}
	}
	bad := local
	bad.ConnectHost = "https://10.88.0.2:28080"
	if rec = call("/heartbeat", HeartbeatRequest{NodeID: node.ID, Network: &bad}); rec.Code != 400 {
		t.Fatal("invalid reported endpoint accepted")
	}
	if a.ConfigVersion() != version+1 {
		t.Fatal("invalid report changed configuration")
	}
	if rec = call("/heartbeat", HeartbeatRequest{NodeID: node.ID, Network: &nodeproto.NodeNetworkConfig{}}); rec.Code != 200 {
		t.Fatal("failed to clear local overrides")
	}
	if a.ConfigVersion() != version+2 {
		t.Fatal("clearing overrides did not publish")
	}
	config, err := NewConfigBuilder(a).BuildFull(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	peer = config.DeviceGroupConfig["2"].Peers[0]
	if peer.Host != "203.0.113.9" || peer.DirectPort != 31080 || peer.UdpPort != 28083 || config.Listeners.DirectPort != 31080 {
		t.Fatal("clearing local overrides failed to restore panel/default settings")
	}
}

func TestStaticTCPOverridePreservesNativeUDPAndReversePorts(t *testing.T) {
	for _, connectType := range []string{"static", " StAtIc "} {
		t.Run(connectType, func(t *testing.T) {
			a, node := newConfigSnapshotFixture(t)
			node.ConnectHost = "10.88.0.2"
			node.ReportedNetwork = model.FromAny(nodeproto.NodeNetworkConfig{ConnectHost: "10.88.0.3", UdpPort: 29083, ConnectUdpPort: 41083, ConnectRevPort: 41084})
			group := &model.DeviceGroup{ID: 2, NodeIDs: model.FromAny([]uint64{node.ID}), Config: model.FromAny(map[string]interface{}{"connect_type": connectType, "connect_address": "10.88.0.4", "connect_port": 41080})}
			peers, err := NewConfigBuilder(a).resolvePeers(group, map[uint64]*model.Node{node.ID: node})
			if err != nil {
				t.Fatal(err)
			}
			peer := peers[0]
			if peer.Host != "10.88.0.4" || peer.DirectPort != 41080 || peer.TlsPort != 41080 || peer.WsPort != 41080 || peer.UdpPort != 41083 || peer.RevPort != 41084 {
				t.Fatalf("static TCP port missing or leaked into other transports: %+v", peer)
			}
			if nodePorts(node).UdpPort != 29083 {
				t.Fatal("NAT port changed local listener")
			}
		})
	}
}
