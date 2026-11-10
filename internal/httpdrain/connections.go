// Package httpdrain bounds shutdown of connections which have sent no request.
package httpdrain

import (
	"net"
	"net/http"
	"sync"
)

type pendingConn struct {
	mu  sync.Mutex
	new bool
}

// Connections tracks only StateNew sockets. Each close decision is serialized
// with that socket's state transition, without blocking other sockets' callbacks.
type Connections struct {
	mu       sync.Mutex
	pending  map[net.Conn]*pendingConn
	stopping bool
}

func (c *Connections) State(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	if c.pending == nil {
		c.pending = make(map[net.Conn]*pendingConn)
	}
	entry := c.pending[conn]
	if state == http.StateNew {
		if c.stopping {
			c.mu.Unlock()
			_ = conn.Close()
			return
		}
		c.pending[conn] = &pendingConn{new: true}
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if entry != nil {
		entry.mu.Lock()
		entry.new = false
		entry.mu.Unlock()
		c.mu.Lock()
		if c.pending[conn] == entry {
			delete(c.pending, conn)
		}
		c.mu.Unlock()
	}
}
func (c *Connections) ClosePending() {
	c.mu.Lock()
	c.stopping = true
	pending := make(map[net.Conn]*pendingConn, len(c.pending))
	for conn, entry := range c.pending {
		pending[conn] = entry
	}
	c.mu.Unlock()
	for conn, entry := range pending {
		entry.mu.Lock()
		if entry.new {
			entry.new = false
			_ = conn.Close()
		}
		entry.mu.Unlock()
		c.mu.Lock()
		if c.pending[conn] == entry {
			delete(c.pending, conn)
		}
		c.mu.Unlock()
	}
}

// Pending provides bounded diagnostics and a deterministic acceptance barrier.
func (c *Connections) Pending() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.pending) }
