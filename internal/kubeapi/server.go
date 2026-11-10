package kubeapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"

	"net"
	"net/http"
	"net/url"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpdrain"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Recorder func(contracts.Observation) error
type Limits struct {
	Resources, Watches, Requests, History int
	BodyBytes                             int64
}

func DefaultLimits() Limits {
	return Limits{Resources: 256, Watches: 16, Requests: 8192, History: 1024, BodyBytes: 1 << 20}
}

type event struct {
	reg, namespace string
	kind           string
	version        uint64
	object         json.RawMessage
}
type subscription struct {
	reg, namespace string
	ch             chan event
	ready          bool
	ctx            context.Context
}
type Server struct {
	commandsClosed        bool
	diagnostics           rejectionSummary
	connections           httpdrain.Connections
	Listen                string
	Registrations         []Registration
	RequiredWatches       []string
	RequireAllNamespaces  bool
	InitialWatchReadiness bool
	Limits                Limits
	Record                Recorder
	mu                    sync.Mutex
	objects               map[string]storedObject
	history               []event
	version               uint64
	subscriptions         map[*subscription]bool
	listed                map[string]bool
	Connected             chan struct{}
	connectOnce           sync.Once
	failures              chan error
	failOnce              sync.Once
	requests              atomic.Int64
	server                *http.Server
	listener              net.Listener
	ctx                   context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
}

