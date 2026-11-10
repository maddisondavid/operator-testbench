package kubeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strconv"
	"sync"
	"time"
)

func (s *Server) watch(w http.ResponseWriter, r *http.Request, reg Registration, ns, corr string, p *contracts.LifecyclePayload) {
	text := r.URL.Query().Get("resourceVersion")
	initial := text == "" || text == "0"
	rv, err := strconv.ParseUint(text, 10, 64)
	if !initial && (err != nil || rv == 0) {
		p.Status = 400
		status(w, 400, "UnsupportedResourceVersion")
		return
	}
	timeout := time.Hour
	if n, e := strconv.Atoi(r.URL.Query().Get("timeoutSeconds")); e == nil {
		timeout = time.Duration(n) * time.Second
	}
	// One deadline includes capture, header flush, initial delivery and live events.
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	s.mu.Lock()
	if rv > s.version {
		s.mu.Unlock()
		p.Status = 400
		status(w, 400, "UnsupportedResourceVersion")
		return
	}
	if len(s.subscriptions) >= s.Limits.Watches {
		s.mu.Unlock()
		p.Status = 503
		status(w, 503, "SessionCapacityExceeded")
		s.fail(errors.New("API watch capacity exhausted"))
		return
	}
	sub := &subscription{reg: reg.Key(), namespace: ns, ch: make(chan event, s.Limits.History), ctx: ctx}
	var snapshot []json.RawMessage
	if initial {
		rv = s.version
		snapshot, _ = s.rawSnapshotLocked(reg, ns, "")
		valid := len(snapshot) <= s.Limits.Resources
		for _, raw := range snapshot {
			if int64(len(raw)) > s.Limits.BodyBytes {
				valid = false
				break
			}
		}
		if !valid {
			s.mu.Unlock()
			p.Status = 503
			status(w, 503, "SessionCapacityExceeded")
			s.fail(errors.New("API watch capacity exhausted"))
			return
		}

	} else {
		for _, e := range s.history {
			if e.version > rv && e.reg == reg.Key() && (ns == "" || ns == e.namespace) {
				sub.ch <- e
			}
		}
	}
	s.subscriptions[sub] = true
	listed := s.listed[reg.Key()+"/"+ns+"/"+strconv.FormatUint(rv, 10)]
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscriptions, sub); s.mu.Unlock(); s.record("watch.closed", corr, *p) }()
	p.Status = 200
	p.Version = strconv.FormatUint(rv, 10)
	if !s.record("watch.open", corr, *p) {
		p.Status = 503
		status(w, 503, "CaptureFailed")
		return
	}
	control := http.NewResponseController(w)
	// Only active writes are interrupted: idle expiry can finish HTTP framing
	// normally, while cancellation never abandons a blocked writer.
	var writeMu sync.Mutex
	writing := false
	writeFailed := false
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		writeMu.Lock()
		defer writeMu.Unlock()
		if writing {
			_ = control.SetWriteDeadline(time.Now())
		}
	})
	defer func() {
		if !stop() {
			<-callbackDone
		}
		// net/http emits a terminal chunk after the handler returns. Keep that
		// final framing bounded too, without turning idle expiry into a bad EOF.
		if !writeFailed {
			_ = control.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		}
	}()
	write := func(fn func() error) error {
		writeMu.Lock()
		if err := ctx.Err(); err != nil {
			writeMu.Unlock()
			return err
		}
		writing = true
		d := time.Now().Add(2 * time.Second)
		if deadline.Before(d) {
			d = deadline
		}
		_ = control.SetWriteDeadline(d)
		writeMu.Unlock()
		err := fn()
		if err != nil {
			writeFailed = true
		}
		writeMu.Lock()
		writing = false
		if err == nil {
			_ = control.SetWriteDeadline(time.Time{})
		}
		writeMu.Unlock()
		return err
	}
	if err := write(func() error {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		return control.Flush()
	}); err != nil {
		return
	}
	send := func(kind string, raw json.RawMessage) bool {
		err := write(func() error {
			if err := json.NewEncoder(w).Encode(map[string]any{"type": kind, "object": raw}); err != nil {
				return err
			}
			return control.Flush()
		})
		if err != nil {
			if ctx.Err() == nil {
				s.fail(errors.New("watch delivery failed"))
			}
			return false
		}
		return ctx.Err() == nil
	}
	for _, raw := range snapshot {
		if !send("ADDED", raw) {
			return
		}
	}
	s.mu.Lock()
	sub.ready = ctx.Err() == nil && ((s.InitialWatchReadiness && initial) || (!s.InitialWatchReadiness && !initial && listed))
	all := len(s.RequiredWatches) > 0
	for _, required := range s.RequiredWatches {
		active := false
		for candidate := range s.subscriptions {
			if candidate.reg == required && candidate.ready && candidate.ctx.Err() == nil && ((!s.RequireAllNamespaces && !s.InitialWatchReadiness) || candidate.namespace == "") {
				active = true
				break
			}
		}
		if !active {
			all = false
			break
		}
	}
	if all {
		s.connectOnce.Do(func() { close(s.Connected) })
	}
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-sub.ch:
			if !send(e.kind, e.object) {
				return
			}
		}
	}
}
