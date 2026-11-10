package kubeapi

import (
	"context"
	"net"
	"operator-dynamo/internal/contracts"
	"testing"
	"time"
)

func TestShutdownPendingNewConnection(t *testing.T) {
	s := New("127.0.0.1:0", []Registration{widget}, nil)
	if err := s.Start(context.Background(), contracts.Session{ID: "pending"}); err != nil {
		t.Fatal(err)
	}
	// Observe acceptance before stopping; no headers/request have been sent.
	conn, err := net.Dial("tcp", s.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(time.Second)
	for s.connections.Pending() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("connection not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("pending new connection exhausted shutdown budget: %v", err)
	}
}
