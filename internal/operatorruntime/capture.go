package operatorruntime

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Attacher interface {
	Attach(context.Context, string) (io.ReadCloser, error)
}
type attachedStream struct {
	net.Conn
	reader *bufio.Reader
	stop   chan struct{}
	once   sync.Once
}

func (a *attachedStream) Read(p []byte) (int, error) { return a.reader.Read(p) }
func (a *attachedStream) Close() error               { a.once.Do(func() { close(a.stop) }); return a.Conn.Close() }
func (d *Docker) Attach(ctx context.Context, id string) (io.ReadCloser, error) {
	conn, e := (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	if e != nil {
		return nil, errors.New("operator attach unavailable")
	}
	return attachConnection(ctx, conn, id)
}

// Cancellation owns the connection only until the upgrade handshake completes.
// Join a running callback before handing the live stream to capture.
func attachConnection(ctx context.Context, conn net.Conn, id string) (io.ReadCloser, error) {
	cancelled := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { conn.Close(); close(cancelled) })
	var handshakeOnce sync.Once
	stopHandshake := func() {
		handshakeOnce.Do(func() {
			if !stopCancel() {
				<-cancelled
			}
		})
	}
	defer stopHandshake()
	deadline := time.Now().Add(5 * time.Second)
	if v, ok := ctx.Deadline(); ok && v.Before(deadline) {
		deadline = v
	}
	if e := conn.SetDeadline(deadline); e != nil {
		conn.Close()
		return nil, errors.New("operator attach deadline failed")
	}
	_, e := fmt.Fprintf(conn, "POST /v1.47/containers/%s/attach?stream=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n", id)
	if e != nil {
		conn.Close()
		return nil, errors.New("operator attach request failed")
	}
	reader := bufio.NewReader(conn)
	res, e := http.ReadResponse(reader, &http.Request{Method: "POST"})
	if e != nil || res.StatusCode != 101 && res.StatusCode != 200 {
		conn.Close()
		return nil, errors.New("operator attach rejected")
	}
	stopHandshake()
	if ctx.Err() != nil {
		conn.Close()
		return nil, errors.New("operator attach cancelled")
	}
	if e := conn.SetDeadline(time.Time{}); e != nil {
		conn.Close()
		return nil, errors.New("operator attach deadline failed")
	}
	a := &attachedStream{Conn: conn, reader: reader, stop: make(chan struct{})}
	return a, nil
}

