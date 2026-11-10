package kubeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInitialSnapshotThenLive(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	s.InitialWatchReadiness = true
	s.RequireAllNamespaces = true
	path := "/api/v1/namespaces/demo/configmaps"
	for _, name := range []string{"b", "a"} {
		if c, o := request(t, s, "POST", path, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"`+name+`"}}`); c != 201 {
			t.Fatal(c, o)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+"/api/v1/configmaps?watch=true&timeoutSeconds=1", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res)
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for _, name := range []string{"a", "b"} {
		var e struct {
			Type   string
			Object struct{ Metadata struct{ Name string } }
		}
		if err := dec.Decode(&e); err != nil || e.Type != "ADDED" || e.Object.Metadata.Name != name {
			t.Fatal(e, err)
		}
	}
	select {
	case <-s.Connected:
	case <-time.After(time.Second):
		t.Fatal("initial stream not ready")
	}
	if c, o := request(t, s, "POST", path, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}`); c != 201 {
		t.Fatal(c, o)
	}
	var e struct {
		Type   string
		Object struct{ Metadata struct{ Name string } }
	}
	if err := dec.Decode(&e); err != nil || e.Type != "ADDED" || e.Object.Metadata.Name != "c" {
		t.Fatal(e, err)
	}
	if err := dec.Decode(&e); err != io.EOF {
		t.Fatal("timeout", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) != 3 || s.version != 4 {
		t.Fatal("snapshot fabricated history")
	}
}
func TestJSONPatchAtomicAndSelectors(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	root := "/api/v1/namespaces/demo/configmaps"
	c, before := request(t, s, "POST", root, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"one"},"data":{"one":"old","a/b":"escaped"},"binaryData":{"one":"AQ=="},"exact":9007199254740993,"array":["a","b"]}`)
	if c != 201 {
		t.Fatal(c, before)
	}
	patch := func(body string) (int, map[string]any) {
		return wireRequest(t, s, "PATCH", root+"/one", jsonPatchMedia, "", []byte(body))
	}
	for _, body := range []string{
		`[ {"op":"replace","path":"/data/one","value":"bad"},{"op":"remove","path":"/missing"}]`,
		`[{"op":"replace","path":"/metadata/resourceVersion","value":null}]`,
		`[{"op":"remove","path":"/metadata/resourceVersion"}]`,
		`[{"op":"replace","path":"/data/missing","value":"bad"}]`,
		`[{"op":"test","path":"/exact","value":9007199254740992}]`,
		`[{"op":"move","from":"/data","path":"/data/inside"}]`,
	} {
		if c, o := patch(body); c != 422 {
			t.Fatal(body, c, o)
		}
		_, after := request(t, s, "GET", root+"/one", "")
		if !equalJSON(before, after) {
			t.Fatal("partial mutation")
		}
	}
	if c, o := patch(`[{"op":"add","path":"/metadata/resourceVersion","value":"stale"}]`); c != 409 {
		t.Fatal(c, o)
	}
	c, o := patch(`[{"op":"test","path":"/exact","value":9007199254740993.0},{"op":"copy","from":"/data/a~1b","path":"/data/copy"},{"op":"move","from":"/array/0","path":"/array/-"},{"op":"replace","path":"/data","value":{"changed":"yes"}},{"op":"replace","path":"/binaryData","value":{"changed":"Ag=="}}]`)
	if c != 200 || encoded(o["array"]) != `["b","a"]` || o["exact"] != json.Number("9007199254740993") {
		t.Fatal(c, o)
	}
	c, list := request(t, s, "GET", root+"?fieldSelector=metadata.name=one", "")
	if c != 200 || len(list["items"].([]any)) != 1 {
		t.Fatal(c, list)
	}
	c, list = request(t, s, "GET", root+"?fieldSelector=metadata.name=absent", "")
	if c != 200 || len(list["items"].([]any)) != 0 {
		t.Fatal(c, list)
	}
	for _, query := range []string{"fieldSelector=metadata.name=one&watch=true", "fieldSelector=metadata.name=one,metadata.name=two", "fieldSelector=metadata.name=one&fieldSelector=metadata.name=one"} {
		if c, o := request(t, s, "GET", root+"?"+query, ""); c != 400 {
			t.Fatal(c, o)
		}
	}
}
func TestSecretPrivacyAndSemantics(t *testing.T) {
	reg, _ := Builtin("v1/secrets")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	path := "/api/v1/namespaces/demo/secrets"
	c, o := request(t, s, "POST", path, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"secret","annotations":{"secret":"PRIVATE_CANARY"},"labels":{"secret":"PRIVATE_CANARY"}},"data":{"key":"UFJJVkFURV9DQU5BUlk="}}`)
	if c != 201 || o["type"] != "Opaque" {
		t.Fatal(c, o)
	}
	view, err := s.Inspect(context.Background(), "v1", "Secret", "demo", "")
	if err != nil || strings.Contains(encoded(view), "PRIVATE") || strings.Contains(encoded(view), "UFJJV") || len(view.Items) != 1 {
		t.Fatal(view, err)
	}
	_, err = s.Inspect(context.Background(), "v1", "Secret", "demo", "secret")
	if err == nil || err.Error() != "secret_inspection_disabled" {
		t.Fatal(err)
	}
	for _, field := range []string{`"stringData":{"key":"PRIVATE_CANARY"}`, `"data":{"key":"invalid-base64"}`, `"type":"kubernetes.io/tls"`, `"immutable":true`, `"status":{}`} {
		c, o := request(t, s, "POST", path, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"bad"},`+field+`}`)
		if c != 422 || strings.Contains(encoded(o), "PRIVATE") {
			t.Fatal(c, o)
		}
	}
}

type snapshotBarrier struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	flushed          chan int
	count            int
}

func (w *snapshotBarrier) FlushError() error {
	w.count++
	if w.count == 1 {
		close(w.entered)
		<-w.release
	}
	w.ResponseRecorder.Flush()
	w.flushed <- w.count
	return nil
}
func TestInitialSnapshotDeleteRecreateBoundary(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	s.InitialWatchReadiness = true
	s.RequireAllNamespaces = true
	root := "/api/v1/namespaces/demo/configmaps"
	body := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"one"}}`
	_, first := request(t, s, "POST", root, body)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/v1/configmaps?watch=true", nil).WithContext(ctx)
	w := &snapshotBarrier{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{}), flushed: make(chan int, 8)}
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, req) }()
	<-w.entered
	select {
	case <-s.Connected:
		t.Fatal("ready before initial flush")
	default:
	}
	if c, o := request(t, s, "DELETE", root+"/one", `{}`); c != 200 {
		t.Fatal(c, o)
	}
	_, second := request(t, s, "POST", root, body)
	close(w.release)
	for i := 0; i < 4; i++ {
		select {
		case <-w.flushed:
		case <-time.After(time.Second):
			t.Fatal("event missing")
		}
	}
	cancel()
	<-done
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	for i, typ := range []string{"ADDED", "DELETED", "ADDED"} {
		var e struct {
			Type   string
			Object map[string]any
		}
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		expected := first
		if i == 2 {
			expected = second
		}
		if e.Type != typ || e.Object["metadata"].(map[string]any)["uid"] != expected["metadata"].(map[string]any)["uid"] {
			t.Fatal(e)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) != 3 || len(s.subscriptions) != 0 {
		t.Fatal("snapshot mutated history or leaked watch")
	}
}
func TestInitialWatchReadinessModes(t *testing.T) {
	for _, initialMode := range []bool{true, false} {
		for _, initialRequest := range []bool{true, false} {
			t.Run(fmt.Sprint(initialMode, initialRequest), func(t *testing.T) {
				reg, _ := Builtin("v1/configmaps")
				s := serve(t, []Registration{reg}, DefaultLimits(), nil)
				s.InitialWatchReadiness = initialMode
				s.RequireAllNamespaces = true
				_, list := request(t, s, "GET", "/api/v1/configmaps", "")
				query := "watch=true"
				if !initialRequest {
					query += "&resourceVersion=" + list["metadata"].(map[string]any)["resourceVersion"].(string)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+"/api/v1/configmaps?"+query, nil)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if initialMode == initialRequest {
					select {
					case <-s.Connected:
					case <-time.After(time.Second):
						t.Fatal("not connected")
					}
				} else {
					select {
					case <-s.Connected:
						t.Fatal("wrong watch mode connected")
					case <-time.After(20 * time.Millisecond):
					}
				}
			})
		}
	}
}

