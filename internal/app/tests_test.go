package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/scenario"
	"operator-dynamo/internal/workspace"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeclarativePreflightDoesNotStartListenersOrSessions(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "config"), 0700)
	os.MkdirAll(filepath.Join(root, "resources"), 0700)
	os.MkdirAll(filepath.Join(root, "tests"), 0700)
	os.WriteFile(filepath.Join(root, "config/operator.yaml"), []byte(configText), 0600)
	os.WriteFile(filepath.Join(root, "resources/example.yaml"), []byte(crdText), 0600)
	os.WriteFile(filepath.Join(root, "tests/suite.yaml"), []byte("apiVersion: dynamo/v1alpha1\nkind: Suite\nname: invalid\nentries: [first.yaml, missing.yaml]\n"), 0600)
	os.WriteFile(filepath.Join(root, "tests/first.yaml"), []byte("apiVersion: dynamo/v1alpha1\nkind: Scenario\nname: first\nsteps:\n- id: read\n  assert:\n    target: {apiVersion: example.test/v1, kind: Widget, namespace: default, name: x}\n    count: 0\n"), 0600)
	occupied, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer occupied.Close()
	err := New().Run(context.Background(), contracts.Options{Workdir: root, Tests: filepath.Join(root, "tests"), Listen: occupied.Addr().String()})
	var result *contracts.TestResult
	if !errors.As(err, &result) || result.Code != 2 {
		t.Fatal(err)
	}
	for _, dir := range []string{"sessions", "runs"} {
		entries, e := os.ReadDir(filepath.Join(root, dir))
		if e != nil || len(entries) != 0 {
			t.Fatal(dir, entries, e)
		}
	}
}
func TestCancelledTestPreflightExitOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New().Run(ctx, contracts.Options{Workdir: t.TempDir(), Tests: "missing", Listen: "127.0.0.1:0"})
	var result *contracts.TestResult
	if !errors.As(err, &result) || result.Code != 1 {
		t.Fatal(err)
	}
}

type failingUserHTTP struct {
	failed        chan error
	starts, stops int
	ready         atomic.Bool
}

func (h *failingUserHTTP) Start(context.Context, contracts.Session) error {
	h.starts++
	h.failed <- errors.New("synthetic listener failure")
	return nil
}
func (h *failingUserHTTP) Stop(ctx context.Context) error {
	h.stops++
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded stop")
	}
	return nil
}
func (h *failingUserHTTP) SetReady(v bool)        { h.ready.Store(v) }
func (h *failingUserHTTP) Failures() <-chan error { return h.failed }
func TestApplicationHTTPFailureAbortsBeforeRuntime(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "config"), 0700)
	os.MkdirAll(filepath.Join(root, "resources"), 0700)
	os.WriteFile(filepath.Join(root, "config/operator.yaml"), []byte(configText), 0600)
	os.WriteFile(filepath.Join(root, "resources/example.yaml"), []byte(crdText), 0600)
	os.WriteFile(filepath.Join(root, "scenario.yaml"), []byte("apiVersion: dynamo/v1alpha1\nkind: Scenario\nname: empty\nsteps:\n- id: absence\n  assert:\n    target: {apiVersion: example.test/v1, kind: Widget, namespace: default, name: x}\n    count: 0\n"), 0600)
	h := &failingUserHTTP{failed: make(chan error, 1)}
	app := New()
	app.UserHTTP = func(string) UserHTTP { return h }
	err := app.Run(context.Background(), contracts.Options{Workdir: root, Tests: filepath.Join(root, "scenario.yaml"), Listen: "127.0.0.1:0"})
	var result *contracts.TestResult
	if !errors.As(err, &result) || result.Code != 3 || result.Reason != "http_failed" || h.starts != 1 || h.stops != 1 || h.ready.Load() {
		t.Fatal(result, h)
	}
}

type closeFailureWorkspace struct {
	*workspace.Workspace
	calls int
}

