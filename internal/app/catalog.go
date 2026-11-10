package app

import (
	"context"
	"crypto/rand"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/workbench"
	"sync"
)

type catalogEntry struct {
	descriptor contracts.SessionDescriptor
	reader     workbench.Reader
	closed     bool
	leases     map[*readLease]context.CancelFunc
}
type sessionCatalog struct {
	writeEnabled bool
	mu           sync.Mutex
	id, runID    string
	order        []string
	entries      map[string]*catalogEntry
}

func newCatalog() *sessionCatalog {
	return &sessionCatalog{id: rand.Text(), entries: map[string]*catalogEntry{}}
}
func (c *sessionCatalog) register(id, name string, index int, operator bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := "notConfigured"
	if operator {
		state = "starting"
	}
	c.entries[id] = &catalogEntry{descriptor: contracts.SessionDescriptor{ID: id, Scenario: name, Index: index, State: "starting", OperatorState: state}, leases: map[*readLease]context.CancelFunc{}}
	c.order = append(c.order, id)
}
func (c *sessionCatalog) available(id string, r workbench.Reader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e != nil && !e.closed {
		e.reader = r
		e.descriptor.APIAvailable = true
		e.descriptor.OperatorState = "starting"
	}
}
func (c *sessionCatalog) ready(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e != nil && !e.closed {
		e.descriptor.State = "ready"
		_, writer := e.reader.(workbench.CommandTarget)
		e.descriptor.Writable = c.writeEnabled && c.runID == "" && writer
		if e.reader != nil {
			e.descriptor.OperatorState = "connected"
		}
	}
}
func (c *sessionCatalog) closing(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e == nil {
		return
	}
	e.closed = true
	if writer, ok := e.reader.(workbench.CommandTarget); ok {
		writer.CloseCommands()
	}
	e.descriptor.Writable = false
	e.reader = nil
	e.descriptor.State = "stopping"
	e.descriptor.APIAvailable = false
	if e.descriptor.OperatorState != "notConfigured" {
		e.descriptor.OperatorState = "stopping"
	}
	for _, cancel := range e.leases {
		cancel()
	}
}
func (c *sessionCatalog) ended(id string, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e == nil {
		return
	}
	state := "stopped"
	if failed {
		state = "failed"
	}
	e.descriptor.State = state
	if e.descriptor.OperatorState != "notConfigured" {
		e.descriptor.OperatorState = state
	}
}
func (c *sessionCatalog) Catalog(ctx context.Context) (contracts.Invocation, error) {
	if e := ctx.Err(); e != nil {
		return contracts.Invocation{}, e
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := contracts.Invocation{ID: c.id, CurrentRunID: c.runID, Sessions: []contracts.SessionDescriptor{}}
	for _, id := range c.order {
		v.Sessions = append(v.Sessions, c.entries[id].descriptor)
	}
	return v, nil
}

type readLease struct {
	catalog *sessionCatalog
	entry   *catalogEntry
	ctx     context.Context
	cancel  context.CancelFunc
	reader  workbench.Reader
}

func (l *readLease) Context() context.Context { return l.ctx }
func (l *readLease) Reader() workbench.Reader { return l.reader }
func (l *readLease) Commit() error {
	l.catalog.mu.Lock()
	defer l.catalog.mu.Unlock()
	if l.entry.closed {
		return contracts.ReadFailure(410, "session_ended")
	}
	return l.ctx.Err()
}
func (l *readLease) Release() {
	l.catalog.mu.Lock()
	delete(l.entry.leases, l)
	l.catalog.mu.Unlock()
	l.cancel()
}
func (c *sessionCatalog) Acquire(ctx context.Context, id string) (workbench.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e == nil {
		return nil, contracts.ReadFailure(404, "session_not_found")
	}
	if e.closed {
		return nil, contracts.ReadFailure(410, "session_ended")
	}
	if e.reader == nil {
		return nil, contracts.ReadFailure(409, "session_not_ready")
	}
	child, cancel := context.WithCancel(ctx)
	l := &readLease{catalog: c, entry: e, ctx: child, cancel: cancel, reader: e.reader}
	e.leases[l] = cancel
	return l, nil
}

func (c *sessionCatalog) ExecuteCommand(ctx context.Context, id string, command contracts.ResourceCommand) (contracts.CommandResult, error) {
	c.mu.Lock()
	reject := func(code int, reason string) (contracts.CommandResult, error) {
		c.mu.Unlock()
		return contracts.CommandResult{}, contracts.ReadFailure(code, reason)
	}
	if !c.writeEnabled || c.runID != "" {
		return reject(403, "writes_disabled")
	}
	if command.SessionID != id {
		return reject(400, "invalid_session")
	}
	e := c.entries[id]
	if e == nil {
		return reject(404, "session_not_found")
	}
	if e.closed {
		return reject(410, "session_ended")
	}
	if e.descriptor.State != "ready" || !e.descriptor.Writable {
		return reject(409, "session_not_ready")
	}
	writer, ok := e.reader.(workbench.CommandTarget)
	if !ok {
		return reject(403, "writes_disabled")
	}
	child, cancel := context.WithCancel(ctx)
	lease := &readLease{catalog: c, entry: e, ctx: child, cancel: cancel, reader: e.reader}
	e.leases[lease] = cancel
	c.mu.Unlock()
	defer lease.Release()
	return writer.Command(child, command)
}
