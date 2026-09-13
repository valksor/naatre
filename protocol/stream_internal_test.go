package protocol

import (
	"errors"
	"testing"
)

func TestStreamReceiverReservesFinalSequenceForLogicalTerminal(t *testing.T) {
	receiver, err := NewStreamReceiver("s", DefaultStreamLimits())
	if err != nil {
		t.Fatal(err)
	}
	receiver.opened = true
	receiver.expected = ^uint64(0)
	frame := StreamFrame{Type: StreamData, Stream: "s", Sequence: ^uint64(0), Data: []byte(`true`)}
	if _, err := receiver.Accept(frame); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("max sequence data = %v", err)
	}
	if receiver.hasState || receiver.expected != ^uint64(0) {
		t.Fatalf("receiver mutated on exhausted sequence: state=%v expected=%d", receiver.hasState, receiver.expected)
	}
	terminal := StreamFrame{Type: StreamComplete, Stream: "s", Sequence: ^uint64(0)}
	if _, err := receiver.Accept(terminal); err != nil {
		t.Fatalf("max sequence terminal = %v", err)
	}
}