func New(listen string, regs []Registration, record Recorder) *Server {
	required := []string{}
	for _, r := range regs {
		required = append(required, r.Key())
	}
	return &Server{RequiredWatches: required, Listen: listen, Registrations: append([]Registration(nil), regs...), Record: record, Limits: DefaultLimits(), objects: map[string]storedObject{}, version: 1, subscriptions: map[*subscription]bool{}, listed: map[string]bool{}, Connected: make(chan struct{}), failures: make(chan error, 1)}
}
func (s *Server) Failures() <-chan error { return s.failures }
func (s *Server) fail(err error) {
	s.failOnce.Do(func() {
		s.diagnostics.mu.Lock()
		s.diagnostics.terminal = safeTerminal(err)
		s.diagnostics.mu.Unlock()
		s.failures <- err
		if s.cancel != nil {
			s.cancel()
		}
	})
}
func (s *Server) record(kind, corr string, p contracts.LifecyclePayload) bool {
	if s.Record == nil {
		return true
	}
	if err := s.Record(contracts.Observation{Kind: kind, CorrelationID: corr, Payload: p}); err != nil {
		s.fail(errors.New("API journal capture failed"))
		return false
	}
	return true
}
func (s *Server) Start(ctx context.Context, session contracts.Session) error {
	if err := ValidateRegistrations(s.Registrations); err != nil {
		return err
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	l, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	s.listener = l
	s.server = &http.Server{ConnState: s.connections.State, Handler: s, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return s.ctx }}
	for _, r := range s.Registrations {
		if r.Group != "batch" && r.Group != "" {
			if !s.record("api.registration", session.ID, contracts.LifecyclePayload{Resource: r.Key(), Notice: RegistrationNotice}) {
				l.Close()
				return errors.New("registration capture failed")
			}
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.fail(errors.New("API listener failed"))
		}
	}()
	return nil
}
func (s *Server) Address() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}
func (s *Server) Stop(ctx context.Context) error {
	s.CloseCommands()
	if s.cancel != nil {
		s.cancel()
	}
	if s.server == nil {
		return nil
	}
	s.connections.ClosePending()
	err := s.server.Shutdown(ctx)
	if err != nil {
		_ = s.server.Close()
	}
	s.wg.Wait()
	return err
}
func writeJSON(w http.ResponseWriter, code int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(obj)
}
func status(w http.ResponseWriter, code int, reason string) {
	if observer, ok := w.(interface{ Rejected(int, string) }); ok {
		observer.Rejected(code, reason)
	}
	writeJSON(w, code, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason, "message": reason, "code": code})
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	w = &rejectionWriter{ResponseWriter: w, server: s, request: r}
	corr := rand.Text()
	w.Header().Set("X-Dynamo-Request-ID", corr)
	p := contracts.LifecyclePayload{Operation: diagnosticMethod(r.Method), Status: 500}
	// Deliberately omit paths, query values, headers and request bodies from errors/traces.
	defer func() {
		p.DurationNS = time.Since(started).Nanoseconds()
		captured := s.record("api.request", corr, p)
		if call, ok := r.Context().Value(commandKey{}).(*commandCall); ok {
			call.state = p.State
			call.captureFailed = !captured
		}
	}()
	reject := func(code int, reason string) { p.Status = code; status(w, code, reason) }
	if s.requests.Add(1) > int64(s.Limits.Requests) {
		reject(503, "SessionCapacityExceeded")
		s.fail(errors.New("API request capacity exhausted"))
		return
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		reject(503, "SessionFailed")
		return
	}
	if !acceptsJSON(strings.Join(r.Header.Values("Accept"), ",")) {
		reject(406, "NotAcceptable")
		return
	}
	if r.Method == http.MethodGet && s.discovery(w, r, &p) {
		return
	}
	// Resolve collection/object routes before considering a subresource:
	// "status" is also a valid resource name (and plural).
	reg, ns, name, ok := s.route(r.URL.Path)
	statusOnly := false
	if !ok && strings.HasSuffix(r.URL.Path, "/status") {
		reg, ns, name, ok = s.route(strings.TrimSuffix(r.URL.Path, "/status"))
		statusOnly = ok && name != "" && reg.Status
		ok = statusOnly
	}
	if !ok {
		reject(404, "NotFound")
		return
	}
	p.Resource = reg.Key()
	if statusOnly {
		p.Resource += "/status"
	}
	p.Namespace = ns
	p.Name = name
	if (statusOnly && r.Method != "GET" && r.Method != "PUT") || (!statusOnly && r.Method != "GET" && r.Method != "POST" && r.Method != "PUT" && r.Method != "DELETE" && !(r.Method == "PATCH" && (deploymentPatch(reg) || configMapPatch(reg)))) {
		reject(405, "MethodNotAllowed")
		return
	}
	if !mutationQueryValid(s, r, reg, name, statusOnly) {
		reject(400, "UnsupportedQuery")
		return
	}
	if r.Method != "GET" {
		if (r.Method == "POST" && name != "") || (r.Method != "POST" && name == "") || (reg.Scope == "Namespaced" && ns == "") {
			reject(405, "MethodNotAllowed")
			return
		}
		s.mutate(w, r, reg, ns, name, statusOnly, corr, &p)
		return
	}
	if r.URL.Query().Get("watch") == "true" {
		if name != "" {
			reject(400, "UnsupportedQuery")
			return
		}
		s.watch(w, r, reg, ns, corr, &p)
		return
	}
	s.mu.Lock()
	items, exists := s.rawSnapshotLocked(reg, ns, name)
	if name != "" {
		s.mu.Unlock()
		if !exists {
			reject(404, "NotFound")
			return
		}
		p.Status = 200
		writeJSON(w, 200, items[0])
		return
	}
	if selector := r.URL.Query().Get("fieldSelector"); selector != "" {
		wanted := strings.TrimPrefix(selector, "metadata.name=")
		filtered := make([]json.RawMessage, 0, len(items))
		for _, raw := range items {
			if storeObject(raw).summary.Name == wanted {
				filtered = append(filtered, raw)
			}
		}
		items = filtered
	}
	rv := strconv.FormatUint(s.version, 10)
	if r.URL.Query().Get("fieldSelector") == "" {
		s.listed[reg.Key()+"/"+ns+"/"+rv] = true
	}
	s.mu.Unlock()
	p.Version = rv
	p.Status = 200
	writeJSON(w, 200, map[string]any{"apiVersion": reg.APIVersion(), "kind": reg.ListKind, "metadata": map[string]string{"resourceVersion": rv}, "items": items})
}
func (s *Server) queryValid(r *http.Request, name string) bool {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	for k, v := range q {
		if len(v) != 1 {
			return false
		}
		value := v[0]
		switch k {
		case "watch":
			if r.Method != "GET" || (value != "true" && value != "false") {
				return false
			}
		case "resourceVersion":
			if r.Method != "GET" {
				return false
			}
			if value != "" && value != "0" {
				if r.URL.Query().Get("watch") != "true" {
					return false
				}
				if _, err := strconv.ParseUint(value, 10, 64); err != nil {
					return false
				}
			}
		case "timeout":
			n, err := strconv.Atoi(r.URL.Query().Get("timeoutSeconds"))
			d, durationErr := time.ParseDuration(value)
			if r.Method != "GET" || name != "" || r.URL.Query().Get("watch") != "true" || err != nil || n < 1 || n > 3600 || durationErr != nil || d <= 0 || d != time.Duration(n)*time.Second {
				return false
			}
		case "timeoutSeconds":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 3600 || r.URL.Query().Get("watch") != "true" {
				return false
			}
		case "allowWatchBookmarks":
			if value != "true" && value != "false" {
				return false
			}
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < s.Limits.Resources || r.Method != "GET" || name != "" || r.URL.Query().Get("watch") == "true" {
				return false
			}
		case "fieldSelector":
			if value != "" {
				reg, _, _, ok := s.route(r.URL.Path)
				wanted, valid := strings.CutPrefix(value, "metadata.name=")
				if !ok || (reg.Key() != "v1/configmaps" && reg.Key() != "v1/secrets") || r.Method != "GET" || name != "" || r.URL.Query().Get("watch") == "true" || !valid || !ValidName(wanted) {
					return false
				}
			}
		case "labelSelector":
			if value != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func (s *Server) route(path string) (Registration, string, string, bool) {
	for _, reg := range s.Registrations {
		prefix := "/apis/" + reg.APIVersion() + "/"
		if reg.Group == "" {
			prefix = "/api/" + reg.Version + "/"
		}
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
		ns := ""
		namespacedPath := len(parts) >= 3 && parts[0] == "namespaces"
		if namespacedPath {
			ns = parts[1]
			parts = parts[2:]
		}
		if len(parts) < 1 || parts[0] != reg.Plural {
			continue
		}
		if namespacedPath && (reg.Scope != "Namespaced" || !ValidName(ns) || strings.Contains(ns, ".") || len(ns) > 63) {
			return Registration{}, "", "", false
		}
		if len(parts) == 1 {
			return reg, ns, "", true
		}
		if len(parts) == 2 && ValidName(parts[1]) && (reg.Scope != "Namespaced" || ns != "") {
			return reg, ns, parts[1], true
		}
	}
	return Registration{}, "", "", false
}
func (s *Server) discovery(w http.ResponseWriter, r *http.Request, p *contracts.LifecyclePayload) bool {
	if r.URL.RawQuery != "" {
		return false
	}
	path := r.URL.Path
	send := func(obj any) { p.Status = 200; p.Resource = "discovery"; writeJSON(w, 200, obj) }
	if path == "/version" {
		send(map[string]string{"major": "1", "minor": "33", "gitVersion": "v1.33.0-dynamo-profile"})
		return true
	}
	groups := map[string][]string{}
	for _, reg := range s.Registrations {
		found := false
		for _, v := range groups[reg.Group] {
			if v == reg.Version {
				found = true
			}
		}
		if !found {
			groups[reg.Group] = append(groups[reg.Group], reg.Version)
		}
	}
	group := func(g string) map[string]any {
		vs := []map[string]string{}
		sort.Strings(groups[g])
		for _, v := range groups[g] {
			vs = append(vs, map[string]string{"groupVersion": g + "/" + v, "version": v})
		}
		return map[string]any{"apiVersion": "v1", "kind": "APIGroup", "name": g, "versions": vs, "preferredVersion": vs[0]}
	}
	if path == "/api" {
		versions := groups[""]
		if versions == nil {
			versions = []string{}
		}
		send(map[string]any{"apiVersion": "v1", "kind": "APIVersions", "versions": versions})
		return true
	}
	if path == "/apis" {
		list := []map[string]any{}
		names := []string{}
		for g := range groups {
			if g != "" {
				names = append(names, g)
			}
		}
		sort.Strings(names)
		for _, g := range names {
			list = append(list, group(g))
		}
		send(map[string]any{"apiVersion": "v1", "kind": "APIGroupList", "groups": list})
		return true
	}
	for g := range groups {
		if g != "" && path == "/apis/"+g {
			send(group(g))
			return true
		}
	}
	for _, reg := range s.Registrations {
		prefix := "/apis/" + reg.APIVersion()
		if reg.Group == "" {
			prefix = "/api/" + reg.Version
		}
		if path != prefix {
			continue
		}
		resources := []map[string]any{}
		for _, other := range s.Registrations {
			if other.APIVersion() == reg.APIVersion() {
				verbs := []string{"get", "list", "watch", "create", "update", "delete"}
				if deploymentPatch(other) || configMapPatch(other) {
					verbs = append(verbs, "patch")
				}
				resources = append(resources, map[string]any{"name": other.Plural, "singularName": other.Singular, "namespaced": other.Scope == "Namespaced", "kind": other.Kind, "verbs": verbs})
				if other.Status {
					resources = append(resources, map[string]any{"name": other.Plural + "/status", "singularName": "", "namespaced": other.Scope == "Namespaced", "kind": other.Kind, "verbs": []string{"get", "update"}})
				}
			}
		}
		send(map[string]any{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": reg.APIVersion(), "resources": resources})
		return true
	}
	return false
}
