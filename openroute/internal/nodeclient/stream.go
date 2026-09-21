package nodeclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/openroute/openroute/internal/nodeproto"
	"github.com/openroute/openroute/internal/nodestream"
)

type nodeSocket struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (s *nodeSocket) write(message nodestream.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(message)
}

func (c *Client) streamLoop(ctx context.Context) {
	backoff := c.retryMin
	for ctx.Err() == nil {
		started := time.Now()
		if err := c.streamOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Printf("node stream disconnected; polling remains active: %s", c.safe(err))
		}
		if time.Since(started) > 30*time.Second {
			backoff = c.retryMin
		}
		if waitContext(ctx, backoff) != nil {
			return
		}
		backoff *= 2
		if backoff > c.retryMax {
			backoff = c.retryMax
		}
	}
}

func (c *Client) streamOnce(ctx context.Context) error {
	endpoint := strings.Replace(c.config.BaseURL, "https://", "wss://", 1)
	endpoint = strings.Replace(endpoint, "http://", "ws://", 1) + "/api/node/stream"
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, response, err := dialer.DialContext(ctx, endpoint, http.Header{nodeproto.NodeTokenHeader: []string{c.config.Token}})
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	socket := &nodeSocket{conn: conn}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	terminals := newTerminalManager(streamCtx, c.config.DisableExecute, socket.write)
	go func() { <-streamCtx.Done(); conn.Close(); terminals.close() }()
	defer terminals.close()
	conn.SetReadLimit(128 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	if err := socket.write(nodestream.Message{Type: "node_hello", Data: nodestream.Payload{DisableExecute: c.config.DisableExecute}}); err != nil {
		return err
	}
	for {
		var message nodestream.Message
		if err := conn.ReadJSON(&message); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		switch message.Type {
		case "hello", "config_changed", "task_created", "task":
			c.notify()
		case "terminal_open", "terminal_input", "terminal_resize", "terminal_close":
			terminals.handle(message)
		}
	}
}

type terminalManager struct {
	ctx      context.Context
	disabled bool
	send     func(nodestream.Message) error
	mu       sync.Mutex
	sessions map[string]*terminalSession
	wg       sync.WaitGroup
	closed   bool
}

type terminalSession struct {
	pty   ptySession
	input chan string
	done  chan struct{}
	once  sync.Once
}

func (s *terminalSession) close() {
	s.once.Do(func() { close(s.done); s.pty.Close() })
}

func newTerminalManager(ctx context.Context, disabled bool, send func(nodestream.Message) error) *terminalManager {
	return &terminalManager{ctx: ctx, disabled: disabled, send: send, sessions: make(map[string]*terminalSession)}
}

func (m *terminalManager) reply(kind, id, data, message string) {
	if err := m.send(nodestream.Message{Type: kind, Data: nodestream.Payload{SessionID: id, Data: data, Message: message}}); err != nil {
		m.closeSession(id)
	}
}

func (m *terminalManager) handle(message nodestream.Message) {
	id := message.Data.SessionID
	if len(id) != 32 {
		return
	}
	if m.disabled {
		m.reply("terminal_error", id, "", "DISABLE_EXECUTE=1: interactive terminals are disabled")
		return
	}
	switch message.Type {
	case "terminal_open":
		m.mu.Lock()
		if m.closed || m.ctx.Err() != nil {
			m.mu.Unlock()
			return
		}
		if _, exists := m.sessions[id]; exists {
			m.mu.Unlock()
			return
		}
		if len(m.sessions) >= 16 {
			m.mu.Unlock()
			m.reply("terminal_error", id, "", "node terminal limit reached")
			return
		}
		cols, rows := message.Data.Cols, message.Data.Rows
		if cols == 0 {
			cols = 80
		}
		if rows == 0 {
			rows = 24
		}
		pty, err := startPTY(m.ctx, cols, rows)
		if err != nil {
			m.mu.Unlock()
			m.reply("terminal_error", id, "", err.Error())
			return
		}
		session := &terminalSession{pty: pty, input: make(chan string, 16), done: make(chan struct{})}
		m.sessions[id] = session
		m.wg.Add(2)
		m.mu.Unlock()
		m.reply("terminal_ready", id, "", "")
		go func() {
			defer m.wg.Done()
			for {
				select {
				case <-session.done:
					return
				case data := <-session.input:
					if _, err := pty.Write([]byte(data)); err != nil {
						m.reply("terminal_error", id, "", err.Error())
						m.closeSession(id)
						return
					}
				}
			}
		}()
		go func() {
			defer m.wg.Done()
			defer m.closeSession(id)
			buf := make([]byte, 16<<10)
			var pending []byte
			for {
				n, err := pty.Read(buf)
				if n > 0 {
					pending = append(pending, buf[:n]...)
					end := 0
					for end < len(pending) && utf8.FullRune(pending[end:]) {
						_, size := utf8.DecodeRune(pending[end:])
						end += size
					}
					if end > 0 {
						m.reply("terminal_output", id, string(pending[:end]), "")
						pending = append(pending[:0], pending[end:]...)
					}
				}
				if err != nil {
					if len(pending) > 0 {
						m.reply("terminal_output", id, string(pending), "")
					}
					m.reply("terminal_closed", id, "", "terminal process exited")
					return
				}
			}
		}()
	case "terminal_close":
		m.closeSession(id)
	case "terminal_input", "terminal_resize":
		m.mu.Lock()
		session := m.sessions[id]
		m.mu.Unlock()
		if session == nil {
			return
		}
		var err error
		if message.Type == "terminal_input" {
			if len(message.Data.Data) > 64<<10 {
				err = errors.New("terminal input too large")
			} else {
				select {
				case session.input <- message.Data.Data:
				case <-session.done:
					return
				default:
					err = errors.New("terminal input queue is full")
				}
			}
		} else {
			err = session.pty.Resize(message.Data.Cols, message.Data.Rows)
		}
		if err != nil {
			m.reply("terminal_error", id, "", err.Error())
			m.closeSession(id)
		}
	}
}

func (m *terminalManager) closeSession(id string) {
	m.mu.Lock()
	session := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if session != nil {
		session.close()
	}
}
func (m *terminalManager) close() {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for id, session := range m.sessions {
		sessions = append(sessions, session)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
	m.wg.Wait()
}