func TestInitialReadinessNeedsActiveGlobalSet(t *testing.T) {
	cm, _ := Builtin("v1/configmaps")
	ns, _ := Builtin("v1/namespaces")
	secret, _ := Builtin("v1/secrets")
	s := serve(t, []Registration{cm, ns, secret}, DefaultLimits(), nil)
	s.InitialWatchReadiness = true
	open := func(path string) (*http.Response, context.CancelFunc) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		r, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+path+"?watch=true", nil)
		res, err := http.DefaultClient.Do(r)
		if err != nil || res.StatusCode != 200 {
			cancel()
			t.Fatal(res, err)
		}
		return res, cancel
	}
	a, ca := open("/api/v1/configmaps")
	ca()
	a.Body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		n := len(s.subscriptions)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed subscription retained")
		}
		time.Sleep(time.Millisecond)
	}
	b, cb := open("/api/v1/namespaces")
	defer cb()
	defer b.Body.Close()
	c, cc := open("/api/v1/secrets")
	defer cc()
	defer c.Body.Close()
	scoped, cs := open("/api/v1/namespaces/demo/configmaps")
	defer cs()
	defer scoped.Body.Close()
	select {
	case <-s.Connected:
		t.Fatal("closed/scoped stream counted")
	default:
	}
	failed := httptest.NewRequest("GET", "/api/v1/configmaps?watch=true", nil)
	s.ServeHTTP(failingFlush{httptest.NewRecorder()}, failed)
	select {
	case <-s.Connected:
		t.Fatal("failed flush counted")
	default:
	}
	live, cl := open("/api/v1/configmaps")
	defer cl()
	defer live.Body.Close()
	select {
	case <-s.Connected:
	case <-time.After(time.Second):
		t.Fatal("complete global set not ready")
	}
}

