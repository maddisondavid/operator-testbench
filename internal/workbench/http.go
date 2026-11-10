package workbench

import (
	"context"
	"embed"
	_ "embed"
	"errors"
	"net"
	"net/http"
	"net/url"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpserver"
	"operator-dynamo/internal/uiaddress"
	"operator-dynamo/internal/uijson"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/*
var assets embed.FS

type Handler struct {
	writeAuthority string
	catalog        Catalog
	events         Events
	reports        Reports
	hosts          map[string]bool
	slots          chan struct{}
	mu             sync.Mutex
	tokens         float64
	last           time.Time
	life           context.Context
}

func New(life context.Context, address, origin string, c Catalog, e Events, r Reports) (*Handler, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, "", err
	}
	hosts := map[string]bool{}
	add := func(h string) {
		key, e := uiaddress.Authority(net.JoinHostPort(h, port))
		if e == nil {
			hosts[key] = true
		}
	}
	ip := net.ParseIP(host)
	wild := host == "" || ip != nil && ip.IsUnspecified()
	if wild {
		add("127.0.0.1")
		add("::1")
		add("localhost")
		addresses, e := net.InterfaceAddrs()
		if e != nil {
			return nil, "", e
		}
		for _, a := range addresses {
			if v, _, e := net.ParseCIDR(a.String()); e == nil {
				add(v.String())
			}
		}
	} else {
		add(host)
		if ip != nil && ip.IsLoopback() {
			add("localhost")
		}
	}
	public := net.JoinHostPort(host, port)
	if wild {
		public = net.JoinHostPort("127.0.0.1", port)
	}
	printed := "http://" + public
	if origin != "" {
		normalized, e := uiaddress.Origin(origin)
		if e != nil {
			return nil, "", e
		}
		printed = normalized
		key, _ := uiaddress.BrowserOrigin(normalized)
		hosts[key] = true
	}
	return &Handler{catalog: c, events: e, reports: r, hosts: hosts, slots: make(chan struct{}, 8), tokens: 20, last: time.Now(), life: life}, printed, nil
}
func (h *Handler) admit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	h.tokens += now.Sub(h.last).Seconds() * 20
	if h.tokens > 20 {
		h.tokens = 20
	}
	h.last = now
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}
func headers(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}
func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"apiVersion":"dynamo.ui/v1alpha1","error":{"code":"` + code + `","message":"Workbench request unavailable: ` + code + `"}}`))
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	observation, observed := httpserver.ObservedHeaders(r.Context())
	received := observation.Completed
	if !observed {
		received = time.Now()
	}
	// Reserve 250ms inside the total two seconds for a precommit error.
	ctx, cancel := context.WithDeadline(r.Context(), received.Add(1750*time.Millisecond))
	defer cancel()
	stop := context.AfterFunc(h.life, cancel)
	defer stop()
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(received.Add(2 * time.Second))
	// Common admission can reject before command() sees an upload. Bound and
	// abort unread bodies before net/http flush/finalization can drain them.
	unreadBody := r.ContentLength != 0 || len(r.TransferEncoding) != 0
	if unreadBody {
		_ = controller.SetReadDeadline(received.Add(1750 * time.Millisecond))
	}
	defer func() {
		if unreadBody {
			r.Close = true
			w.Header().Set("Connection", "close")
			_ = controller.SetReadDeadline(time.Now())
		}
		if e := controller.Flush(); e != nil && !errors.Is(e, http.ErrNotSupported) {
			panic(http.ErrAbortHandler)
		}
		_ = controller.SetWriteDeadline(time.Time{})
	}()
	headers(w)
	authority, authorityError := uiaddress.Authority(r.Host)
	if authorityError != nil || !h.hosts[authority] {
		problem(w, 403, "forbidden_host")
		return
	}
	origins := r.Header.Values("Origin")
	originOK := len(origins) == 0
	if len(origins) == 1 {
		key, e := uiaddress.BrowserOrigin(origins[0])
		originOK = e == nil && key == authority
	}
	if !originOK {
		problem(w, 403, "forbidden_origin")
		return
	}
	if len(r.RequestURI) > 4096 {
		problem(w, 400, "invalid_uri")
		return
	}
	if !observed || observation.Bytes > 32768 {
		problem(w, 400, "invalid_headers")
		return
	}
	if r.URL.RawPath != "" || strings.Contains(r.URL.Path, "%") || strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "/../") {
		problem(w, 400, "invalid_path")
		return
	}
	data := strings.HasPrefix(r.URL.Path, "/ui/api/")
	if !data {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			problem(w, 405, "method_not_allowed")
			return
		}
		if r.URL.RawQuery != "" {
			problem(w, 400, "invalid_query")
			return
		}
		if r.URL.Path == "/ui" {
			http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
			return
		}
		name := ""
		content := ""
		switch r.URL.Path {
		case "/ui/":
			name = "index.html"
			content = "text/html; charset=utf-8"
		case "/ui/app.js":
			name = "app.js"
			content = "text/javascript; charset=utf-8"
		case "/ui/style.css":
			name = "style.css"
			content = "text/css; charset=utf-8"
		default:
			problem(w, 404, "not_found")
			return
		}
		b, e := assets.ReadFile("assets/" + name)
		if e != nil {
			problem(w, 500, "asset_unavailable")
			return
		}
		w.Header().Set("Content-Type", content)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if r.Method == "GET" {
			w.Write(b)
		}
		return
	}
	if strings.HasSuffix(r.URL.Path, "/commands") {
		h.command(ctx, w, r, controller, authority, &unreadBody)
		return
	}
	if r.Method != "GET" {
		w.Header().Set("Allow", "GET")
		problem(w, 405, "method_not_allowed")
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		r.Close = true
		w.Header().Set("Connection", "close")
		_ = controller.SetReadDeadline(received)
		problem(w, 400, "request_body_forbidden")
		return
	}
	if !h.admit() {
		w.Header().Set("Retry-After", "1")
		problem(w, 429, "rate_limited")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		problem(w, 429, "capacity")
		return
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		problem(w, 400, "invalid_query")
		return
	}
	for _, v := range q {
		if len(v) != 1 {
			problem(w, 400, "invalid_query")
			return
		}
	}
	envelope, lease, err := h.read(ctx, r.URL.Path, q)
	if lease != nil {
		defer lease.Release()
	}
	if err == nil {
		var b []byte
		encodeContext := ctx
		if lease != nil {
			encodeContext = lease.Context()
		}
		b, err = uijson.Encode(encodeContext, envelope)
		if err == nil && lease != nil {
			err = lease.Commit()
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			writeCtx := ctx
			if lease != nil {
				writeCtx = lease.Context()
			}
			aborted := make(chan struct{})
			abort := context.AfterFunc(writeCtx, func() { defer close(aborted); _ = controller.SetWriteDeadline(time.Now()) })
			defer func() {
				if !abort() {
					<-aborted
				}
			}()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(200)
			_, writeError := w.Write(b)
			flushError := controller.Flush()
			if writeError != nil || flushError != nil && !errors.Is(flushError, http.ErrNotSupported) || writeCtx.Err() != nil {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
	if lease != nil {
		if e := lease.Commit(); e != nil {
			err = e
		}
	}
	var re *contracts.ReadError
	if errors.As(err, &re) {
		problem(w, re.Status, re.Code)
	} else if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		problem(w, 504, "request_timeout")
	} else {
		problem(w, 500, "inspection_failed")
	}
}
func query(q url.Values, allowed ...string) bool {
	set := map[string]bool{}
	for _, s := range allowed {
		set[s] = true
	}
	for k := range q {
		if !set[k] {
			return false
		}
	}
	return true
}
func limit(q url.Values, fallback, max int) (int, error) {
	s := q.Get("limit")
	if s == "" {
		if _, ok := q["limit"]; ok {
			return 0, contracts.ReadFailure(400, "invalid_query")
		}
		return fallback, nil
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 || n > max || strconv.Itoa(n) != s {
		return 0, contracts.ReadFailure(400, "invalid_query")
	}
	return n, nil
}
func (h *Handler) read(ctx context.Context, path string, q url.Values) (map[string]any, Lease, error) {
	out := map[string]any{"apiVersion": "dynamo.ui/v1alpha1"}
	bad := func() (map[string]any, Lease, error) { return nil, nil, contracts.ReadFailure(400, "invalid_query") }
	if path == "/ui/api/v1/sessions" {
		if !query(q) {
			return bad()
		}
		v, e := h.catalog.Catalog(ctx)
		out["invocationId"] = v.ID
		out["currentRunId"] = v.CurrentRunID
		out["sessions"] = v.Sessions
		out["writeEnabled"] = h.writeAuthority != ""
		return out, nil, e
	}
	if path == "/ui/api/v1/runs" {
		if !query(q, "after", "limit") {
			return bad()
		}
		n, e := limit(q, 50, 100)
		if e != nil {
			return nil, nil, e
		}
		v, e := h.reports.ListReports(ctx, q.Get("after"), n)
		out["ids"] = v.IDs
		out["nextAfter"] = v.NextAfter
		return out, nil, e
	}
	if strings.HasPrefix(path, "/ui/api/v1/runs/") {
		id := strings.TrimPrefix(path, "/ui/api/v1/runs/")
		if !contracts.ValidID(id) || !query(q) {
			return bad()
		}
		raw, v, e := h.reports.ReadReport(ctx, id)
		out["reportJson"] = uijson.RawText(raw)
		out["reportView"] = v
		return out, nil, e
	}
	parts := strings.Split(strings.TrimPrefix(path, "/ui/api/v1/sessions/"), "/")
	if !strings.HasPrefix(path, "/ui/api/v1/sessions/") {
		return nil, nil, contracts.ReadFailure(404, "not_found")
	}
	if len(parts) != 2 || !contracts.ValidID(parts[0]) {
		return bad()
	}
	id, kind := parts[0], parts[1]
	out["sessionId"] = id
	if kind == "events" {
		if !query(q, "after", "limit") {
			return bad()
		}
		n, e := limit(q, 100, 200)
		if e != nil {
			return nil, nil, e
		}
		s := q.Get("after")
		if s == "" {
			if _, ok := q["after"]; ok {
				return bad()
			}
			s = "0"
		}
		after, e := strconv.ParseUint(s, 10, 64)
		if e != nil || strconv.FormatUint(after, 10) != s {
			return bad()
		}
		v, e := h.events.Page(ctx, id, after, n)
		events := make([]any, len(v.Events))
		for i, r := range v.Events {
			events[i] = r
		}
		out["events"] = events
		out["nextAfter"] = v.NextAfter
		out["observedFirst"] = v.ObservedFirst
		out["observedLast"] = v.ObservedLast
		out["retainedFirst"] = v.RetainedFirst
		out["retainedLast"] = v.RetainedLast
		out["gap"] = v.Gap
		return out, nil, e
	}
	if kind != "types" && kind != "objects" {
		return nil, nil, contracts.ReadFailure(404, "not_found")
	}
	if kind == "types" && !query(q) || kind == "objects" && !query(q, "apiVersion", "kind", "namespace", "name") {
		return bad()
	}
	if kind == "objects" && (q.Get("apiVersion") == "" || q.Get("kind") == "" || len(q.Get("kind")) > 253 || len(q.Get("apiVersion")) > 320) {
		return bad()
	}
	l, e := h.catalog.Acquire(ctx, id)
	if e != nil {
		return nil, nil, e
	}
	if kind == "types" {
		v, e := l.Reader().InspectionTypes(l.Context())
		out["types"] = v
		return out, l, e
	}
	if _, ok := q["name"]; ok && q.Get("name") == "" {
		return nil, l, contracts.ReadFailure(400, "invalid_query")
	}
	registered, err := l.Reader().InspectionTypes(l.Context())
	if err != nil {
		return nil, l, err
	}
	for _, t := range registered {
		if t.APIVersion == q.Get("apiVersion") && t.Kind == q.Get("kind") {
			_, provided := q["namespace"]
			if t.Scope == "Cluster" && provided || t.Scope == "Namespaced" && !provided {
				return nil, l, contracts.ReadFailure(400, "invalid_scope")
			}
		}
	}
	v, e := l.Reader().Inspect(l.Context(), q.Get("apiVersion"), q.Get("kind"), q.Get("namespace"), q.Get("name"))
	if q.Get("name") != "" {
		out["objectJson"] = v.ObjectJSON
		out["uid"] = v.UID
		out["resourceVersion"] = v.ResourceVersion
	} else {
		out["items"] = v.Items
	}
	return out, l, e
}
