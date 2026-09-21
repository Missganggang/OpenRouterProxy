package nodeproto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// NodeListeners are local tunnel listeners, independent of public/NAT peer ports.
type NodeListeners struct {
	DirectPort int    `json:"direct_port"`
	WsPort     int    `json:"ws_port"`
	TlsPort    int    `json:"tls_port"`
	UdpPort    int    `json:"udp_port"`
	RevPort    int    `json:"rev_port"`
	TLSCertPEM string `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM  string `json:"tls_key_pem,omitempty"`
}

// UserLimits contains the effective user/group policy enforced at each ingress.
type UserLimits struct {
	Disabled     bool  `json:"disabled"`
	SpeedLimit   int64 `json:"speed_limit"`
	ConnLimit    int   `json:"conn_limit"`
	IPLimit      int   `json:"ip_limit"`
	DeviceLimit  int   `json:"device_limit"`
	TrafficLimit int64 `json:"traffic_limit"`
	TrafficUsed  int64 `json:"traffic_used"`
}

// ConfigHash excludes transport metadata and ephemeral health/counter fields.
// Those values change on heartbeats without a configuration change.
func ConfigHash(cfg ConfigResponse) string {
	cfg.ConfigVersion, cfg.GeneratedAt = 0, 0
	cfg.Full = true
	cfg.RemovedRuleIDs = nil
	cfg.Rules = append([]ConfigRule(nil), cfg.Rules...)
	for i := range cfg.Rules {
		cfg.Rules[i].UserLimits.TrafficUsed = 0
	}
	sort.Slice(cfg.Rules, func(i, j int) bool {
		if cfg.Rules[i].RuleID != cfg.Rules[j].RuleID {
			return cfg.Rules[i].RuleID < cfg.Rules[j].RuleID
		}
		return !cfg.Rules[i].IsOutbound && cfg.Rules[j].IsOutbound
	})
	groups := make(map[string]DeviceGroupConfig, len(cfg.DeviceGroupConfig))
	for key, group := range cfg.DeviceGroupConfig {
		group.Peers = append([]GroupPeer(nil), group.Peers...)
		for i := range group.Peers {
			group.Peers[i].Online = false
			group.Peers[i].CurrentConn = 0
		}
		groups[key] = group
	}
	cfg.DeviceGroupConfig = groups
	data, _ := json.Marshal(cfg)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func RuleConfigHash(cfg ConfigResponse, rule ConfigRule) string {
	cfg.Rules = []ConfigRule{rule}
	return ConfigHash(cfg)
}
