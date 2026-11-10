package operatorruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"operator-dynamo/internal/contracts"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeEngine struct {
	mu      sync.Mutex
	calls   []string
	fail    string
	exit    bool
	created map[string]any
	objects map[string]map[string]any
}

func (f *fakeEngine) CheckPaths(context.Context, string, []string) error { return nil }
func (f *fakeEngine) Call(ctx context.Context, m, p string, b io.Reader, out any) error {
	f.mu.Lock()
	f.calls = append(f.calls, m+" "+p)
	fail := f.fail
	if strings.Contains(p, "/wait?") {
		exit := f.exit
		f.mu.Unlock()
		if !exit {
			<-ctx.Done()
			return ctx.Err()
		}
		return json.Unmarshal([]byte(`{"StatusCode":0}`), out)
	}
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = map[string]map[string]any{}
	}
	respond := func(v any) error {
		if out == nil {
			return nil
		}
		data, _ := json.Marshal(v)
		return json.Unmarshal(data, out)
	}
	if strings.HasPrefix(p, "/containers/create") {
		var body map[string]any
		json.NewDecoder(b).Decode(&body)
		f.created = body
		u, _ := url.Parse(p)
		name := u.Query().Get("name")
		if _, exists := f.objects["/containers/"+name+"/json"]; exists {
			return &HTTPError{Method: m, Status: 409}
		}
		f.objects["/containers/"+name+"/json"] = map[string]any{"Config": body, "State": map[string]bool{"Running": true}}
		if fail != "" && strings.Contains(p, fail) {
			return errors.New("injected lost create response")
		}
		return respond(map[string]string{"Id": name})
	}
	if fail != "" && strings.Contains(p, fail) {
		return errors.New("injected")
	}
	if strings.HasPrefix(p, "/images/") {
		return respond(map[string]string{"Id": "sha256:immutable"})
	}
	if p == "/volumes/create" {
		var body map[string]any
		json.NewDecoder(b).Decode(&body)
		if existing, ok := f.objects["/volumes/"+body["Name"].(string)]; ok {
			return respond(existing)
		}
		f.objects["/volumes/"+body["Name"].(string)] = body
		return respond(body)
	}
	if m == "GET" {
		obj, ok := f.objects[p]
		if !ok {
			return &HTTPError{Method: m, Status: 404}
		}
		return respond(obj)
	}
	if m == "DELETE" {
		base := strings.Split(p, "?")[0]
		delete(f.objects, base)
		delete(f.objects, base+"/json")
	}
	return nil
}
func TestRuntimeFailuresAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, fail string
		exit       bool
	}{{"missing", "/images/", false}, {"create", "/containers/create", false}, {"copy", "/archive?", false}, {"start", "/start", false}, {"timeout", "", false}, {"exit", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEngine{fail: tc.fail, exit: tc.exit}
			r := New(Config{Image: "local"}, "http://example:18081", 20*time.Millisecond, make(chan struct{}), nil)
			r.Engine = f
			if err := r.Start(context.Background(), contracts.Session{ID: "test"}); err == nil {
				t.Fatal("startup should fail")
			}
			ctx, c := context.WithTimeout(context.Background(), time.Second)
			defer c()
			if err := r.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			for _, call := range f.calls {
				if strings.Contains(call, "images/create") {
					t.Fatal("pulled image")
				}
			}
			if tc.name != "missing" && !strings.Contains(strings.Join(f.calls, "\n"), "DELETE /containers/") {
				t.Fatal("owned container not removed")
			}
		})
	}
}
func TestLaunchPreservesDefaultsAndReportsCleanup(t *testing.T) {
	connected := make(chan struct{})
	close(connected)
	f := &fakeEngine{}
	r := New(Config{Image: "local", Env: map[string]string{"SECRET": "seed"}}, "http://example:18081", time.Second, connected, nil)
	r.Engine = f
	if err := r.Start(context.Background(), contracts.Session{ID: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.created["Cmd"]; ok {
		t.Fatal("image args overwritten")
	}
	if _, ok := f.created["Entrypoint"]; ok {
		t.Fatal("entrypoint overwritten")
	}
	if f.created["Image"] != "sha256:immutable" {
		t.Fatal("mutable image used")
	}
	f.mu.Lock()
	f.fail = "?force="
	f.mu.Unlock()
	err := r.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatal("cleanup failure not reported", err)
	}
}

func TestForeignHandleCollisionIsNeverRemoved(t *testing.T) {
	foreign := map[string]any{"Config": map[string]any{"Labels": map[string]string{"io.dynamo.session": "test", "io.dynamo.owner": "someone-else"}}, "State": map[string]bool{"Running": true}}
	f := &fakeEngine{objects: map[string]map[string]any{"/containers/dynamo-prepare-test/json": foreign}}
	r := New(Config{Image: "local"}, "http://example:18081", time.Second, make(chan struct{}), nil)
	r.Engine = f
	if e := r.Start(context.Background(), contracts.Session{ID: "test"}); e == nil {
		t.Fatal("foreign collision accepted")
	}
	if e := r.Stop(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, ok := f.objects["/containers/dynamo-prepare-test/json"]; !ok {
		t.Fatal("deleted foreign handle")
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "DELETE /containers/dynamo-prepare-test") {
			t.Fatal(call)
		}
	}
}

func TestExistingImageProgressDoesNotInventBuildOrConnection(t *testing.T) {
	for _, fail := range []string{"", "/start"} {
		connected := make(chan struct{})
		close(connected)
		r := New(Config{Image: "local"}, "http://example:18081", time.Second, connected, nil)
		r.Engine = &fakeEngine{fail: fail}
		var trace []contracts.ProgressKind
		r.Progress = func(p contracts.Progress) { trace = append(trace, p.Kind) }
		e := r.Start(context.Background(), contracts.Session{ID: "progress"})
		if (e != nil) != (fail != "") {
			t.Fatal(e)
		}
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		r.Stop(c)
		cancel()
		want := []contracts.ProgressKind{contracts.Preparing, contracts.ContainerStarting}
		if fail == "" {
			want = append(want, contracts.ContainerStarted, contracts.ConnectionWaiting, contracts.OperatorConnected)
		}
		if len(trace) != len(want) {
			t.Fatal(trace, want)
		}
		for i := range want {
			if trace[i] != want[i] {
				t.Fatal(trace, want)
			}
		}
	}
}
