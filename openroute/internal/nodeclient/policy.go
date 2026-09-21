package nodeclient

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type streamInfo struct {
	protocol, sni, path, device string
	tls12, websocket            bool
}
type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}

// Inspect only when a policy requires it. Server-first protocols otherwise have
// no added latency. Bytes read for classification are replayed without change.
func inspectStream(conn net.Conn) (net.Conn, streamInfo, error) {
	var info streamInfo
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	r := bufio.NewReaderSize(conn, 131072)
	first, err := r.Peek(5)
	if err != nil {
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			info.protocol = "fet"
			return &bufferedConn{conn, r}, info, nil
		}
		return &bufferedConn{conn, r}, info, err
	}
	switch {
	case first[0] == 22 && first[1] == 3:
		info.protocol = "tls"
		hello := []byte{}
		offset := 0
		for {
			header, peekErr := r.Peek(offset + 5)
			if peekErr != nil {
				return conn, info, peekErr
			}
			header = header[offset:]
			size := int(binary.BigEndian.Uint16(header[3:5]))
			if header[0] != 22 || size == 0 || size > 32768 || offset+size+5 > 131072 {
				return conn, info, errors.New("invalid TLS ClientHello record")
			}
			record, peekErr := r.Peek(offset + size + 5)
			if peekErr != nil {
				return conn, info, peekErr
			}
			hello = append(hello, record[offset+5:]...)
			offset += size + 5
			if len(hello) >= 4 {
				length := 4 + int(hello[1])<<16 + int(hello[2])<<8 + int(hello[3])
				if length > 65536 {
					return conn, info, errors.New("TLS ClientHello too large")
				}
				if len(hello) >= length {
					hello = hello[:length]
					break
				}
			}
		}
		info.sni, info.tls12, err = parseClientHello(hello)
		if err != nil {
			return conn, info, err
		}
		info.device = helloFingerprint(hello)
	case bytes.HasPrefix(first, []byte("GET ")) || bytes.HasPrefix(first, []byte("POST ")) || bytes.HasPrefix(first, []byte("HEAD ")) || bytes.HasPrefix(first, []byte("PUT ")) || bytes.HasPrefix(first, []byte("CONNE")) || bytes.HasPrefix(first, []byte("OPTIO")) || bytes.HasPrefix(first, []byte("DELET")) || bytes.HasPrefix(first, []byte("PATCH")):
		info.protocol = "http"
		// Read headers without consuming a request body or changing its framing.
		for size := 5; size <= 65536; size++ {
			header, err := r.Peek(size)
			if err != nil {
				return conn, info, err
			}
			if bytes.HasSuffix(header, []byte("\r\n\r\n")) {
				request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
				if err != nil {
					return conn, info, err
				}
				info.path = request.URL.Path
				info.sni = request.Host
				info.websocket = strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
				if request.UserAgent() != "" {
					sum := sha256.Sum256([]byte(request.UserAgent()))
					info.device = hex.EncodeToString(sum[:])
				}
				break
			}
			if size == 65536 {
				return conn, info, errors.New("HTTP headers too large")
			}
		}
	case first[0] == 4 || first[0] == 5:
		info.protocol = "socks"
	default:
		info.protocol = "fet"
	}
	return &bufferedConn{conn, r}, info, nil
}
func parseClientHello(data []byte) (string, bool, error) {
	bad := func() (string, bool, error) { return "", false, errors.New("malformed TLS ClientHello") }
	if len(data) < 42 || data[0] != 1 {
		return bad()
	}
	pos := 4 + 2 + 32
	if pos >= len(data) {
		return bad()
	}
	pos += 1 + int(data[pos])
	if pos+2 > len(data) {
		return bad()
	}
	pos += 2 + int(binary.BigEndian.Uint16(data[pos:]))
	if pos >= len(data) {
		return bad()
	}
	pos += 1 + int(data[pos])
	if pos == len(data) {
		return "", true, nil
	}
	if pos+2 > len(data) {
		return bad()
	}
	end := pos + 2 + int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	if end > len(data) {
		return bad()
	}
	sni := ""
	tls12 := true
	for pos+4 <= end {
		typ := binary.BigEndian.Uint16(data[pos:])
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		pos += 4
		if pos+size > end {
			return bad()
		}
		ext := data[pos : pos+size]
		pos += size
		if typ == 0 && len(ext) >= 5 {
			n := int(binary.BigEndian.Uint16(ext[3:5]))
			if ext[2] == 0 && n <= len(ext)-5 {
				sni = strings.ToLower(string(ext[5 : 5+n]))
			}
		}
		if typ == 43 && len(ext) >= 3 {
			for i := 1; i+1 < len(ext); i += 2 {
				if binary.BigEndian.Uint16(ext[i:]) >= tls.VersionTLS13 {
					tls12 = false
				}
			}
		}
	}
	return sni, tls12, nil
}