func (w *closeFailureWorkspace) Close() error {
	w.calls++
	if w.calls == 1 {
		return errors.New("injected final workspace close failure")
	}
	return w.Workspace.Close()
}
func TestFinalWorkspaceCloseFailureCannotPublishPass(t *testing.T) {
	root := t.TempDir()
	base, e := workspace.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer base.Close()
	if e = base.NewRun("run"); e != nil {
		t.Fatal(e)
	}
	report := scenario.NewReport(&scenario.Plan{Scenarios: []scenario.Scenario{{Name: "completed", Steps: []scenario.Step{{ID: "passed", Operation: "assert"}}}}}, "run")
	report.Scenarios[0].Status = "passed"
	report.Scenarios[0].Steps[0].Outcome = "passed"
	report.Scenarios[0].Cleanup.Outcome = "passed"
	if e = base.WriteRun(context.Background(), report); e != nil {
		t.Fatal(e)
	}
	w := &closeFailureWorkspace{Workspace: base}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out := finalizeTestRun(ctx, w, report, scenario.Outcome{Status: "passed"}, time.Now())
	if out.Code != 3 || out.Reason != "cleanup_failed" {
		t.Fatal(out)
	}
	b, e := os.ReadFile(filepath.Join(root, "runs/run/report.json"))
	if e != nil {
		t.Fatal(e)
	}
	var final scenario.Report
	if e = json.Unmarshal(b, &final); e != nil || final.Status != "error" || final.ExitCode == nil || *final.ExitCode != 3 || final.ReasonCode != "cleanup_failed" {
		t.Fatal(string(b), e)
	}
	other, e := workspace.Open(root)
	if e != nil {
		t.Fatal("run lock not released", e)
	}
	other.Close()
}

type outputFailureWorkspace struct {
	*workspace.Workspace
	writeFailure bool
}
type failedRunWriter struct {
	workspace.RunWriter
	writeFailure bool
}

func (w *outputFailureWorkspace) TakeRunOutput() (workspace.RunWriter, error) {
	output, e := w.Workspace.TakeRunOutput()
	if e != nil {
		return nil, e
	}
	return &failedRunWriter{RunWriter: output, writeFailure: w.writeFailure}, nil
}
func (w *failedRunWriter) Write(c context.Context, v any) error {
	if w.writeFailure {
		return errors.New("injected report publication failure")
	}
	return w.RunWriter.Write(c, v)
}
func (w *failedRunWriter) Close() error {
	return errors.Join(w.RunWriter.Close(), errors.New("injected output release failure"))
}
func TestOutputPublicationAndReleaseFailureNeverReturnPass(t *testing.T) {
	for _, writeFailure := range []bool{true, false} {
		root := t.TempDir()
		base, e := workspace.Open(root)
		if e != nil {
			t.Fatal(e)
		}
		defer base.Close()
		base.NewRun("run")
		report := scenario.NewReport(&scenario.Plan{Scenarios: []scenario.Scenario{{Name: "finished", Steps: []scenario.Step{{ID: "read", Operation: "assert"}}}}}, "run")
		base.WriteRun(context.Background(), report)
		w := &outputFailureWorkspace{Workspace: base, writeFailure: writeFailure}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out := finalizeTestRun(ctx, w, report, scenario.Outcome{Status: "passed"}, time.Now())
		cancel()
		if out.Code != 3 || out.Status != "error" || out.Reason != "report_failed" {
			t.Fatal(out)
		}
		other, e := workspace.Open(root)
		if e != nil {
			t.Fatal(e)
		}
		other.Close()
		if writeFailure {
			b, _ := os.ReadFile(filepath.Join(root, "runs/run/report.json"))
			var prior scenario.Report
			json.Unmarshal(b, &prior)
			if prior.Status != "running" {
				t.Fatal("failed publication overwrote running report")
			}
		}
	}
}
