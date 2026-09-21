package api

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"github.com/openroute/openroute/internal/nodestream"
)

type terminalLease struct {
	id       string
	disabled bool
	done     chan struct{}
}
type terminalRoute struct {
	nodeID  uint64
	leaseID string
	output  chan nodestream.Message
}
type terminalBroker struct {
	mu       sync.Mutex
	nodes    map[uint64]*terminalLease
	sessions map[string]*terminalRoute
}

func sessionID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic("random source unavailable")
	}
	return hex.EncodeToString(data[:])
}

func (b *terminalBroker) attach(nodeID uint64, disabled bool) *terminalLease {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes == nil {
		b.nodes = make(map[uint64]*terminalLease)
		b.sessions = make(map[string]*terminalRoute)
	}
	if old := b.nodes[nodeID]; old != nil {
		close(old.done)
		b.removeNodeSessionsLocked(nodeID)
	}
	lease := &terminalLease{id: sessionID(), disabled: disabled, done: make(chan struct{})}
	b.nodes[nodeID] = lease
	return lease
}
func (b *terminalBroker) detach(nodeID uint64, lease *terminalLease) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes[nodeID] != lease {
		return
	}
	delete(b.nodes, nodeID)
	close(lease.done)
	b.removeNodeSessionsLocked(nodeID)
}
func (b *terminalBroker) removeNodeSessionsLocked(nodeID uint64) {
	for id, route := range b.sessions {
		if route.nodeID == nodeID {
			close(route.output)
			delete(b.sessions, id)
		}
	}
}
func (b *terminalBroker) setDisabled(nodeID uint64, lease *terminalLease, disabled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.nodes[nodeID] != lease {
		return
	}
	lease.disabled = disabled
	if disabled {
		b.removeNodeSessionsLocked(nodeID)
	}
}
func (b *terminalBroker) open(nodeID uint64) (string, <-chan nodestream.Message, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lease := b.nodes[nodeID]
	if lease == nil || lease.disabled {
		return "", nil, false
	}
	id := sessionID()
	route := &terminalRoute{nodeID: nodeID, leaseID: lease.id, output: make(chan nodestream.Message, 64)}
	b.sessions[id] = route
	return id, route.output, true
}
func (b *terminalBroker) close(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if route := b.sessions[id]; route != nil {
		delete(b.sessions, id)
		close(route.output)
	}
}
func (b *terminalBroker) receive(nodeID uint64, lease *terminalLease, message nodestream.Message) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	route := b.sessions[message.Data.SessionID]
	if b.nodes[nodeID] != lease || route == nil || route.nodeID != nodeID || route.leaseID != lease.id {
		return false
	}
	switch message.Type {
	case "terminal_ready", "terminal_output", "terminal_error", "terminal_closed":
	default:
		return false
	}
	select {
	case route.output <- message:
		return true
	default:
		delete(b.sessions, message.Data.SessionID)
		close(route.output)
		return false
	}
}
