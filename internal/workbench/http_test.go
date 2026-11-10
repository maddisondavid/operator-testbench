package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpserver"
	"strings"
	"testing"
	"time"
)

const testID = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

type stubPorts struct{}

func (stubPorts) Catalog(context.Context) (contracts.Invocation, error) {
	return contracts.Invocation{ID: testID, Sessions: []contracts.SessionDescriptor{}}, nil
}
func (stubPorts) Acquire(context.Context, string) (Lease, error) {
	return nil, contracts.ReadFailure(410, "session_ended")
}
func (stubPorts) Page(context.Context, string, uint64, int) (contracts.EventPage, error) {
	return contracts.EventPage{Events: []json.RawMessage{}, ObservedLast: "0", NextAfter: "0"}, nil
}
func (stubPorts) ListReports(context.Context, string, int) (contracts.RunPage, error) {
	return contracts.RunPage{IDs: []string{}}, nil
}
func (stubPorts) ReadReport(context.Context, string) ([]byte, map[string]any, error) {
	return nil, nil, contracts.ReadFailure(422, "invalid_report")
}
func handler(t *testing.T) *Handler {
	t.Helper()
	p := stubPorts{}
	h, url, e := New(context.Background(), "0.0.0.0:8080", "http://198.51.100.10:18080", p, p, p)
	if e != nil || url != "http://198.51.100.10:18080" {
		t.Fatal(url, e)
	}
	return h
}
func TestStrictRoutesAuthoritiesAndFraming(t *testing.T) {
	for _, tc := range []struct {
		method, path, host, origin string
		code                       int
		body                       bool
	}{
		{"GET", "/ui", "evil.test:8080", "", 403, false}, {"GET", "/ui", "198.51.100.10:18080", "", 307, false}, {"GET", "/ui/", "198.51.100.10:8080", "", 403, false}, {"GET", "/ui/", "198.51.100.10:18080", "http://evil.test", 403, false}, {"GET", "/ui/", "198.51.100.10:18080", "http://198.51.100.10:18080", 200, false},
		{"HEAD", "/ui/app.js", "198.51.100.10:18080", "", 200, false}, {"POST", "/ui/api/v1/sessions", "198.51.100.10:18080", "", 405, false}, {"GET", "/ui/api/v1/sessions?unexpected=x", "198.51.100.10:18080", "", 400, false}, {"GET", "/ui/api/v1/sessions?x=1&x=2", "198.51.100.10:18080", "", 400, false}, {"GET", "/ui/api/v1/sessions", "198.51.100.10:18080", "", 400, true}, {"GET", "/ui/api/v1/sessions/../types", "198.51.100.10:18080", "", 400, false}, {"GET", "/ui/api/v1/runs/%2e%2e", "198.51.100.10:18080", "", 400, false}, {"GET", "/ui/api/v1/sessions/" + testID + "/types", "198.51.100.10:18080", "", 410, false}, {"GET", "/ui/api/v1/sessions/" + testID + "/events?after=01", "198.51.100.10:18080", "", 400, false}, {"GET", "/ui/api/v1/runs/" + testID, "198.51.100.10:18080", "", 422, false},
	} {
		t.Run(tc.method+tc.path+tc.host, func(t *testing.T) {
			h := handler(t)
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.body {
				r.TransferEncoding = []string{"chunked"}
			}
			w := httptest.NewRecorder()
			serveObserved(t, h, w, r)
			if w.Code != tc.code {
				t.Fatal(w.Code, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Fatal(w.Header())
			}
			if tc.method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD body")
			}
		})
	}
}
func TestIndependentRateAndConcurrentCapacity(t *testing.T) {
	h := handler(t)
	for i := 0; i < 8; i++ {
		h.slots <- struct{}{}
	}
	r := httptest.NewRequest("GET", "http://198.51.100.10:18080/ui/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	serveObserved(t, h, w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal(w.Code)
	}
	for i := 0; i < 8; i++ {
		<-h.slots
	}
	h.tokens = 0
	h.last = time.Now()
	w = httptest.NewRecorder()
	serveObserved(t, h, w, r)
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
}
func TestAssetAndDataSecurity(t *testing.T) {
	h := handler(t)
	s := observedServer(h)
	defer s.Close()
	r, _ := http.NewRequest("GET", s.URL+"/ui/api/v1/sessions", nil)
	r.Host = "198.51.100.10:18080"
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(res.StatusCode, res.Header)
	}
}

