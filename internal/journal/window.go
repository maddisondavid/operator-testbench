package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"operator-dynamo/internal/contracts"
	"strconv"
	"sync"
)

type retainedEvent struct {
	sequence, order uint64
	data            json.RawMessage
}
type eventWindow struct {
	events      []retainedEvent
	bytes       int
	first, last uint64
	completed   bool
}

// Windows is invocation-scoped disposable projection. Readers copy references to
// immutable encoded events under the lock; encoding and socket I/O happen outside.
type Windows struct {
	mu      sync.Mutex
	windows map[string]*eventWindow
	order   []string
	bytes   int
	clock   uint64
}

func NewWindows() *Windows { return &Windows{windows: map[string]*eventWindow{}} }
func (w *Windows) Register(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.windows[id] == nil && len(w.order) < 100 {
		w.windows[id] = &eventWindow{}
		w.order = append(w.order, id)
	}
}
func (w *Windows) Complete(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s := w.windows[id]; s != nil {
		s.completed = true
	}
}
func (w *Windows) remove(s *eventWindow) {
	if len(s.events) == 0 {
		return
	}
	n := len(s.events[0].data)
	s.bytes -= n
	w.bytes -= n
	s.events[0] = retainedEvent{}
	s.events = s.events[1:]
}
func (w *Windows) append(id string, seq uint64, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.windows[id]
	if s == nil {
		return
	}
	if s.first == 0 {
		s.first = seq
	}
	s.last = seq
	if len(data) > 64<<10 {
		return
	}
	w.clock++
	copyData := append(json.RawMessage(nil), data...)
	s.events = append(s.events, retainedEvent{seq, w.clock, copyData})
	s.bytes += len(data)
	w.bytes += len(data)
	for len(s.events) > 1024 || s.bytes > 1<<20 {
		w.remove(s)
	}
	for w.bytes > 8<<20 {
		var victim *eventWindow
		for _, id := range w.order {
			v := w.windows[id]
			if v.completed && len(v.events) > 0 {
				victim = v
				break
			}
		}
		if victim != nil {
			for len(victim.events) > 0 {
				w.remove(victim)
			}
			continue
		}
		for _, v := range w.windows {
			if len(v.events) > 0 && (victim == nil || v.events[0].order < victim.events[0].order) {
				victim = v
			}
		}
		if victim == nil {
			break
		}
		w.remove(victim)
	}
}
func strptr(n uint64) *string {
	if n == 0 {
		return nil
	}
	s := strconv.FormatUint(n, 10)
	return &s
}
func stringifyNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case map[string]any:
		for k, v := range x {
			x[k] = stringifyNumbers(v)
		}
	case []any:
		for i, v := range x {
			x[i] = stringifyNumbers(v)
		}
	}
	return v
}
func (w *Windows) Page(ctx context.Context, id string, after uint64, limit int) (contracts.EventPage, error) {
	if limit < 1 || limit > 200 {
		return contracts.EventPage{}, contracts.ReadFailure(400, "invalid_limit")
	}
	p := contracts.EventPage{Events: []json.RawMessage{}, NextAfter: strconv.FormatUint(after, 10), ObservedLast: "0"}
	w.mu.Lock()
	s := w.windows[id]
	if s == nil {
		w.mu.Unlock()
		return p, contracts.ReadFailure(404, "session_not_found")
	}
	if after > s.last {
		w.mu.Unlock()
		return p, contracts.ReadFailure(400, "invalid_cursor")
	}
	p.ObservedFirst = strptr(s.first)
	p.ObservedLast = strconv.FormatUint(s.last, 10)
	if len(s.events) > 0 {
		p.RetainedFirst = strptr(s.events[0].sequence)
		p.RetainedLast = strptr(s.events[len(s.events)-1].sequence)
	}
	selected := make([]retainedEvent, 0, limit)
	for _, e := range s.events {
		if e.sequence > after {
			selected = append(selected, e)
			if len(selected) == limit {
				break
			}
		}
	}
	next := s.last
	if len(selected) == limit {
		next = selected[len(selected)-1].sequence
	}
	p.NextAfter = strconv.FormatUint(next, 10)
	p.Gap = next-after > uint64(len(selected))
	w.mu.Unlock()
	for _, e := range selected {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		dec := json.NewDecoder(bytes.NewReader(e.data))
		dec.UseNumber()
		var value any
		if dec.Decode(&value) != nil {
			return p, contracts.ReadFailure(500, "event_view_failed")
		}
		raw, err := json.Marshal(stringifyNumbers(value))
		if err != nil {
			return p, contracts.ReadFailure(500, "event_view_failed")
		}
		p.Events = append(p.Events, raw)
	}
	return p, nil
}
