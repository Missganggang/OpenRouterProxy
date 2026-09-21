package nodeclient

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"github.com/openroute/openroute/internal/nodeproto"
	utls "github.com/refraction-networking/utls"
	"net"
	"strings"
)

var tlsProfiles = map[string]utls.ClientHelloID{"chrome": utls.HelloChrome_Auto, "firefox": utls.HelloFirefox_Auto, "safari": utls.HelloSafari_Auto, "ios": utls.HelloIOS_Auto, "android": utls.HelloAndroid_11_OkHttp, "edge": utls.HelloEdge_Auto, "360": utls.Hello360_Auto, "qq": utls.HelloQQ_Auto}

func (f *forwarder) securePeer(conn net.Conn, peer nodeproto.GroupPeer, options map[string]any) (net.Conn, error) {
	if len(peer.TLSPin) != 64 {
		return nil, errors.New("peer TLS certificate pin missing")
	}
	verify := func(certs [][]byte, chains [][]*x509.Certificate) error {
		if len(certs) == 0 {
			return errors.New("no peer TLS certificate")
		}
		sum := sha256.Sum256(certs[0])
		if !strings.EqualFold(hex.EncodeToString(sum[:]), peer.TLSPin) {
			return errors.New("peer TLS certificate pin mismatch")
		}
		return nil
	}
	sni := stringOption(options["sni"])
	if sni == "" {
		sni = peer.Host
	}
	if boolOption(options["force_empty_sni"]) || boolOption(options["empty_sni"]) {
		sni = ""
	}
	profile := stringOption(options["chfp"])
	if profile == "" {
		secure := tls.Client(conn, &tls.Config{ServerName: sni, MinVersion: tls.VersionTLS12, NextProtos: listOption(options["alpn"]), InsecureSkipVerify: true, VerifyPeerCertificate: verify})
		err := secure.HandshakeContext(f.ctx)
		return secure, err
	}
	hello, ok := tlsProfiles[profile]
	if !ok {
		return nil, errors.New("unknown TLS client fingerprint")
	}
	secure := utls.UClient(conn, &utls.Config{ServerName: sni, MinVersion: utls.VersionTLS12, InsecureSkipVerify: true, VerifyPeerCertificate: verify}, hello)
	if err := secure.BuildHandshakeState(); err != nil {
		return nil, err
	}
	if alpn := listOption(options["alpn"]); len(alpn) > 0 {
		for _, ext := range secure.Extensions {
			if a, ok := ext.(*utls.ALPNExtension); ok {
				a.AlpnProtocols = alpn
			}
		}
	}
	err := secure.HandshakeContext(f.ctx)
	return secure, err
}
