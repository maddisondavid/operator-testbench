package kubeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
	"time"
)

var widget = Registration{Group: "example.test", Version: "v1", Kind: "Widget", ListKind: "WidgetList", Plural: "widgets", Singular: "widget", Scope: "Namespaced"}

func serve(t *testing.T, regs []Registration, limits Limits, record Recorder) *Server {
	t.Helper()
	s := New("127.0.0.1:0", regs, record)
	s.Limits = limits
	if err := s.Start(context.Background(), contracts.Session{ID: "test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func request(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+s.Address()+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var obj map[string]any
	decoder := json.NewDecoder(res.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, obj
}
func object(name string) string {
	return fmt.Sprintf(`{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":%q},"spec":{"number":"schema-invalid-marker"}}`, name)
}

const collection = "/apis/example.test/v1/namespaces/demo/widgets"

func TestPublicCreateListWatchHandoffAndCancellation(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	code, list := request(t, s, "GET", collection, "")
	if code != 200 || len(list["items"].([]any)) != 0 {
		t.Fatal(code, list)
	}
	rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
	// Barrier: CREATE has committed after LIST returned and before WATCH starts.
	code, created := request(t, s, "POST", collection, object("between"))
	if code != 201 {
		t.Fatal(code, created)
	}
	meta := created["metadata"].(map[string]any)
	if meta["uid"] == "" || meta["creationTimestamp"] == "" || meta["resourceVersion"] == rv {
		t.Fatal(meta)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+collection+"?watch=true&resourceVersion="+rv, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	var observed struct {
		Type   string
		Object map[string]any
	}
	if err := dec.Decode(&observed); err != nil {
		t.Fatal(err)
	}
	if observed.Type != "ADDED" || observed.Object["metadata"].(map[string]any)["uid"] != meta["uid"] {
		t.Fatal(observed)
	}
	code, _ = request(t, s, "POST", collection, object("live"))
	if code != 201 {
		t.Fatal(code)
	}
	if err := dec.Decode(&observed); err != nil {
		t.Fatal(err)
	}
	if observed.Object["metadata"].(map[string]any)["name"] != "live" {
		t.Fatal(observed)
	}
	cancel()
	res.Body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		n := len(s.subscriptions)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watch leaked")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestNegativeRequestsDoNotMutateAndSchemaLimitation(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", collection, object("one"), 201}, {"POST", collection, object("one"), 409},
		{"POST", collection, `{"kind":"Wrong"}`, 400}, {"POST", collection, strings.Replace(object("two"), `"name":"two"`, `"name":"two","namespace":"other"`, 1), 400},
		{"POST", collection, `{`, 400}, {"PATCH", collection + "/one", object("one"), 405},
		{"GET", collection + "?labelSelector=secret", "", 400}, {"GET", collection + "?unknown=true", "", 400},
		{"GET", "/apis/missing/v1/objects", "", 404}, {"GET", collection + "/one/status", "", 404},
		{"GET", collection + "?watch=true&resourceVersion=9999", "", 400},
	} {
		code, obj := request(t, s, tc.method, tc.path, tc.body)
		if code != tc.code {
			t.Fatalf("%s %s: %d %v", tc.method, tc.path, code, obj)
		}
	}
	_, list := request(t, s, "GET", collection, "")
	if len(list["items"].([]any)) != 1 {
		t.Fatal(list)
	}
	_, obj := request(t, s, "GET", collection+"/one", "")
	if obj["spec"].(map[string]any)["number"] != "schema-invalid-marker" {
		t.Fatal("metadata-only limitation missing")
	}
}
func TestScopesAndFreshServers(t *testing.T) {
	cluster := widget
	cluster.Scope = "Cluster"
	cluster.Kind = "ClusterWidget"
	cluster.Plural = "clusterwidgets"
	a := serve(t, []Registration{widget, cluster}, DefaultLimits(), nil)
	b := serve(t, []Registration{widget}, DefaultLimits(), nil)
	code, _ := request(t, a, "POST", collection, object("same"))
	if code != 201 {
		t.Fatal(code)
	}
	code, _ = request(t, b, "GET", collection+"/same", "")
	if code != 404 {
		t.Fatal("state crossed sessions")
	}
	code, _ = request(t, a, "POST", "/apis/example.test/v1/clusterwidgets", strings.Replace(object("cluster"), `"Widget"`, `"ClusterWidget"`, 1))
	if code != 201 {
		t.Fatal(code)
	}
	code, _ = request(t, a, "GET", "/apis/example.test/v1/namespaces/demo/clusterwidgets", "")
	if code != 404 {
		t.Fatal(code)
	}
	code, list := request(t, a, "GET", "/apis/example.test/v1/widgets", "")
	if code != 200 || len(list["items"].([]any)) != 1 {
		t.Fatal(code, list)
	}
}
func TestCapacityAndCaptureFailures(t *testing.T) {
	limits := DefaultLimits()
	limits.Resources = 1
	s := serve(t, []Registration{widget}, limits, nil)
	request(t, s, "POST", collection, object("one"))
	code, _ := request(t, s, "POST", collection, object("two"))
	if code != 503 {
		t.Fatal(code)
	}
	select {
	case <-s.Failures():
	case <-time.After(time.Second):
		t.Fatal("capacity did not fail session")
	}
	s.mu.Lock()
	if len(s.objects) != 1 {
		t.Fatal("failed create mutated")
	}
	s.mu.Unlock()
	var captured bytes.Buffer
	c := serve(t, []Registration{widget}, DefaultLimits(), func(o contracts.Observation) error {
		json.NewEncoder(&captured).Encode(o)
		if o.Kind == "resource.created" {
			return errors.New("injected")
		}
		return nil
	})
	code, _ = request(t, c, "POST", collection, object("one"))
	if code != 503 {
		t.Fatal(code)
	}
	c.mu.Lock()
	if len(c.objects) != 1 {
		t.Fatal("committed create rolled back")
	}
	c.mu.Unlock()
	// Wait for response-deferred request capture before reading buffer via shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.Stop(ctx)
	if strings.Contains(captured.String(), "schema-invalid-marker") {
		t.Fatal("body leaked")
	}
}
func TestWatchCapacityAndIsolation(t *testing.T) {
	limits := DefaultLimits()
	limits.Watches = 1
	s := serve(t, []Registration{widget}, limits, nil)
	_, list := request(t, s, "GET", collection, "")
	rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
	res, err := http.Get("http://" + s.Address() + collection + "?watch=true&resourceVersion=" + rv)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	code, _ := request(t, s, "GET", collection+"?watch=true&resourceVersion="+rv, "")
	if code != 503 {
		t.Fatal(code)
	}
	_, _ = io.Copy(io.Discard, res.Body)
}

// Regression for independent Verifier F1/F2: routing must not depend on an
// unrelated registration order, and round-tripping valid numbers must be exact.
func TestRegistrationOrderAndExactNumericContent(t *testing.T) {
	cluster := widget
	cluster.Scope = "Cluster"
	cluster.Kind = "Global"
	cluster.ListKind = "GlobalList"
	cluster.Plural = "globals"
	for _, regs := range [][]Registration{{cluster, widget}, {widget, cluster}} {
		s := serve(t, regs, DefaultLimits(), nil)
		body := strings.Replace(object("exact"), `"schema-invalid-marker"`, `9007199254740993`, 1)
		code, created := request(t, s, "POST", collection, body)
		if code != 201 {
			t.Fatal(code, created)
		}
		if created["spec"].(map[string]any)["number"].(json.Number).String() != "9007199254740993" {
			t.Fatal("CREATE corrupted numeric payload", created)
		}
		code, got := request(t, s, "GET", collection+"/exact", "")
		if code != 200 || got["spec"].(map[string]any)["number"].(json.Number).String() != "9007199254740993" {
			t.Fatal("GET corrupted stored number", code, got)
		}
	}
}

// A completed first watch must not satisfy readiness when the second opens later.
func TestConnectionRequiresSimultaneouslyActiveProfileWatches(t *testing.T) {
	other := widget
	other.Kind = "Other"
	other.ListKind = "OtherList"
	other.Plural = "others"
	s := serve(t, []Registration{widget, other}, DefaultLimits(), nil)
	open := func(path string) (*http.Response, context.CancelFunc) {
		t.Helper()
		_, list := request(t, s, "GET", path, "")
		rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+path+"?watch=true&resourceVersion="+rv, nil)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		return response, cancel
	}
	first, cancel := open(collection)
	cancel()
	first.Body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		count := len(s.subscriptions)
		s.mu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed watch remains")
		}
		time.Sleep(time.Millisecond)
	}
	second, cancelSecond := open("/apis/example.test/v1/namespaces/demo/others")
	defer cancelSecond()
	defer second.Body.Close()
	select {
	case <-s.Connected:
		t.Fatal("historical closed watch satisfied readiness")
	default:
	}
	replacement, cancelReplacement := open(collection)
	defer cancelReplacement()
	defer replacement.Body.Close()
	select {
	case <-s.Connected:
	case <-time.After(time.Second):
		t.Fatal("active profile did not connect")
	}
}

type failingFlush struct{ *httptest.ResponseRecorder }

func (w failingFlush) FlushError() error { return errors.New("injected flush failure") }
func TestFailedWatchFlushDoesNotEstablishConnection(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	_, list := request(t, s, "GET", collection, "")
	rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
	req := httptest.NewRequest("GET", collection+"?watch=true&resourceVersion="+rv, nil)
	s.ServeHTTP(failingFlush{httptest.NewRecorder()}, req)
	select {
	case <-s.Connected:
		t.Fatal("failed flush established connection")
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subscriptions) != 0 {
		t.Fatal("failed watch remained active")
	}
}
