package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/nodeproto"
)

func (b *ConfigBuilder) deriveSecret(purpose string, id uint64) []byte {
	mac := hmac.New(sha256.New, []byte(b.app.Config.SecretKey))
	fmt.Fprintf(mac, "openroute-v1/%s/%d", purpose, id)
	return mac.Sum(nil)
}

// Persisted P-256 certificates support browser TLS fingerprints and retain their
// pins across restarts. Only the owner node receives its private key.
func (b *ConfigBuilder) nodeCertificate(id uint64) (cert, key, pin string, err error) {
	var node model.Node
	if err = b.app.DB.First(&node, id).Error; err != nil {
		return
	}
	if node.TunnelTLSCert == "" || node.TunnelTLSKey == "" {
		private, generateErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if generateErr != nil {
			err = generateErr
			return
		}
		template := &x509.Certificate{SerialNumber: new(big.Int).SetUint64(id + 1), Subject: pkix.Name{CommonName: fmt.Sprintf("openroute-node-%d", id)},
			NotBefore: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2120, 1, 1, 0, 0, 0, 0, time.UTC),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, generateErr := x509.CreateCertificate(rand.Reader, template, template, private.Public(), private)
		if generateErr != nil {
			err = generateErr
			return
		}
		privateDER, generateErr := x509.MarshalPKCS8PrivateKey(private)
		if generateErr != nil {
			err = generateErr
			return
		}
		certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
		// Concurrent builders must select the same winning certificate and key pair.
		if err = b.app.DB.Model(&model.Node{}).Where("id = ? AND (tunnel_tls_cert IS NULL OR tunnel_tls_cert = '' OR tunnel_tls_key IS NULL OR tunnel_tls_key = '')", id).
			Updates(map[string]interface{}{"tunnel_tls_cert": certificate, "tunnel_tls_key": privatePEM}).Error; err != nil {
			return
		}
		if err = b.app.DB.First(&node, id).Error; err != nil {
			return
		}
	}
	cert, key = node.TunnelTLSCert, node.TunnelTLSKey
	pair, parseErr := tls.X509KeyPair([]byte(cert), []byte(key))
	if parseErr != nil {
		err = fmt.Errorf("节点 %d 隧道证书无效: %w", id, parseErr)
		return
	}
	sum := sha256.Sum256(pair.Certificate[0])
	pin = hex.EncodeToString(sum[:])
	return
}

func nodePorts(n *model.Node) nodeproto.NodeListeners {
	ports := nodeproto.NodeListeners{DirectPort: n.DirectPort, WsPort: n.WsPort, TlsPort: n.TlsPort, UdpPort: n.UdpPort, RevPort: n.RevPort}
	if ports.DirectPort == 0 {
		ports.DirectPort = 28080
	}
	if ports.WsPort == 0 {
		ports.WsPort = 28081
	}
	if ports.TlsPort == 0 {
		ports.TlsPort = 28082
	}
	if ports.UdpPort == 0 {
		ports.UdpPort = 28083
	}
	if ports.RevPort == 0 {
		ports.RevPort = 28084
	}
	return ports
}

func (b *ConfigBuilder) applyUserPolicies(ctx context.Context, rules []ConfigRule) error {
	var users []model.User
	var userGroups []model.UserGroup
	if err := b.app.DB.WithContext(ctx).Find(&users).Error; err != nil {
		return err
	}
	if err := b.app.DB.WithContext(ctx).Find(&userGroups).Error; err != nil {
		return err
	}
	groups := map[uint64]model.UserGroup{}
	for _, group := range userGroups {
		groups[group.ID] = group
	}
	policies := map[uint64]nodeproto.UserLimits{}
	for _, user := range users {
		group := groups[user.GroupID]
		policy := nodeproto.UserLimits{Disabled: user.Status != model.StatusEnabled || (user.ExpireAt != nil && !user.ExpireAt.After(time.Now())),
			SpeedLimit: user.SpeedLimit, ConnLimit: user.ConnLimit, IPLimit: user.IPLimit, DeviceLimit: user.DeviceLimit,
			TrafficLimit: user.TrafficLimit, TrafficUsed: user.TrafficUsed}
		if policy.SpeedLimit == 0 {
			policy.SpeedLimit = group.SpeedLimit
		}
		if policy.ConnLimit == 0 {
			policy.ConnLimit = group.ConnLimit
		}
		if policy.IPLimit == 0 {
			policy.IPLimit = group.IPLimit
		}
		if policy.TrafficLimit == 0 {
			policy.TrafficLimit = group.TrafficLimit
		}
		policies[user.ID] = policy
	}
	for i := range rules {
		if rules[i].UserID == 0 {
			continue
		}
		policy, ok := policies[rules[i].UserID]
		if !ok {
			policy.Disabled = true
		}
		rules[i].UserLimits = policy
	}
	return nil
}
