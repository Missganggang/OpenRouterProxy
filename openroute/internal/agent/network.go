package agent

import (
	"encoding/json"
	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
	"strings"
)

func staticTCPPort(group *model.DeviceGroup) int {
	var cfg struct {
		Type    string `json:"connect_type"`
		Address string `json:"connect_address"`
		Port    int    `json:"connect_port"`
	}
	if group == nil || json.Unmarshal([]byte(group.Config), &cfg) != nil {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Type), "static") && strings.TrimSpace(cfg.Address) != "" && cfg.Port > 0 && cfg.Port <= 65535 {
		return cfg.Port
	}
	return 0
}

func applyNetworkInfo(node *model.Node, network *nodeproto.NodeNetworkConfig, updates map[string]interface{}) (bool, error) {
	if network == nil {
		return false, nil
	}
	if err := network.Normalize(); err != nil {
		return false, response.Field(response.CodeParamInvalid, "network", nil, err.Error())
	}
	if *network == node.LocalNetwork() {
		return false, nil
	}
	updates["reported_network"] = model.FromAny(network)
	return true, nil
}

func nodeConnectPorts(n *model.Node) nodeproto.NodeListeners {
	ports, local := nodePorts(n), n.LocalNetwork()
	if local.ConnectDirectPort > 0 {
		ports.DirectPort = local.ConnectDirectPort
	}
	if local.ConnectWsPort > 0 {
		ports.WsPort = local.ConnectWsPort
	}
	if local.ConnectTlsPort > 0 {
		ports.TlsPort = local.ConnectTlsPort
	}
	if local.ConnectUdpPort > 0 {
		ports.UdpPort = local.ConnectUdpPort
	}
	if local.ConnectRevPort > 0 {
		ports.RevPort = local.ConnectRevPort
	}
	return ports
}
