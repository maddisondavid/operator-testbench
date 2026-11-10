package httpserver

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// HeaderObservation is immutable evidence from the UI-enabled HTTP/1 ingress.
// Bytes includes every raw header octet, including the final empty line.
type HeaderObservation struct {
	Bytes     int
	Completed time.Time
}
type ingressKey struct{}
type observationKey struct{}

func ObservedHeaders(ctx context.Context) (HeaderObservation, bool) {
	v, ok := ctx.Value(observationKey{}).(HeaderObservation)
	return v, ok && !v.Completed.IsZero()
}

type ingressListener struct{ net.Listener }

func (l ingressListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &ingressConn{Conn: c, reader: bufio.NewReaderSize(c, 4096)}, nil
}

type ingressConn struct {
	net.Conn
	reader      *bufio.Reader
	mu          sync.Mutex
	phase       int // request line, headers, complete/body
	lineBytes   int
	lineContent bool
	headerBytes int
	completed   time.Time
	frozen      HeaderObservation
}

func (c *ingressConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Go 1.26.5 net/http's connReader background read consumes at most one byte.
	// Returning one byte prevents its parser from buffering a following body or
	// request. The underlying bounded reader amortizes socket syscalls. We never
	// interpret body framing: standard net/http owns draining and state changes.
	b, e := c.reader.ReadByte()
	if e != nil {
		return 0, e
	}
	c.mu.Lock()
	switch c.phase {
	case 0:
		c.lineBytes++
		if b != '\r' && b != '\n' {
			c.lineContent = true
		}
		if b == '\n' {
			if c.lineContent {
				c.phase = 1
			}
			c.lineBytes = 0
			c.lineContent = false
		}
	case 1:
		c.headerBytes++
		c.lineBytes++
		if b != '\r' && b != '\n' {
			c.lineContent = true
		}
		if b == '\n' {
			if !c.lineContent {
				c.phase = 2
				c.completed = time.Now()
			}
			c.lineBytes = 0
			c.lineContent = false
		}
	}
	c.mu.Unlock()
	p[0] = b
	return 1, nil
}
func (c *ingressConn) state(state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch state {
	case http.StateActive:
		c.frozen = HeaderObservation{Bytes: c.headerBytes, Completed: c.completed}
	case http.StateIdle:
		c.phase = 0
		c.lineBytes = 0
		c.lineContent = false
		c.headerBytes = 0
		c.completed = time.Time{}
		c.frozen = HeaderObservation{}
	}
}

// ObserveHeaders composes the user HTTP adapter's existing callbacks. Install
// before Serve, only for UI-enabled listeners. No shared parser limit changes.
func ObserveHeaders(s *http.Server, listener net.Listener) net.Listener {
	oldContext, oldState, handler := s.ConnContext, s.ConnState, s.Handler
	s.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if oldContext != nil {
			ctx = oldContext(ctx, c)
		}
		if observed, ok := c.(*ingressConn); ok {
			ctx = context.WithValue(ctx, ingressKey{}, observed)
		}
		return ctx
	}
	s.ConnState = func(c net.Conn, state http.ConnState) {
		if observed, ok := c.(*ingressConn); ok {
			observed.state(state)
		}
		if oldState != nil {
			oldState(c, state)
		}
	}
	s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := r.Context().Value(ingressKey{}).(*ingressConn); ok {
			c.mu.Lock()
			evidence := c.frozen
			c.mu.Unlock()
			r = r.WithContext(context.WithValue(r.Context(), observationKey{}, evidence))
		}
		handler.ServeHTTP(w, r)
	})
	return ingressListener{listener}
}
