package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpserver"
	"operator-dynamo/internal/journal"
	"operator-dynamo/internal/kubeapi"
	"operator-dynamo/internal/workbench"
	"strings"
	"testing"
	"time"
)

func TestSessionLeaseCloseCommitAndReplacement(t *testing.T) {
	c := newCatalog()
	ctx := context.Background()
	c.register("A", "one", 1, true)
	_, e := c.Acquire(ctx, "A")
	assertReadCode(t, e, 409)
	api := kubeapi.New("127.0.0.1:0", nil, nil)
	c.available("A", api)
	before, e := c.Acquire(ctx, "A")
	if e != nil {
		t.Fatal(e)
	}
	committed, e := c.Acquire(ctx, "A")
	if e != nil || committed.Commit() != nil {
		t.Fatal(e)
	}
	c.closing("A")
	c.ended("A", false)
	c.register("B", "two", 2, true)
	c.available("B", kubeapi.New("127.0.0.1:0", nil, nil))
	assertReadCode(t, before.Commit(), 410)
	if committed.Context().Err() == nil {
		t.Fatal("committed old response not cancelled")
	}
	if committed.Reader() != api {
		t.Fatal("rebound old lease")
	}
	_, e = c.Acquire(ctx, "A")
	assertReadCode(t, e, 410)
	_, e = c.Acquire(ctx, "unknown")
	assertReadCode(t, e, 404)
	before.Release()
	committed.Release()
	if len(c.entries["A"].leases) != 0 {
		t.Fatal("leaked leases")
	}
}
func assertReadCode(t *testing.T, e error, n int) {
	t.Helper()
	var r *contracts.ReadError
	if !errors.As(e, &r) || r.Status != n {
		t.Fatalf("got %v want %d", e, n)
	}
}

func TestReportOnlyWorkbenchLifecycleAndDisabledRoutes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			a := New()
			var server *httpserver.Server
			a.Prepare = func(o contracts.Options, w Workspace, j Journal) ([]Component, error) {
				server = httpserver.New("127.0.0.1:0")
				return []Component{server}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			ready := make(chan string, 1)
			a.Prepare = func(o contracts.Options, w Workspace, j Journal) ([]Component, error) {
				server = httpserver.New("127.0.0.1:0")
				return []Component{server, &announceServer{server: server, ready: ready}}, nil
			}
			go func() {
				done <- a.Run(ctx, contracts.Options{Workdir: t.TempDir(), Listen: "127.0.0.1:0", UI: enabled})
			}()
			var address string
			select {
			case address = <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("startup timeout")
			}
			res, e := http.Get("http://" + address + "/ui/api/v1/sessions")
			if e != nil {
				t.Fatal(e)
			}
			defer res.Body.Close()
			want := 404
			if enabled {
				want = 200
				var body map[string]any
				if json.NewDecoder(res.Body).Decode(&body) != nil {
					t.Fatal("bad catalog")
				}
				sessions := body["sessions"].([]any)
				if len(sessions) != 1 {
					t.Fatal(body)
				}
			}
			if res.StatusCode != want {
				t.Fatal(res.StatusCode)
			}
			cancel()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown timeout")
			}
		})
	}
}

type announceServer struct {
	server *httpserver.Server
	ready  chan string
}

func (a *announceServer) Start(context.Context, contracts.Session) error {
	a.ready <- a.server.Address()
	return nil
}
func (a *announceServer) Stop(context.Context) error { return nil }

type barrierReader struct {
	entered chan struct{}
	release chan struct{}
}