type outputRecord struct {
	Version   int       `json:"version"`
	Sequence  uint64    `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Text      string    `json:"text"`
}
type capture struct {
	source    io.ReadCloser
	done      chan struct{}
	cancel    context.CancelFunc
	mu        sync.Mutex
	err       error
	truncated bool
	records   uint64
	bytes     int
	queued    int
	patterns  [][]byte
}
type markedByte struct {
	v        byte
	suppress bool
}
type streamRedactor struct {
	cancelled   func() bool
	patterns    [][]byte
	pending     []markedByte
	max         int
	raw         []byte
	rawDiscard  bool
	line        []byte
	lineDiscard bool
	emit        func([]byte)
	truncate    func()
}

func newRedactor(patterns [][]byte, emit func([]byte), truncate func()) *streamRedactor {
	r := &streamRedactor{patterns: patterns, emit: emit, truncate: truncate}
	for _, p := range patterns {
		if len(p) > r.max {
			r.max = len(p)
		}
	}
	if r.max == 0 {
		r.max = 1
	}
	return r
}
func (r *streamRedactor) output(data []byte, suppressed bool) {
	for _, b := range data {
		if suppressed {
			r.lineDiscard = true
		}
		if b == '\n' {
			if !r.lineDiscard {
				r.emit(r.line)
			}
			r.line = nil
			r.lineDiscard = false
			continue
		}
		if !r.lineDiscard {
			r.line = append(r.line, b)
			if len(r.line) > 64<<10 {
				r.line = nil
				r.lineDiscard = true
				r.truncate()
			}
		}
	}
}
func (r *streamRedactor) drain(final bool) {
	for len(r.pending) > 0 && (final || len(r.pending) >= r.max) {
		match := 0
		for _, p := range r.patterns {
			if len(p) > len(r.pending) || len(p) <= match {
				continue
			}
			ok := true
			for i, b := range p {
				if r.pending[i].v != b {
					ok = false
					break
				}
			}
			if ok {
				match = len(p)
			}
		}
		count := 1
		data := []byte{r.pending[0].v}
		if match > 0 {
			count = match
			data = []byte("[REDACTED]")
		}
		suppress := false
		for _, b := range r.pending[:count] {
			suppress = suppress || b.suppress
		}
		r.output(data, suppress)
		r.pending = r.pending[count:]
	}
}
func (r *streamRedactor) feed(data []byte, suppress bool) {
	for i, b := range data {
		if i%16 == 0 && r.cancelled != nil && r.cancelled() {
			return
		}
		r.pending = append(r.pending, markedByte{b, suppress})
		r.drain(false)
	}
}
func (r *streamRedactor) Write(data []byte) {
	for i, b := range data {
		if i%16 == 0 && r.cancelled != nil && r.cancelled() {
			return
		}
		if r.rawDiscard {
			r.feed([]byte{b}, true)
			if b == '\n' {
				r.rawDiscard = false
			}
			continue
		}
		r.raw = append(r.raw, b)
		if b == '\n' {
			r.feed(r.raw, false)
			r.raw = nil
		} else if len(r.raw) > 64<<10 {
			r.truncate()
			r.feed(r.raw, true)
			r.raw = nil
			r.rawDiscard = true
		}
	}
}
func (r *streamRedactor) End() {
	r.feed(r.raw, false)
	r.raw = nil
	r.drain(true)
	if len(r.line) > 0 && !r.lineDiscard {
		r.emit(r.line)
	}
	r.line = nil
}
func (c *capture) run(ctx context.Context, write func(context.Context, []byte) error) {
	defer close(c.done)
	defer c.source.Close()
	retaining := true
	queue := make(chan []byte, 8192)
	writerDone := make(chan struct{})
	var writeErr error
	go func() {
		defer close(writerDone)
		for data := range queue {
			if writeErr == nil {
				if e := write(ctx, data); e != nil {
					writeErr = errors.New("capture artifact write failed")
					c.source.Close()
				}
			}
			c.mu.Lock()
			c.queued -= len(data)
			c.mu.Unlock()
		}
	}()

	truncate := func() { c.mu.Lock(); c.truncated = true; c.mu.Unlock() }
	emit := func(stream string) func([]byte) {
		return func(line []byte) {
			if !retaining {
				return
			}
			c.records++
			data, _ := json.Marshal(outputRecord{1, c.records, time.Now().UTC(), stream, strings.ToValidUTF8(string(line), "\ufffd")})
			data = append(data, '\n')
			c.mu.Lock()
			over := c.bytes+len(data) > 4<<20 || c.queued+len(data) > 1<<20
			if !over {
				c.queued += len(data)
				c.bytes += len(data)
			}
			c.mu.Unlock()
			if over {
				retaining = false
				c.records--
				truncate()
				return
			}
			select {
			case queue <- data:
			default:
				c.mu.Lock()
				c.queued -= len(data)
				c.bytes -= len(data)
				c.mu.Unlock()
				retaining = false
				c.records--
				truncate()
			}
		}
	}

	stdout := newRedactor(c.patterns, emit("stdout"), truncate)
	stderr := newRedactor(c.patterns, emit("stderr"), truncate)
	stdout.cancelled = func() bool { return ctx.Err() != nil }
	stderr.cancelled = stdout.cancelled
	var runErr error
	for {
		var header [8]byte
		n, e := io.ReadFull(c.source, header[:])
		if e == io.EOF && n == 0 {
			stdout.End()
			stderr.End()
			break
		}
		if e != nil {
			runErr = errors.New("capture frame header incomplete")
			break
		}
		size := binary.BigEndian.Uint32(header[4:])
		if header[0] != 1 && header[0] != 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 || size > 1<<20 {
			runErr = errors.New("capture frame invalid or oversized")
			break
		}
		if size == 0 {
			continue
		}
		data := make([]byte, size)
		if _, e = io.ReadFull(c.source, data); e != nil {
			runErr = errors.New("capture frame payload incomplete")
			break
		}
		if retaining {
			if header[0] == 1 {
				stdout.Write(data)
			} else {
				stderr.Write(data)
			}
		}
		if ctx.Err() != nil {
			runErr = errors.New("capture cancelled before EOF")
			break
		}
	}
	close(queue)
	<-writerDone
	if writeErr != nil {
		runErr = writeErr
	}

	c.mu.Lock()
	c.err = runErr
	c.mu.Unlock()
}
func (r *Runtime) startCapture(ctx context.Context) error {
	if r.Config.Logs.Mode != "capture" {
		return nil
	}
	a, ok := r.Engine.(Attacher)
	if !ok {
		return errors.New("Docker attach unavailable")
	}
	if e := r.Inputs.WriteOutput(ctx, "operator-output.jsonl", nil); e != nil {
		return e
	}
	life, cancel := context.WithCancel(context.Background())
	source, e := a.Attach(ctx, r.id)
	if e != nil {
		cancel()
		return e
	}
	c := &capture{source: source, done: make(chan struct{}), cancel: cancel}
	for _, p := range r.Config.patterns() {
		c.patterns = append(c.patterns, []byte(p))
	}
	sort.Slice(c.patterns, func(i, j int) bool { return len(c.patterns[i]) > len(c.patterns[j]) })
	r.capture = c
	go c.run(life, func(ctx context.Context, data []byte) error {
		return r.Inputs.AppendOutput(ctx, "operator-output.jsonl", data)
	})
	if e = r.emit("capture.started"); e != nil {
		source.Close()
		cancel()
		return e
	}
	return nil
}