// Best-effort device signature. Randoms, session IDs, SNI, key shares and GREASE
// values are deliberately excluded so reconnecting the same client is stable.
func helloFingerprint(data []byte) string {
	if len(data) < 39 {
		return ""
	}
	pos := 38
	pos += 1 + int(data[pos])
	if pos+2 > len(data) {
		return ""
	}
	size := int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	if pos+size >= len(data) {
		return ""
	}
	stable := append([]byte(nil), data[4:6]...)
	for i := pos; i+1 < pos+size; i += 2 {
		v := binary.BigEndian.Uint16(data[i:])
		if v&0x0f0f != 0x0a0a {
			stable = append(stable, data[i:i+2]...)
		}
	}
	pos += size
	pos += 1 + int(data[pos])
	if pos+2 > len(data) {
		return ""
	}
	end := pos + 2 + int(binary.BigEndian.Uint16(data[pos:]))
	pos += 2
	if end > len(data) {
		return ""
	}
	extensions := []string{}
	for pos+4 <= end {
		typ := binary.BigEndian.Uint16(data[pos:])
		n := int(binary.BigEndian.Uint16(data[pos+2:]))
		pos += 4
		if pos+n > end {
			return ""
		}
		if typ&0x0f0f != 0x0a0a {
			part := fmt.Sprint(typ)
			switch typ {
			case 10, 13, 43:
				vector := data[pos : pos+n]
				prefix := 2
				if typ == 43 {
					prefix = 1
				}
				if len(vector) >= prefix {
					for i := prefix; i+1 < len(vector); i += 2 {
						value := binary.BigEndian.Uint16(vector[i:])
						if value&0x0f0f != 0x0a0a {
							part += "/" + fmt.Sprint(value)
						}
					}
				}
			case 16:
				part += hex.EncodeToString(data[pos : pos+n])
			}
			extensions = append(extensions, part)
		}
		pos += n
	}
	sort.Strings(extensions)
	stable = append(stable, []byte(strings.Join(extensions, ","))...)
	sum := sha256.Sum256(stable)
	return hex.EncodeToString(sum[:])
}
func listOption(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, v := range x {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if x != "" {
			return []string{x}
		}
	}
	return nil
}
func numberOption(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}
func boolOption(v any) bool          { x, _ := v.(bool); return x }
func mapOption(v any) map[string]any { x, _ := v.(map[string]any); return x }
func hostMatches(host, pattern string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
	if strings.HasPrefix(pattern, "*.") {
		pattern = pattern[1:]
	}
	if strings.HasPrefix(pattern, ".") {
		return strings.HasSuffix(host, pattern) || host == pattern[1:]
	}
	return host == pattern
}
func allowedHost(host string, options map[string]any) bool {
	for _, p := range listOption(options["blocked_host"]) {
		if hostMatches(host, p) {
			return false
		}
	}
	allow := listOption(options["allowed_host"])
	if len(allow) == 0 {
		return true
	}
	for _, p := range allow {
		if hostMatches(host, p) {
			return true
		}
	}
	return false
}
func (f *forwarder) needsInspection() bool {
	c := f.group.Config
	return len(listOption(c["blocked_protocol"])) > 0 || len(listOption(c["blocked_path"])) > 0 || numberOption(c["tls_inbound_policy"]) > 0 || boolOption(c["tls_reject_empty_sni"]) || len(f.rule.Shaping) > 0 || f.rule.UserLimits.DeviceLimit > 0
}
func (f *forwarder) enforcePolicy(info streamInfo) error {
	c := f.group.Config
	for _, p := range listOption(c["blocked_protocol"]) {
		if strings.EqualFold(p, info.protocol) {
			return fmt.Errorf("protocol %s blocked", p)
		}
	}
	if info.protocol == "http" {
		path := info.path
		if decoded, err := url.PathUnescape(path); err == nil {
			path = decoded
		}
		for _, p := range listOption(c["blocked_path"]) {
			if strings.HasPrefix(path, p) {
				return errors.New("HTTP path blocked")
			}
		}
	}
	if boolOption(c["tls_reject_empty_sni"]) && info.protocol == "tls" && info.sni == "" {
		return errors.New("empty SNI rejected")
	}
	for _, shape := range f.rule.Shaping {
		switch shape {
		case 1:
			if info.protocol == "tls" && info.tls12 {
				return errors.New("TLS 1.2 rejected")
			}
		case 2:
			if info.protocol == "tls" {
				return errors.New("TLS rejected")
			}
		case 3:
			if info.protocol == "http" {
				return errors.New("HTTP/1 rejected")
			}
		case 4:
			if info.websocket {
				return errors.New("WebSocket rejected")
			}
		}
	}
	return nil
}
func (f *forwarder) terminateTLS(conn net.Conn) (net.Conn, error) {
	if numberOption(f.group.Config["tls_inbound_policy"]) != 1 {
		return conn, nil
	}
	cfg := mapOption(f.rule.Options["tls"])
	cert, err := tls.X509KeyPair([]byte(strings.Join(listOption(cfg["cert"]), "\n")), []byte(strings.Join(listOption(cfg["key"]), "\n")))
	if err != nil {
		return nil, fmt.Errorf("TLS inbound certificate: %w", err)
	}
	secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: listOption(cfg["alpn"])})
	_ = secure.SetDeadline(time.Now().Add(5 * time.Second))
	err = secure.HandshakeContext(f.ctx)
	_ = secure.SetDeadline(time.Time{})
	return secure, err
}
