package nodeclient

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/openroute/openroute/internal/nodeproto"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const udpHeaderSize = 49

// A native UDP session is bound to a rule, sender, random session ID and remote
// endpoint. Authenticated sequence numbers reject duplicate/replayed packets.
type udpTunnelSession struct {
	conn    net.Conn
	mu      sync.Mutex
	last    uint64
	send    uint64
	rule    uint64
	id      [16]byte
	aead    cipher.AEAD
	remote  *net.UDPAddr
	release func()
	f       *forwarder
}
type nativePacketConn struct {
	net.Conn
	rule       uint64
	id         [16]byte
	aead       cipher.AEAD
	writeMu    sync.Mutex
	send, last uint64
}

func udpAEAD(token string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte("openroute/udp/v1/" + token))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func sealUDP(aead cipher.AEAD, rule uint64, id [16]byte, seq uint64, response bool, kind byte, data []byte) ([]byte, error) {
	if len(data) > 65400 {
		return nil, errors.New("native tunnel datagram exceeds MTU limit")
	}
	header := make([]byte, udpHeaderSize)
	copy(header, "ORU1")
	binary.BigEndian.PutUint64(header[4:12], rule)
	copy(header[12:28], id[:])
	binary.BigEndian.PutUint64(header[28:36], seq)
	if response {
		header[36] = 1
	}
	if _, err := rand.Read(header[37:49]); err != nil {
		return nil, err
	}
	body := append([]byte{kind}, data...)
	return aead.Seal(header, header[37:49], body, header[:37]), nil
}
func openUDP(aead cipher.AEAD, packet []byte) (uint64, [16]byte, uint64, bool, byte, []byte, error) {
	var id [16]byte
	if len(packet) < udpHeaderSize+17 || string(packet[:4]) != "ORU1" {
		return 0, id, 0, false, 0, nil, errors.New("invalid UDP tunnel packet")
	}
	rule := binary.BigEndian.Uint64(packet[4:12])
	copy(id[:], packet[12:28])
	seq := binary.BigEndian.Uint64(packet[28:36])
	body, err := aead.Open(nil, packet[37:49], packet[49:], packet[:37])
	if err != nil {
		return 0, id, 0, false, 0, nil, err
	}
	return rule, id, seq, packet[36] == 1, body[0], body[1:], nil
}
func (c *nativePacketConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.send++
	packet, err := sealUDP(c.aead, c.rule, c.id, c.send, false, 2, p)
	if err != nil {
		return 0, err
	}
	if _, err = c.Conn.Write(packet); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *nativePacketConn) Read(p []byte) (int, error) {
	buffer := make([]byte, 65535)
	for {
		n, err := c.Conn.Read(buffer)
		if err != nil {
			return 0, err
		}
		rule, id, seq, response, kind, body, err := openUDP(c.aead, buffer[:n])
		if err != nil || rule != c.rule || id != c.id || !response || seq <= c.last || kind != 2 {
			continue
		}
		c.last = seq
		if len(body) > len(p) {
			return 0, io.ErrShortBuffer
		}
		return copy(p, body), nil
	}
}
func (f *forwarder) dialNativeUDP(peer nodeproto.GroupPeer, source string, path []uint64, hop int) (net.Conn, error) {
	if peer.UdpPort <= 0 {
		return nil, errors.New("peer native UDP port missing")
	}
	conn, err := outboundDial(f.ctx, "udp", net.JoinHostPort(peer.Host, strconv.Itoa(peer.UdpPort)))
	if err != nil {
		return nil, err
	}
	stopCancel := context.AfterFunc(f.ctx, func() { conn.Close() })
	defer stopCancel()
	aead, err := udpAEAD(f.rule.TunnelToken)
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &nativePacketConn{Conn: conn, rule: f.rule.RuleID, aead: aead, send: 1}
	_, _ = rand.Read(c.id[:])
	req := newTunnelRequest(f, "udp", source, path, hop)
	data, _ := json.Marshal(req)
	packet, err := sealUDP(aead, c.rule, c.id, c.send, false, 1, data)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = conn.Write(packet); err != nil {
		conn.Close()
		return nil, err
	}
	buffer := make([]byte, 65535)
	n, err := conn.Read(buffer)
	if err != nil {
		conn.Close()
		return nil, err
	}
	rule, id, seq, response, kind, body, err := openUDP(aead, buffer[:n])
	if err != nil || rule != c.rule || id != c.id || !response || kind != 1 || len(body) != 0 {
		conn.Close()
		return nil, errors.New("native UDP authentication failed")
	}
	c.last = seq
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}
func (s *tunnelServer) serveNativeUDP() {
	buffer := make([]byte, 65535)
	pending := make(chan struct{}, 32)
	for {
		n, remote, err := s.packet.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if n < udpHeaderSize+17 || string(buffer[:4]) != "ORU1" {
			continue
		}
		ruleID := binary.BigEndian.Uint64(buffer[4:12])
		s.engine.mu.Lock()
		f := s.engine.rules[ruleID]
		s.engine.mu.Unlock()
		if f == nil || !f.outbound || len(f.rule.TunnelToken) < 16 {
			continue
		}
		aead, err := udpAEAD(f.rule.TunnelToken)
		if err != nil {
			continue
		}
		rule, id, seq, response, kind, data, err := openUDP(aead, buffer[:n])
		if err != nil || response || rule != ruleID {
			continue
		}
		key := strconv.FormatUint(ruleID, 10) + ":" + remote.String() + ":" + hex.EncodeToString(id[:])
		s.mu.Lock()
		session := s.sessions[key]
		count := len(s.sessions)
		s.mu.Unlock()
		if kind == 1 && session == nil && count < 4096 {
			var req tunnelRequest
			if json.Unmarshal(data, &req) != nil || req.RuleID != ruleID || req.Network != "udp" || req.Action != "open" {
				continue
			}
			authorized, err := s.authenticate(req)
			if err != nil || authorized != f {
				continue
			}
			select {
			case pending <- struct{}{}:
				if !s.spawn(func() { defer func() { <-pending }(); s.startUDPSession(f, remote, key, id, seq, aead, req) }) {
					<-pending
				}
			default:
			}
			continue
		}
		if kind != 2 || session == nil || session.rule != ruleID || session.f != f {
			continue
		}
		session.mu.Lock()
		if seq <= session.last {
			session.mu.Unlock()
			continue
		}
		session.last = seq
		session.mu.Unlock()
		_ = session.conn.SetDeadline(time.Now().Add(time.Minute))
		writer := accountWriter{f: f, writer: session.conn, counter: f.outTraffic, upload: true, outbound: true, udp: true}
		if _, err = writer.Write(data); err != nil {
			session.conn.Close()
		}
	}
}
func (s *tunnelServer) startUDPSession(f *forwarder, remote *net.UDPAddr, key string, id [16]byte, seq uint64, aead cipher.AEAD, req tunnelRequest) {
	conn, release, err := f.openAt("udp", req.Source, req.Path, req.Hop+1)
	if err != nil {
		return
	}
	defer release()
	if !f.track(conn) {
		return
	}
	defer f.untrack(conn)
	session := &udpTunnelSession{conn: conn, last: seq, rule: f.rule.RuleID, id: id, aead: aead, remote: remote, f: f}
	s.mu.Lock()
	if s.closed || len(s.sessions) >= 4096 {
		s.mu.Unlock()
		return
	}
	s.sessions[key] = session
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.sessions, key); s.mu.Unlock() }()
	f.outTraffic.connections.Add(1)
	defer f.outTraffic.connections.Add(-1)
	if err = session.respond(s.packet, 1, nil); err != nil {
		return
	}
	buffer := make([]byte, 65535)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		n, err := conn.Read(buffer)
		if err != nil {
			return
		}
		if session.respond(s.packet, 2, buffer[:n]) != nil {
			return
		}
		f.recordTraffic(f.outTraffic, false, n)
	}
}
func (s *udpTunnelSession) respond(listener *net.UDPConn, kind byte, p []byte) error {
	s.mu.Lock()
	s.send++
	seq := s.send
	s.mu.Unlock()
	packet, err := sealUDP(s.aead, s.rule, s.id, seq, true, kind, p)
	if err != nil {
		return err
	}
	_, err = listener.WriteToUDP(packet, s.remote)
	return err
}
