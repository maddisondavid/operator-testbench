package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
	"time"
)

type scriptedGateway struct {
	calls   int
	request func(context.Context, string, Target, any) (Response, error)
}

func (g *scriptedGateway) Request(c context.Context, m string, t Target, v any) (Response, error) {
	g.calls++
	return g.request(c, m, t, v)
}
func (g *scriptedGateway) Close() {}

type scriptedSession struct {
	g         *scriptedGateway
	reason    string
	stopped   bool
	stopError error
	records   []contracts.LifecyclePayload
}

func (s *scriptedSession) ID() string       { return fmt.Sprintf("session-%p", s) } // overridden below to avoid formatting cycles
func (s *scriptedSession) Gateway() Gateway { return s.g }
func (s *scriptedSession) Failure() string  { return s.reason }
func (s *scriptedSession) Record(_ string, p contracts.LifecyclePayload) error {
	s.records = append(s.records, p)
	return nil
}
func (s *scriptedSession) Stop(context.Context) error { s.stopped = true; return s.stopError }

type scriptedFactory struct {
	sessions []*scriptedSession
	next     int
}

func (f *scriptedFactory) Start(context.Context, string) (Session, error) {
	if f.next > 0 && !f.sessions[f.next-1].stopped {
		panic("overlapping sessions")
	}
	s := f.sessions[f.next]
	f.next++
	return s, nil
}
func controls() Control {
	return Control{Publish: func(*Report) error { return nil }, Cleanup: func(bool) (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), time.Second)
	}}
}
func target() Target {
	return Target{APIVersion: "example.test/v1", Kind: "Widget", Namespace: "default", Name: "x", Resource: widgets[0]}
}
func TestFailedScenarioContinuesWithFreshSession(t *testing.T) {
	fail := &scriptedSession{g: &scriptedGateway{request: func(context.Context, string, Target, any) (Response, error) {
		return Response{Code: 404, RequestID: "first"}, nil
	}}}
	pass := &scriptedSession{g: &scriptedGateway{request: func(context.Context, string, Target, any) (Response, error) {
		return Response{Code: 404, RequestID: "second"}, nil
	}}}
	plan := &Plan{Scenarios: []Scenario{{Name: "fails", Timeout: time.Second, Steps: []Step{{ID: "missing", Operation: "assert", Assert: &Assertion{Target: target(), Count: 1, Timeout: 110 * time.Millisecond}}, {ID: "skipped", Operation: "create"}}}, {Name: "passes", Timeout: time.Second, Steps: []Step{{ID: "absent", Operation: "assert", Assert: &Assertion{Target: target(), Count: 0, Timeout: time.Second}}}}}}
	r := NewReport(plan, "run")
	f := &scriptedFactory{sessions: []*scriptedSession{fail, pass}}
	o := Run(context.Background(), plan, r, f, controls())
	if o.Code != 1 || f.next != 2 || r.Scenarios[0].Steps[1].Outcome != "skipped" || r.Scenarios[1].Status != "passed" || !fail.stopped || !pass.stopped {
		t.Fatal(o, r)
	}
}
func TestActionConflictNoRetryAndCleanupPrecedence(t *testing.T) {
	g := &scriptedGateway{}
	g.request = func(_ context.Context, m string, _ Target, v any) (Response, error) {
		if m == http.MethodGet {
			return Response{Code: 200, RequestID: "read", Object: map[string]any{"metadata": map[string]any{"uid": "old", "resourceVersion": "7", "creationTimestamp": "time"}}}, nil
		}
		meta := object(object(v)["metadata"])
		if meta["uid"] != "old" || meta["resourceVersion"] != "7" {
			t.Fatal(meta)
		}
		return Response{Code: 409, RequestID: "write"}, nil
	}
	s := &scriptedSession{g: g, stopError: errors.New("cleanup")}
	plan := &Plan{Scenarios: []Scenario{{Timeout: time.Second, Steps: []Step{{ID: "conflict", Operation: "update", Target: target(), Fixture: map[string]any{"metadata": map[string]any{"name": "x"}}}}}, {Timeout: time.Second}}}
	r := NewReport(plan, "run")
	o := Run(context.Background(), plan, r, &scriptedFactory{sessions: []*scriptedSession{s}}, controls())
	if o.Code != 3 || g.calls != 2 || r.Scenarios[0].Steps[0].ReasonCode != "api_rejected" || r.Scenarios[0].Cleanup.Outcome != "error" || r.Scenarios[1].Status != "skipped" {
		t.Fatal(o, r)
	}
}
func TestLateMatchAndQueuedFailureCannotPass(t *testing.T) {
	for _, late := range []bool{false, true} {
		s := &scriptedSession{}
		s.g = &scriptedGateway{request: func(c context.Context, _ string, _ Target, _ any) (Response, error) {
			if late {
				<-c.Done()
			} else {
				s.reason = "journal_failed"
			}
			return Response{Code: 404, RequestID: "id"}, nil
		}}
		plan := &Plan{Scenarios: []Scenario{{Timeout: time.Second, Steps: []Step{{ID: "absence", Operation: "assert", Assert: &Assertion{Target: target(), Count: 0, Timeout: 10 * time.Millisecond}}}}}}
		r := NewReport(plan, "run")
		o := Run(context.Background(), plan, r, &scriptedFactory{sessions: []*scriptedSession{s}}, controls())
		expected := 3
		if late {
			expected = 1
		}
		if o.Code != expected {
			t.Fatal(o)
		}
	}
}
func TestSavedObservationDoesNotRefreshAndMissingReferenceFails(t *testing.T) {
	g := &scriptedGateway{}
	g.request = func(context.Context, string, Target, any) (Response, error) {
		return Response{Code: 200, RequestID: "id", Object: map[string]any{"metadata": map[string]any{"uid": "original"}, "spec": map[string]any{"number": json.Number("9007199254740995")}}}, nil
	}
	s := &scriptedSession{g: g}
	steps := []Step{{ID: "save", Operation: "assert", Assert: &Assertion{Target: target(), Count: 1, SaveAs: "saved", Timeout: time.Second}}, {ID: "reference", Operation: "assert", Assert: &Assertion{Target: target(), Count: 1, Fields: []Field{{Pointer: "/metadata/uid", EqualsFrom: &Reference{Observation: "saved", Pointer: "/missing"}}}, Timeout: time.Second}}}
	plan := &Plan{Scenarios: []Scenario{{Timeout: time.Second, Steps: steps}}}
	r := NewReport(plan, "run")
	o := Run(context.Background(), plan, r, &scriptedFactory{sessions: []*scriptedSession{s}}, controls())
	if o.Code != 1 || o.Reason != "reference_missing" || g.calls != 1 {
		t.Fatal(o, g.calls)
	}
}

