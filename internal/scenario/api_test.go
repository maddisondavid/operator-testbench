package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/kubeapi"
	"testing"
	"time"
)

func TestActionsUsePublicAPIExactReplacementAndDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := kubeapi.New("127.0.0.1:0", []kubeapi.Registration{{Group: "example.test", Version: "v1", Kind: "Widget", ListKind: "WidgetList", Plural: "widgets", Scope: "Namespaced", Status: true}}, nil)
	if e := api.Start(ctx, contracts.Session{ID: "api-contract"}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, d := context.WithTimeout(context.Background(), time.Second)
		defer d()
		if e := api.Stop(c); e != nil {
			t.Error(e)
		}
	})
	gateway := NewHTTPGateway(api.Address())
	defer gateway.Close()
	g := &scriptedGateway{request: gateway.Request}
	s := &scriptedSession{g: g}
	fixture := map[string]any{"apiVersion": "example.test/v1", "kind": "Widget", "metadata": map[string]any{"name": "x", "namespace": "default", "labels": map[string]any{"remove": "yes"}}, "spec": map[string]any{"number": json.Number("9007199254740995")}}
	replacement := copyObject(fixture)
	delete(object(replacement["metadata"]), "labels")
	object(replacement["spec"])["number"] = json.Number("9007199254740996")
	plan := &Plan{Scenarios: []Scenario{{Timeout: time.Second, Steps: []Step{
		{ID: "create", Operation: "create", Target: target(), Fixture: fixture},
		{ID: "observe", Operation: "assert", Assert: &Assertion{Target: target(), Count: 1, Timeout: time.Second, SaveAs: "before", Fields: []Field{{Pointer: "/spec/number", Equals: json.Number("9007199254740995"), EqualsSet: true}}}},
		{ID: "update", Operation: "update", Target: target(), Fixture: replacement},
		{ID: "replacement", Operation: "assert", Assert: &Assertion{Target: target(), Count: 1, Timeout: time.Second, Fields: []Field{{Pointer: "/metadata/labels", Exists: boolPointer(false)}, {Pointer: "/spec/number", Equals: json.Number("9007199254740996"), EqualsSet: true}, {Pointer: "/metadata/uid", EqualsFrom: &Reference{Observation: "before", Pointer: "/metadata/uid"}}}}},
		{ID: "delete", Operation: "delete", Target: target()},
		{ID: "absence", Operation: "assert", Assert: &Assertion{Target: target(), Count: 0, Timeout: time.Second}},
	}}}}
	r := NewReport(plan, "run")
	out := Run(ctx, plan, r, &scriptedFactory{sessions: []*scriptedSession{s}}, controls())
	if out.Code != 0 {
		t.Fatal(out, r)
	}
	if len(r.Scenarios[0].Steps[2].RequestIDs) != 2 || len(r.Scenarios[0].Steps[4].RequestIDs) != 2 {
		t.Fatal("read/write correlations missing")
	}
}
func boolPointer(v bool) *bool { return &v }
func TestAPIRejectedCreateDoesNotRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := kubeapi.New("127.0.0.1:0", []kubeapi.Registration{{Group: "example.test", Version: "v1", Kind: "Widget", ListKind: "WidgetList", Plural: "widgets", Scope: "Namespaced"}}, nil)
	if e := api.Start(ctx, contracts.Session{ID: "negative"}); e != nil {
		t.Fatal(e)
	}
	defer api.Stop(context.Background())
	gateway := NewHTTPGateway(api.Address())
	defer gateway.Close()
	g := &scriptedGateway{request: gateway.Request}
	s := &scriptedSession{g: g}
	fixture := map[string]any{"apiVersion": "example.test/v1", "kind": "Widget", "metadata": map[string]any{"name": "x", "namespace": "default", "uid": "forbidden"}}
	plan := &Plan{Scenarios: []Scenario{{Timeout: time.Second, Steps: []Step{{ID: "reject", Operation: "create", Target: target(), Fixture: fixture}}}}}
	r := NewReport(plan, "run")
	out := Run(ctx, plan, r, &scriptedFactory{sessions: []*scriptedSession{s}}, controls())
	if out.Code != 1 || out.Reason != "api_rejected" || g.calls != 1 {
		t.Fatal(out, g.calls)
	}
	response, e := gateway.Request(ctx, http.MethodGet, target(), nil)
	if e != nil || response.Code != 404 {
		t.Fatal(response, e)
	}
}
func TestReportFailureAbortsBeforeSession(t *testing.T) {
	plan := &Plan{Scenarios: []Scenario{{Steps: []Step{{ID: "never", Operation: "create"}}}}}
	r := NewReport(plan, "run")
	control := controls()
	control.Publish = func(*Report) error { return errors.New("synthetic report I/O") }
	factory := &scriptedFactory{}
	out := Run(context.Background(), plan, r, factory, control)
	if out.Code != 3 || factory.next != 0 || r.Scenarios[0].Steps[0].Outcome != "skipped" {
		t.Fatal(out, r)
	}
}
