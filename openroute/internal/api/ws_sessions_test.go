package api

import (
	"testing"

	"github.com/openroute/openroute/internal/nodestream"
)

func TestTerminalBrokerIsolatesNodesAndReplacedConnections(t *testing.T) {
	var broker terminalBroker
	first := broker.attach(1, false)
	other := broker.attach(2, false)
	id, output, ok := broker.open(1)
	if !ok {
		t.Fatal("cannot open connected node")
	}
	message := nodestream.Message{Type: "terminal_output", Data: nodestream.Payload{SessionID: id, Data: "private shell"}}
	if broker.receive(2, other, message) {
		t.Fatal("cross-node output accepted")
	}
	if !broker.receive(1, first, message) {
		t.Fatal("owner output rejected")
	}
	if got := <-output; got.Data.Data != "private shell" {
		t.Fatal("output changed")
	}
	replacement := broker.attach(1, false)
	if _, ok := <-output; ok {
		t.Fatal("replaced connection left terminal alive")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("replaced node not closed")
	}
	broker.detach(1, first)
	id, output, ok = broker.open(1)
	if !ok {
		t.Fatal("stale detach closed replacement")
	}
	message.Data.SessionID = id
	if broker.receive(1, first, message) {
		t.Fatal("stale lease injected output")
	}
	if !broker.receive(1, replacement, message) {
		t.Fatal("replacement cannot deliver output")
	}
	<-output
	broker.setDisabled(1, replacement, true)
	if _, ok := <-output; ok {
		t.Fatal("execution disabled but terminal retained")
	}
	if _, _, ok := broker.open(1); ok {
		t.Fatal("disabled terminal allowed")
	}
	broker.detach(1, replacement)
	broker.detach(2, other)
}

func TestTerminalBrokerSlowConsumerDoesNotBlockNode(t *testing.T) {
	var broker terminalBroker
	lease := broker.attach(1, false)
	defer broker.detach(1, lease)
	id, output, _ := broker.open(1)
	message := nodestream.Message{Type: "terminal_output", Data: nodestream.Payload{SessionID: id}}
	for i := 0; i < 64; i++ {
		if !broker.receive(1, lease, message) {
			t.Fatal("output queue ended too soon")
		}
	}
	if broker.receive(1, lease, message) {
		t.Fatal("unbounded output queue")
	}
	for range output {
	}
}