func TestCancellationDuringOrdinaryCleanupUsesTerminalReportContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &scriptedSession{g: &scriptedGateway{request: func(context.Context, string, Target, any) (Response, error) {
		return Response{Code: 404, RequestID: "id"}, nil
	}}}
	plan := &Plan{Scenarios: []Scenario{{Timeout: time.Second, Steps: []Step{{ID: "read", Operation: "assert", Assert: &Assertion{Target: target(), Count: 0, Timeout: time.Second}}}}, {Timeout: time.Second, Steps: []Step{{ID: "later", Operation: "create"}}}}}
	r := NewReport(plan, "run")
	control := controls()
	control.Cancelled = func() bool { return parent.Err() != nil }
	control.Cleanup = func(terminal bool) (context.Context, context.CancelFunc) {
		cancel()
		return context.WithTimeout(context.Background(), time.Second)
	}
	o := Run(parent, plan, r, &scriptedFactory{sessions: []*scriptedSession{s}}, control)
	if o.Status != "cancelled" || o.Code != 1 || r.Scenarios[0].Status != "cancelled" || r.Scenarios[1].Status != "skipped" {
		t.Fatal(o, r)
	}
}

func TestDiagnosticReportsAndEventsExcludeValues(t *testing.T) {
	const secret = "fixture-assertion-private-canary"
	fixture := map[string]any{"metadata": map[string]any{"name": "x"}, "spec": map[string]any{"private": secret}}
	s := &scriptedSession{g: &scriptedGateway{request: func(context.Context, string, Target, any) (Response, error) {
		return Response{Code: 422, RequestID: "request", Object: map[string]any{"message": secret}}, nil
	}}}
	plan := &Plan{Scenarios: []Scenario{{Name: "safe", SourcePath: "tests/scenario.yaml", Timeout: time.Second, Steps: []Step{{ID: "create", Operation: "create", Target: target(), Fixture: fixture}}}}}
	report := NewReport(plan, "run")
	out := Run(context.Background(), plan, report, &scriptedFactory{sessions: []*scriptedSession{s}}, controls())
	if out.Code != 1 {
		t.Fatal(out)
	}
	for _, v := range []any{report, s.records} {
		b, e := json.Marshal(v)
		if e != nil || bytes.Contains(b, []byte(secret)) {
			t.Fatal("payload in diagnostics", e)
		}
	}
	_, e := parse([]byte("secret: ["+secret), "tests/invalid.yaml")
	if e == nil || strings.Contains(e.Error(), secret) {
		t.Fatal("parser leaked offending value", e)
	}
}

