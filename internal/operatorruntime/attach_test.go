package operatorruntime

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestAttachHandshakeCancellation(t *testing.T) {
	for _, readRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked-request", true: "blocked-response"}[readRequest], func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				s, e := attachConnection(ctx, client, "synthetic")
				if s != nil {
					s.Close()
				}
				result <- e
			}()
			if readRequest {
				if _, e := http.ReadRequest(bufio.NewReader(server)); e != nil {
					t.Fatal(e)
				}
			}
			cancel()
			select {
			case e := <-result:
				if e == nil {
					t.Fatal("cancelled handshake succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt handshake")
			}
			server.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if _, e := server.Read(b[:]); e != io.EOF {
				t.Fatalf("handshake connection not closed: %v", e)
			}
		})
	}
}
func TestAttachHandshakeTransfersStream(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		_, e := http.ReadRequest(bufio.NewReader(server))
		if e == nil {
			_, e = io.WriteString(server, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		}
		served <- e
	}()
	stream, e := attachConnection(ctx, client, "synthetic")
	if e != nil {
		t.Fatal(e)
	}
	defer stream.Close()
	if e = <-served; e != nil {
		t.Fatal(e)
	}
	cancel() // Startup context must no longer own the successful attachment.
	go func() { _, e := io.WriteString(server, "tail"); served <- e }()
	client.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 4)
	if _, e = io.ReadFull(stream, b); e != nil || string(b) != "tail" {
		t.Fatalf("capture lost after startup cancellation: %q %v", b, e)
	}
	if e = <-served; e != nil {
		t.Fatal(e)
	}
}
func TestAttachRejectedHandshakeCloses(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		_, e := http.ReadRequest(bufio.NewReader(server))
		if e == nil {
			_, e = io.WriteString(server, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		}
		served <- e
	}()
	stream, e := attachConnection(ctx, client, "synthetic")
	if e == nil || stream != nil {
		t.Fatal("rejected handshake succeeded")
	}
	if e = <-served; e != nil {
		t.Fatal(e)
	}
	cancel()
	server.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, e = server.Read(b[:]); e != io.EOF {
		t.Fatalf("rejected connection not closed: %v", e)
	}
}
