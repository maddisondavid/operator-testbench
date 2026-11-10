package kubeapi

import (
	"fmt"
	"math"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strings"
	"sync"
)

type rejectionKey struct {
	method, resource, reason string
	watch                    bool
	status                   int
}
type rejectionCount struct {
	key   rejectionKey
	count uint64
}
type rejectionSummary struct {
	mu             sync.Mutex
	entries        [8]rejectionCount
	used           int
	total, omitted uint64
	saturated      bool
	terminal       string
}

func increment(v *uint64, saturated *bool) {
	if *v == math.MaxUint64 {
		*saturated = true
	} else {
		*v++
	}
}
func (d *rejectionSummary) add(k rejectionKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	increment(&d.total, &d.saturated)
	for i := 0; i < d.used; i++ {
		if d.entries[i].key == k {
			increment(&d.entries[i].count, &d.saturated)
			return
		}
	}
	if d.used < len(d.entries) {
		d.entries[d.used] = rejectionCount{k, 1}
		d.used++
		return
	}
	increment(&d.omitted, &d.saturated)
}

type rejectionWriter struct {
	http.ResponseWriter
	server  *Server
	request *http.Request
}

func (w *rejectionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *rejectionWriter) Rejected(code int, reason string) {
	method := diagnosticMethod(w.request.Method)
	resource := "unregistered-route"
	if reg, _, _, ok := w.server.route(w.request.URL.Path); ok {
		resource = reg.Key()
	} else if reg, _, _, ok := w.server.route(strings.TrimSuffix(w.request.URL.Path, "/status")); ok {
		resource = reg.Key() + "/subresource"
	}
	w.server.diagnostics.add(rejectionKey{method: method, resource: resource, reason: reason, watch: w.request.URL.Query().Get("watch") == "true", status: code})
}
func diagnosticMethod(method string) string {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return method
	default:
		return "OTHER"
	}
}
func safeTerminal(err error) string {
	switch err.Error() {
	case "API request capacity exhausted":
		return "API request budget exhausted"
	case "API mutation capacity exhausted":
		return "API mutation capacity exhausted"
	case "API watch capacity exhausted":
		return "API watch capacity exhausted"
	case "API watch delivery capacity exhausted":
		return "API watch delivery capacity exhausted"
	case "API journal capture failed":
		return "API journal capture failed"
	case "API listener failed":
		return "API listener failed"
	case "watch delivery failed":
		return "API watch delivery failed"
	default:
		return "API failure"
	}
}

// Diagnostics is a snapshot after producers stop; it never consumes failure notifications.
func (s *Server) Diagnostics() contracts.APIDiagnostic {
	d := &s.diagnostics
	d.mu.Lock()
	defer d.mu.Unlock()
	out := contracts.APIDiagnostic{Terminal: d.terminal, Records: d.total, Truncated: d.omitted > 0 || d.saturated}
	if d.total == 0 && d.terminal == "" {
		return out
	}
	var b strings.Builder
	if d.terminal != "" {
		fmt.Fprintf(&b, "Terminal API failure: %s\n", d.terminal)
	}
	fmt.Fprintf(&b, "Earlier API rejections (not a root-cause diagnosis), total=%d:\n", d.total)
	for i := 0; i < d.used; i++ {
		e := d.entries[i]
		mode := "non-watch"
		if e.key.watch {
			mode = "WATCH"
		}
		line := fmt.Sprintf("%s %s %s: %s (%d), count=%d\n", e.key.method, e.key.resource, mode, e.key.reason, e.key.status, e.count)
		// Keep complete lines and reserve space for counts/explicit truncation.
		if b.Len()+len(line) > 1900 {
			out.Truncated = true
			break
		}
		b.WriteString(line)
	}
	if out.Truncated {
		fmt.Fprintf(&b, "[truncated] omitted occurrences=%d; counters saturated=%t\n", d.omitted, d.saturated)
	}
	out.Notice = b.String()
	return out
}
