package model

import (
	"encoding/json"
	"github.com/openroute/openroute/internal/nodeproto"
)

func (n *Node) LocalNetwork() nodeproto.NodeNetworkConfig {
	var config nodeproto.NodeNetworkConfig
	_ = json.Unmarshal([]byte(n.ReportedNetwork), &config)
	return config
}

// TunnelHost is the address other nodes should use, not a listener or the
// address distributed to end users in subscriptions.
func (n *Node) TunnelHost() string {
	if local := n.LocalNetwork(); local.ConnectHost != "" {
		return local.ConnectHost
	}
	return n.ConnectHost
}