func (b *barrierReader) InspectionTypes(ctx context.Context) ([]contracts.ResourceType, error) {
	close(b.entered)
	select {
	case <-b.release:
		return []contracts.ResourceType{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *barrierReader) Inspect(context.Context, string, string, string, string) (contracts.ResourceSnapshot, error) {
	return contracts.ResourceSnapshot{ObjectJSON: `{"metadata":{"uid":"old"}}`}, nil
}
func TestHTTPLeaseBeforeCommitClose(t *testing.T) {
	ctx := context.Background()
	c := newCatalog()
	id := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	c.register(id, "first", 1, true)
	r := &barrierReader{entered: make(chan struct{}), release: make(chan struct{})}
	c.available(id, r)
	windows := journal.NewWindows()
	windows.Register(id)
	h, _, e := workbench.New(ctx, "127.0.0.1:8080", "", c, windows, nil)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/ui/api/v1/sessions/"+id+"/types", nil)
	done := make(chan struct{})
	go func() { defer close(done); serveObservedLease(t, h, w, req) }()
	<-r.entered
	c.closing(id)
	c.register("BCDEFGHIJKLMNOPQRSTUVWXYZA", "second", 2, true)
	close(r.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read not cancelled")
	}
	if w.Code != 410 || strings.Contains(w.Body.String(), "second") {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(c.entries[id].leases) != 0 {
		t.Fatal("lease leak")
	}
}

type committedWriter struct {
	*httptest.ResponseRecorder
	committed, release chan struct{}
}

func (w *committedWriter) WriteHeader(code int) {
	w.ResponseRecorder.WriteHeader(code)
	if code == 200 {
		close(w.committed)
		<-w.release
	}
}
func TestHTTPCommitWinningCloseKeepsOldIdentity(t *testing.T) {
	c := newCatalog()
	id := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	c.register(id, "first", 1, true)
	r := &barrierReader{entered: make(chan struct{}), release: make(chan struct{})}
	close(r.release)
	c.available(id, r)
	windows := journal.NewWindows()
	windows.Register(id)
	h, _, e := workbench.New(context.Background(), "127.0.0.1:8080", "", c, windows, nil)
	if e != nil {
		t.Fatal(e)
	}
	w := &committedWriter{ResponseRecorder: httptest.NewRecorder(), committed: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil && p != http.ErrAbortHandler {
				t.Error(p)
			}
		}()
		serveObservedLease(t, h, w, httptest.NewRequest("GET", "http://127.0.0.1:8080/ui/api/v1/sessions/"+id+"/types", nil))
	}()
	<-w.committed
	c.closing(id)
	c.register("BCDEFGHIJKLMNOPQRSTUVWXYZA", "second", 2, true)
	close(w.release)
	<-done
	if w.Code != 200 || strings.Contains(w.Body.String(), "second") || strings.Contains(w.Body.String(), "session_ended") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w.Body.Len() > 0 && !strings.Contains(w.Body.String(), id) {
		t.Fatal("old identity missing")
	}
}

// Keep deterministic response barriers while obtaining real ingress evidence
// from the same adapter used by production, rather than inventing raw counts.
func serveObservedLease(t *testing.T, h http.Handler, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, observed *http.Request) { h.ServeHTTP(w, observed) }))
	s.Listener = httpserver.ObserveHeaders(s.Config, s.Listener)
	s.Start()
	defer s.Close()
	r = r.Clone(r.Context())
	r.URL.Host = strings.TrimPrefix(s.URL, "http://")
	r.RequestURI = ""
	res, e := s.Client().Do(r)
	if e == nil {
		res.Body.Close()
	} // after-commit cancellation may abort transport
}

func TestWriteCatalogScopeAndClosure(t *testing.T) {
	c := newCatalog()
	ctx := context.Background()
	c.register("A", "", 1, true)
	cmd := contracts.ResourceCommand{SessionID: "A", Operation: "create", APIVersion: "v1", Kind: "Namespace", Name: "one", Object: json.RawMessage(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"one"}}`)}
	_, err := c.ExecuteCommand(ctx, "A", cmd)
	assertReadCode(t, err, 403)
	c.writeEnabled = true
	_, err = c.ExecuteCommand(ctx, "A", cmd)
	assertReadCode(t, err, 409)
	reg, _ := kubeapi.Builtin("v1/namespaces")
	api := kubeapi.New("127.0.0.1:0", []kubeapi.Registration{reg}, nil)
	c.available("A", api)
	c.ready("A")
	c.runID = "test-run"
	_, err = c.ExecuteCommand(ctx, "A", cmd)
	assertReadCode(t, err, 403)
	c.runID = ""
	c.closing("A")
	c.ended("A", false)
	c.register("B", "", 2, true)
	c.available("B", kubeapi.New("127.0.0.1:0", nil, nil))
	c.ready("B")
	_, err = c.ExecuteCommand(ctx, "A", cmd)
	assertReadCode(t, err, 410)
	cmd.SessionID = "missing"
	_, err = c.ExecuteCommand(ctx, "missing", cmd)
	assertReadCode(t, err, 404)
	// A saved reference cannot bypass catalog closure after the old lease ends.
	cmd.SessionID = "A"
	result, err := api.Command(ctx, cmd)
	if err != nil || result.Status != 410 {
		t.Fatal(result, err)
	}
}
