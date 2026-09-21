package nodeclient

import (
	"errors"
	"github.com/hashicorp/yamux"
	"github.com/openroute/openroute/internal/nodeproto"
	"io"
	"net"
	"time"
)

func muxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.ConnectionWriteTimeout = 10 * time.Second
	cfg.StreamOpenTimeout = 10 * time.Second
	cfg.StreamCloseTimeout = 10 * time.Second
	return cfg
}

// yamux Stream.Close sends FIN while allowing the peer to finish its response.
type muxConn struct{ *yamux.Stream }

func (c *muxConn) CloseWrite() error { return c.Stream.Close() }
func (f *forwarder) dialMux(peer nodeproto.GroupPeer, group uint64, network, source string, path []uint64, hop int) (net.Conn, error) {
	f.muxMu.Lock()
	if f.muxSessions == nil {
		f.muxSessions = map[uint64]*yamux.Session{}
	}
	session := f.muxSessions[peer.NodeID]
	if session == nil || session.IsClosed() {
		conn, err := f.dialPeerBase(peer, group, "tcp", source, path, hop, false, true)
		if err != nil {
			f.muxMu.Unlock()
			return nil, err
		}
		if !f.track(conn) {
			f.muxMu.Unlock()
			return nil, errors.New("rule closed")
		}
		session, err = yamux.Client(conn, muxConfig())
		if err != nil {
			f.untrack(conn)
			f.muxMu.Unlock()
			return nil, err
		}
		f.muxSessions[peer.NodeID] = session
		f.spawn(func() { <-session.CloseChan(); f.untrack(conn) })
	}
	f.muxMu.Unlock()
	stream, err := session.OpenStream()
	if err != nil {
		return nil, err
	}
	conn := &muxConn{stream}
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	req := newTunnelRequest(f, network, source, path, hop)
	if err = writeMessage(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	var reply tunnelReply
	if err = readMessage(conn, &reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply.Error != "" {
		conn.Close()
		return nil, errors.New(reply.Error)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
func (s *tunnelServer) serveMux(f *forwarder, conn net.Conn) {
	if writeMessage(conn, tunnelReply{}) != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if !f.track(conn) {
		return
	}
	defer f.untrack(conn)
	session, err := yamux.Server(conn, muxConfig())
	if err != nil {
		return
	}
	defer session.Close()
	active := make(chan struct{}, 512)
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		select {
		case active <- struct{}{}:
		default:
			stream.Close()
			continue
		}
		if !s.spawn(func() {
			defer func() { <-active }()
			c := &muxConn{stream}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(8 * time.Second))
			var req tunnelRequest
			if readMessage(c, &req) != nil || req.Action != "open" {
				return
			}
			authorized, err := s.authenticate(req)
			if err != nil || authorized != f {
				_ = writeMessage(c, tunnelReply{Error: "unauthorized mux stream"})
				return
			}
			f.serveTunnel(c, req)
		}) {
			stream.Close()
			<-active
			return
		}
	}
}
