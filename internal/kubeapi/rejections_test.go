package kubeapi

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRejectionSummaryDefaultBudget(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := New("", []Registration{reg}, nil)
	for i := 0; i <= DefaultLimits().Requests; i++ {
		req := httptest.NewRequest("GET", "/api/v1/configmaps?watch=true&resourceVersion=1&fieldSelector=PRIVATE_CANARY", nil)
		s.ServeHTTP(httptest.NewRecorder(), req)
	}
	d := s.Diagnostics()
	if d.Records != 8193 || d.Terminal != "API request budget exhausted" || !strings.Contains(d.Notice, "UnsupportedQuery (400), count=8192") || strings.Contains(d.Notice, "PRIVATE_CANARY") {
		t.Fatal(d)
	}
	if len(d.Notice) > 2048 {
		t.Fatal("unbounded")
	}
	fresh := New("", []Registration{reg}, nil).Diagnostics()
	if fresh.Records != 0 || fresh.Notice != "" {
		t.Fatal(fresh)
	}
}
func TestRejectionSummaryBoundsAndConcurrency(t *testing.T) {
	s := New("", nil, nil)
	for i := 0; i < 10; i++ {
		s.diagnostics.add(rejectionKey{method: "GET", resource: strings.Repeat("r", 300), reason: "NotFound", status: 400 + i})
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				s.diagnostics.add(rejectionKey{method: "GET", resource: strings.Repeat("r", 300), reason: "NotFound", status: 400})
			}
		}()
	}
	wg.Wait()
	d := s.Diagnostics()
	if d.Records != 2010 || !d.Truncated || len(d.Notice) > 2048 || !strings.Contains(d.Notice, "count=2001") || !strings.Contains(d.Notice, "omitted occurrences=2") {
		t.Fatal(d)
	}
	s.diagnostics.mu.Lock()
	s.diagnostics.total = math.MaxUint64
	s.diagnostics.entries[0].count = math.MaxUint64
	s.diagnostics.mu.Unlock()
	s.diagnostics.add(s.diagnostics.entries[0].key)
	if d = s.Diagnostics(); !d.Truncated || !strings.Contains(d.Notice, "saturated=true") {
		t.Fatal(d)
	}
}
func TestRejectionFallbackDoesNotFailSession(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	request(t, s, "GET", "/api/v1/configmaps?watch=true&sendInitialEvents=true", "")
	_, list := request(t, s, "GET", "/api/v1/configmaps", "")
	rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+"/api/v1/configmaps?watch=true&resourceVersion="+rv, nil)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	select {
	case <-s.Connected:
	case <-time.After(time.Second):
		t.Fatal("fallback not connected")
	}
	d := s.Diagnostics()
	if d.Records != 1 || d.Terminal != "" {
		t.Fatal(d)
	}
	select {
	case e := <-s.Failures():
		t.Fatal(e)
	default:
	}
}
func TestRejectionUnknownInputNotRetained(t *testing.T) {
	s := New("", nil, nil)
	req := httptest.NewRequest("PRIVATE_METHOD", "/PRIVATE_PATH?watch=true&private=VALUE", strings.NewReader("PRIVATE_BODY"))
	req.Header.Set("Authorization", "PRIVATE_TOKEN")
	s.ServeHTTP(httptest.NewRecorder(), req)
	d := s.Diagnostics()
	if strings.Contains(d.Notice, "PRIVATE") || !strings.Contains(d.Notice, "OTHER unregistered-route WATCH") {
		t.Fatal(d)
	}
}