func TestProgressFollowsStepAndCleanupOutcomes(t *testing.T) {
	for _, cleanupFailure := range []bool{false, true} {
		var trace []contracts.ProgressKind
		control := controls()
		control.Progress = func(p contracts.Progress) { trace = append(trace, p.Kind) }
		s := &scriptedSession{g: &scriptedGateway{request: func(context.Context, string, Target, any) (Response, error) {
			if len(trace) == 0 || trace[len(trace)-1] != contracts.Asserting {
				t.Fatal("start not before request", trace)
			}
			return Response{Code: 404, RequestID: "id"}, nil
		}}}
		if cleanupFailure {
			s.stopError = errors.New("private cleanup detail")
		}
		plan := &Plan{Scenarios: []Scenario{{Name: "same", Timeout: time.Second, Steps: []Step{{ID: "absence", Operation: "assert", Assert: &Assertion{Target: target(), Count: 0, Timeout: time.Second}}}}}}
		Run(context.Background(), plan, NewReport(plan, "run"), &scriptedFactory{sessions: []*scriptedSession{s}}, control)
		last := contracts.CleanupComplete
		if cleanupFailure {
			last = contracts.CleanupFailed
		}
		want := []contracts.ProgressKind{contracts.ScenarioRunning, contracts.Asserting, contracts.StepPassed, contracts.CleanupStarting, last}
		if fmt.Sprint(trace) != fmt.Sprint(want) {
			t.Fatal(trace, want)
		}
	}
}

func TestAPIFailureDoesNotMislabelSuccessfulCleanup(t *testing.T) {
	s := &scriptedSession{reason: "api_failed"}
	plan := &Plan{Scenarios: []Scenario{{Name: "failed", Timeout: time.Second, Steps: []Step{{ID: "unused", Operation: "assert", Assert: &Assertion{Target: target(), Count: 0, Timeout: time.Second}}}}}}
	report := NewReport(plan, "run")
	control := controls()
	var trace []contracts.ProgressKind
	control.Progress = func(p contracts.Progress) { trace = append(trace, p.Kind) }
	result := Run(context.Background(), plan, report, &scriptedFactory{sessions: []*scriptedSession{s}}, control)
	if result.Code != 3 || report.Scenarios[0].Cleanup.Outcome != "passed" {
		t.Fatal(result, report)
	}
	complete := false
	for _, kind := range trace {
		if kind == contracts.CleanupFailed {
			t.Fatal(trace)
		}
		if kind == contracts.CleanupComplete {
			complete = true
		}
	}
	if !complete {
		t.Fatal(trace)
	}
}
