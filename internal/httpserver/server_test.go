package httpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"operator-dynamo/internal/contracts"
	"testing"
	"time"
)

func TestHTTPAndCleanup(t *testing.T) {
	s := New("127.0.0.1:0")
	if err := s.Start(context.Background(), contracts.Session{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		path  string
		ready bool
		code  int
		body  string
	}{{"/healthz", false, 200, "{\"status\":\"ok\"}\n"}, {"/readyz", false, 503, "{\"status\":\"not-ready\"}\n"}, {"/readyz", true, 200, "{\"status\":\"ready\"}\n"}, {"/api", true, 404, ""}, {"/apis", true, 404, ""}, {"/unknown", true, 404, ""}} {
		s.SetReady(tc.ready)
		r, err := client.Get("http://" + s.Address() + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != tc.code || (tc.body != "" && (string(b) != tc.body || r.Header.Get("Content-Type") != "application/json")) {
			t.Fatalf("%s: %d %q", tc.path, r.StatusCode, b)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("Serve goroutine leaked")
	}
	listener, err := net.Listen("tcp", s.Address())
	if err != nil {
		t.Fatal("listener leaked", err)
	}
	listener.Close()
}
func TestBindAndCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := New(listener.Addr().String())
	if err = s.Start(context.Background(), contracts.Session{}); err == nil {
		t.Fatal("occupied port accepted")
	}
	if err = s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = New("127.0.0.1:0")
	if err = s.Start(ctx, contracts.Session{}); err == nil {
		t.Fatal("cancel ignored")
	}
}
func TestUnexpectedServeFailure(t *testing.T) {
	s := New("127.0.0.1:0")
	if err := s.Start(context.Background(), contracts.Session{}); err != nil {
		t.Fatal(err)
	}
	s.listener.Close()
	select {
	case err := <-s.Failures():
		if err == nil {
			t.Fatal("missing failure")
		}
	case <-time.After(time.Second):
		t.Fatal("failure not observed")
	}
	s.Stop(context.Background())
}

func TestShutdownPendingNewConnection(t *testing.T) {
	s := New("127.0.0.1:0")
	if err := s.Start(context.Background(), contracts.Session{ID: "pending"}); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("pending connection exhausted shutdown: %v", err)
	}
}
