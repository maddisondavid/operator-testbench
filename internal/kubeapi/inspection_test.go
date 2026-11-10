package kubeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestInspectionHasNoTrafficOrReadinessEffects(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	code, _ := request(t, s, "POST", collection, `{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"precise"},"spec":{"number":9007199254740993}}`)
	if code != 201 {
		t.Fatal(code)
	}
	s.mu.Lock()
	version := s.version
	history := len(s.history)
	s.mu.Unlock()
	for i := 0; i < 9000; i++ {
		v, e := s.Inspect(context.Background(), widget.APIVersion(), widget.Kind, "demo", "")
		if e != nil || len(v.Items) != 1 {
			t.Fatal(v, e)
		}
	}
	v, e := s.Inspect(context.Background(), widget.APIVersion(), widget.Kind, "demo", "precise")
	if e != nil || !strings.Contains(v.ObjectJSON, "9007199254740993") {
		t.Fatal(v, e)
	}
	s.mu.Lock()
	if s.version != version || len(s.history) != history || len(s.listed) != 0 {
		t.Fatal("inspection changed API/readiness state")
	}
	s.mu.Unlock()
	code, _ = request(t, s, "GET", collection+"/precise", "")
	if code != 200 {
		t.Fatal("inspection consumed public budget", code)
	}
}

func TestInspectionPressureAtResourceCapacity(t *testing.T) {
	if testing.Short() {
		t.Skip("near-limit pressure")
	}
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	payload := strings.Repeat("x", 950000)
	for i := 0; i < 256; i++ {
		body := fmt.Sprintf(`{"apiVersion":"example.test/v1","kind":"Widget","metadata":{"name":"object-%03d"},"spec":{"payload":%q}}`, i, payload)
		code, _ := request(t, s, "POST", collection, body)
		if code != 201 {
			t.Fatal(i, code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				v, e := s.Inspect(ctx, widget.APIVersion(), widget.Kind, "demo", "")
				if e == nil && len(v.Items) != 256 {
					t.Error("incoherent count")
					return
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		_, obj := request(t, s, "GET", collection+"/object-000", "")
		obj["spec"] = map[string]any{"iteration": i}
		b, _ := json.Marshal(obj)
		code, _ := request(t, s, "PUT", collection+"/object-000", string(b))
		if code != 200 {
			t.Fatal(code)
		}
	}
	cancel()
	wg.Wait()
}