type slowReports struct{ stubPorts }

func (slowReports) ReadReport(context.Context, string) ([]byte, map[string]any, error) {
	return []byte(strings.Repeat("x", 8<<20)), map[string]any{"status": "running"}, nil
}
func TestRealSlowClientReleasesAdmission(t *testing.T) {
	h := handler(t)
	h.reports = slowReports{}
	server := observedServer(h)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	conn, e := net.Dial("tcp", address)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(1024)
	fmt.Fprintf(conn, "GET /ui/api/v1/runs/%s HTTP/1.1\r\nHost: 198.51.100.10:18080\r\n\r\n", testID)
	deadline := time.Now().Add(time.Second)
	for len(h.slots) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(h.slots) == 0 {
		t.Fatal("request not admitted")
	}
	deadline = time.Now().Add(3 * time.Second)
	for len(h.slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.slots) != 0 {
		t.Fatal("slow client retained admission")
	}
}

type blockedCatalog struct{ stubPorts }

func (blockedCatalog) Catalog(ctx context.Context) (contracts.Invocation, error) {
	<-ctx.Done()
	return contracts.Invocation{}, ctx.Err()
}
func TestBackendDeadlineAndInvocationCancellation(t *testing.T) {
	for _, closing := range []bool{false, true} {
		life, cancel := context.WithCancel(context.Background())
		p := stubPorts{}
		h, _, e := New(life, "127.0.0.1:8080", "http://198.51.100.10:18080", blockedCatalog{}, p, p)
		if e != nil {
			t.Fatal(e)
		}
		done := make(chan struct{})
		w := httptest.NewRecorder()
		start := time.Now()
		go func() {
			defer close(done)
			serveObserved(t, h, w, httptest.NewRequest("GET", "http://198.51.100.10:18080/ui/api/v1/sessions", nil))
		}()
		if closing {
			cancel()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("unbounded backend")
		}
		cancel()
		if w.Code != 504 || len(h.slots) != 0 {
			t.Fatal(w.Code, len(h.slots))
		}
		if closing && time.Since(start) > time.Second {
			t.Fatal("invocation cancellation delayed")
		}
	}
}

func observedServer(h http.Handler) *httptest.Server {
	s := httptest.NewUnstartedServer(h)
	s.Listener = httpserver.ObserveHeaders(s.Config, s.Listener)
	s.Start()
	return s
}
func serveObserved(t *testing.T, h http.Handler, w *httptest.ResponseRecorder, r *http.Request) {
	t.Helper()
	s := observedServer(h)
	defer s.Close()
	r = r.Clone(r.Context())
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(s.URL, "http://")
	r.RequestURI = ""
	if len(r.TransferEncoding) > 0 {
		r.Body = io.NopCloser(strings.NewReader("x"))
		r.ContentLength = -1
	}
	c := s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := c.Do(r)
	if e != nil {
		t.Error(e)
		return
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	_, e = io.Copy(w, res.Body)
	if e != nil {
		t.Error(e)
	}
}
func TestCanonicalBrowserAuthority(t *testing.T) {
	for _, host := range []string{"198.51.100.10", "localhost", "[::1]", "[2001:db8::1]"} {
		declared := host
		if host == "[2001:db8::1]" {
			declared = "[2001:0db8:0000:0000:0000:0000:0000:0001]"
		}
		p := stubPorts{}
		h, _, e := New(context.Background(), "127.0.0.1:8080", "http://"+declared+":80", p, p, p)
		if e != nil {
			t.Fatal(e)
		}
		for _, suffix := range []string{"", ":80", ":81"} {
			r := httptest.NewRequest("GET", "http://"+host+suffix+"/ui/api/v1/sessions", nil)
			r.Header.Set("Origin", "http://"+host)
			w := httptest.NewRecorder()
			serveObserved(t, h, w, r)
			want := 200
			if suffix == ":81" {
				want = 403
			}
			if w.Code != want {
				t.Fatal(host, suffix, w.Code)
			}
		}
	}
}
func TestMissingIngressEvidenceFailsClosed(t *testing.T) {
	h := handler(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://198.51.100.10:18080/ui/", nil))
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
