package httpdrain

import (
	"net"
	"net/http"
	"testing"
)

type trackedConn struct {
	net.Conn
	closed bool
}

func (c *trackedConn) Close() error { c.closed = true; return nil }
func TestCloseOnlyPendingAndRejectLateNew(t *testing.T) {
	var c Connections
	pending, active, late := &trackedConn{}, &trackedConn{}, &trackedConn{}
	c.State(pending, http.StateNew)
	c.State(active, http.StateNew)
	c.State(active, http.StateActive)
	if c.Pending() != 1 {
		t.Fatal(c.Pending())
	}
	c.ClosePending()
	if !pending.closed || active.closed || c.Pending() != 0 {
		t.Fatal("incorrect shutdown ownership")
	}
	c.State(late, http.StateNew)
	if !late.closed {
		t.Fatal("late accepted socket left open")
	}
	c.State(active, http.StateClosed)
	c.ClosePending()
	if active.closed {
		t.Fatal("active handler must retain normal HTTP shutdown")
	}
}

type blockingConn struct {
	net.Conn
	id      int
	started chan int
	release <-chan struct{}
	closed  bool
}

func (c *blockingConn) Close() error { c.started <- c.id; <-c.release; c.closed = true; return nil }
func TestPendingTransitionDuringAnotherClose(t *testing.T) {
	var c Connections
	started := make(chan int, 2)
	release := make(chan struct{})
	conns := []*blockingConn{{id: 0, started: started, release: release}, {id: 1, started: started, release: release}}
	for _, conn := range conns {
		c.State(conn, http.StateNew)
	}
	done := make(chan struct{})
	go func() { c.ClosePending(); close(done) }()
	first := <-started
	c.State(conns[1-first], http.StateActive)
	close(release)
	<-done
	if !conns[first].closed || conns[1-first].closed {
		t.Fatal("active transition was not respected")
	}
}