func TestJSONPatchVersionsBoundsAndNoop(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	root := "/api/v1/namespaces/demo/configmaps"
	_, before := request(t, s, "POST", root, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"one"},"exact":9007199254740993,"data":{"a":"one"}}`)
	patch := func(v any) (int, map[string]any) {
		return wireRequest(t, s, "PATCH", root+"/one", jsonPatchMedia, "", []byte(encoded(v)))
	}
	op := func(verb, path string, v any) map[string]any {
		return map[string]any{"op": verb, "path": path, "value": v}
	}
	if c, o := patch([]any{op("replace", "/exact", json.Number("9007199254740993.0"))}); c != 200 || !equalJSON(before, o) {
		t.Fatal(c, o)
	}
	for _, v := range []any{nil, "", json.Number("1")} {
		m := copyObject(before)["metadata"].(map[string]any)
		m["resourceVersion"] = v
		if c, o := patch([]any{op("replace", "/metadata", m)}); c != 422 {
			t.Fatal(c, o)
		}
	}
	for _, path := range []string{"/metadata/uid", "/metadata/creationTimestamp"} {
		if c, o := patch([]any{map[string]any{"op": "remove", "path": path}}); c != 422 {
			t.Fatal(c, o)
		}
	}
	// An oversized post-image must fail even when each request is under the cap.
	if c, o := patch([]any{op("add", "/large", strings.Repeat("a", 600000))}); c != 200 {
		t.Fatal(c, o)
	}
	_, before = request(t, s, "GET", root+"/one", "")
	if c, o := patch([]any{map[string]any{"op": "copy", "from": "/large", "path": "/second"}}); c != 422 {
		t.Fatal(c, o)
	}
	_, after := request(t, s, "GET", root+"/one", "")
	if !equalJSON(before, after) {
		t.Fatal("oversize committed")
	}
	rv := before["metadata"].(map[string]any)["resourceVersion"]
	results := make(chan int, 2)
	for i := range 2 {
		go func() {
			c, _ := patch([]any{op("replace", "/metadata/resourceVersion", rv), op("replace", "/data/a", fmt.Sprint(i))})
			results <- c
		}()
	}
	a, b := <-results, <-results
	if !(a == 200 && b == 409 || a == 409 && b == 200) {
		t.Fatal(a, b)
	}
}

func TestInitialZeroDoesNotBecomeHistoryReplay(t *testing.T) {
	reg, _ := Builtin("v1/configmaps")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	s.InitialWatchReadiness = true
	path := "/api/v1/namespaces/demo/configmaps"
	_, o := request(t, s, "POST", path, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"one"},"data":{"value":"old"}}`)
	o["data"] = map[string]any{"value": "current"}
	if c, v := request(t, s, "PUT", path+"/one", encoded(o)); c != 200 {
		t.Fatal(c, v)
	}
	for _, rv := range []string{"00", "000", "%2B0", "-0"} {
		if c, o := request(t, s, "GET", "/api/v1/configmaps?watch=true&resourceVersion="+rv, ""); c != 400 {
			t.Fatal(rv, c, o)
		}
		s.mu.Lock()
		n := len(s.subscriptions)
		s.mu.Unlock()
		if n != 0 {
			t.Fatal("invalid zero subscribed")
		}
		select {
		case <-s.Connected:
			t.Fatal("invalid zero connected")
		default:
		}
	}
	res, err := http.Get("http://" + s.Address() + "/api/v1/configmaps?watch=true&resourceVersion=0&timeoutSeconds=1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	var e struct {
		Type   string
		Object map[string]any
	}
	if err := dec.Decode(&e); err != nil || e.Type != "ADDED" || e.Object["data"].(map[string]any)["value"] != "current" {
		t.Fatal(e, err)
	}
	if err := dec.Decode(&e); err != io.EOF {
		t.Fatal("zero replayed history", e, err)
	}
}
