package nodeclient

import (
	"errors"
	"github.com/openroute/openroute/internal/nodeproto"
	"net"
	"strconv"
	"sync"
	"time"
)

type reverseConn struct {
	net.Conn
	peer     uint64
	done     chan struct{}
	once     sync.Once
	created  time.Time
	acquired chan struct{}
}

func (c *reverseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
func (c *reverseConn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}
func (s *tunnelServer) acceptReverse(f *forwarder, conn net.Conn, peer uint64) {
	pooled := &reverseConn{Conn: conn, peer: peer, done: make(chan struct{}), acquired: make(chan struct{}), created: time.Now()}
	f.engine.mu.Lock()
	queue := f.engine.reverse[f.rule.RuleID]
	if queue == nil {
		queue = make(chan net.Conn, 32)
		f.engine.reverse[f.rule.RuleID] = queue
	}
	f.engine.mu.Unlock()
	select {
	case queue <- pooled:
	case <-f.ctx.Done():
		return
	default:
		return
	}
	timer := time.NewTimer(40 * time.Second)
	defer timer.Stop()
	select {
	case <-pooled.acquired:
		select {
		case <-pooled.done:
		case <-f.ctx.Done():
			pooled.Close()
		}
	case <-pooled.done:
	case <-f.ctx.Done():
		pooled.Close()
	case <-timer.C:
		pooled.Close()
	}
}
func (f *forwarder) openReverse(network, source string, path []uint64) (net.Conn, func(), error) {
	f.engine.mu.Lock()
	queue := f.engine.reverse[f.rule.RuleID]
	if queue == nil {
		queue = make(chan net.Conn, 32)
		f.engine.reverse[f.rule.RuleID] = queue
	}
	f.engine.mu.Unlock()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-f.ctx.Done():
			return nil, func() {}, f.ctx.Err()
		case <-timer.C:
			return nil, func() {}, errors.New("no available reverse tunnel")
		case conn := <-queue:
			pooled, ok := conn.(*reverseConn)
			if !ok || time.Since(pooled.created) > 30*time.Second {
				conn.Close()
				continue
			}
			if !f.member(path[0], pooled.peer) {
				conn.Close()
				continue
			}
			close(pooled.acquired)
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			req := newTunnelRequest(f, network, source, path, 0)
			if err := writeMessage(conn, req); err != nil {
				conn.Close()
				continue
			}
			var reply tunnelReply
			if err := readMessage(conn, &reply); err != nil || reply.Error != "" {
				conn.Close()
				continue
			}
			_ = conn.SetDeadline(time.Time{})
			if network == "udp" {
				return &framedPacketConn{Conn: conn}, func() { conn.Close() }, nil
			}
			return conn, func() { conn.Close() }, nil
		}
	}
}
func (f *forwarder) maintainReverse() {
	group := f.cfg.DeviceGroupConfig[strconv.FormatUint(f.rule.InboundGroupID, 10)]
	for _, peer := range group.Peers {
		for range 2 {
			f.spawn(func() { f.reverseWorker(peer) })
		}
	}
}
func (f *forwarder) reverseWorker(peer nodeproto.GroupPeer) {
	for {
		select {
		case <-f.ctx.Done():
			return
		default:
		}
		f.reverseOnce(peer)
		timer := time.NewTimer(time.Second)
		select {
		case <-f.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (f *forwarder) reverseOnce(peer nodeproto.GroupPeer) {
	port := peer.RevPort
	if f.rule.ReversePort > 0 {
		port = f.rule.ReversePort
	}
	if port <= 0 {
		return
	}
	conn, err := outboundDial(f.ctx, "tcp", net.JoinHostPort(peer.Host, strconv.Itoa(port)))
	if err != nil {
		return
	}
	if !f.track(conn) {
		return
	}
	raw := conn
	defer f.untrack(raw)
	_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
	path := f.routePath()
	if len(path) != 1 {
		return
	}
	protocol, opts := f.transportConfig(path[0])
	switch protocol {
	case "tls":
		conn, err = f.securePeer(conn, peer, mapOption(opts["tls"]))
		if err != nil {
			return
		}
	case "ws", "http":
		if writeCamouflageRequest(conn, opts, protocol, peer.Host) != nil {
			return
		}
	case "direct":
	default:
		return
	}
	req := newTunnelRequest(f, "tcp", "", f.routePath(), 0)
	req.Action = "reverse"
	signRequest(req, f.rule.TunnelToken)
	if writeMessage(conn, req) != nil {
		return
	}
	if protocol == "ws" || protocol == "http" {
		var header string
		conn, header, err = readCamouflage(conn)
		if err != nil {
			return
		}
		if expected := stringOption(mapOption(opts["ws"])["response"]); expected != "" && header != expected {
			return
		}
	}
	var reply tunnelReply
	if readMessage(conn, &reply) != nil || reply.Error != "" {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(35 * time.Second))
	var open tunnelRequest
	if readMessage(conn, &open) != nil {
		return
	}
	verifier := &tunnelServer{engine: f.engine, nonces: map[string]int64{}}
	authorized, err := verifier.authenticate(open)
	if err != nil || authorized != f {
		_ = writeMessage(conn, tunnelReply{Error: "unauthorized reverse request"})
		return
	}
	f.serveTunnel(conn, open)
}
