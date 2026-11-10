package app

import (
	"context"
	"encoding/json"
	"errors"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/reportview"
	"operator-dynamo/internal/scenario"
	"operator-dynamo/internal/workspace"
	"os"
	"path/filepath"
	"testing"
)

type verifierCloseWorkspace struct{ *workspace.Workspace }

func (w *verifierCloseWorkspace) Close() error {
	return errors.Join(w.Workspace.Close(), errors.New("synthetic close failure"))
}

type verifierCancelledHTTP struct {
	cancel   context.CancelFunc
	failures chan error
}

func (h *verifierCancelledHTTP) Start(context.Context, contracts.Session) error {
	h.cancel()
	return context.Canceled
}
func (*verifierCancelledHTTP) Stop(context.Context) error { return nil }
func (*verifierCancelledHTTP) SetReady(bool)              {}
func (h *verifierCancelledHTTP) Failures() <-chan error   { return h.failures }
func TestVerifierFinalCloseReportConsistency(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"config", "resources"} {
		os.MkdirAll(filepath.Join(root, dir), 0700)
	}
	os.WriteFile(filepath.Join(root, "config/operator.yaml"), []byte(configText), 0600)
	os.WriteFile(filepath.Join(root, "resources/example.yaml"), []byte(crdText), 0600)
	os.WriteFile(filepath.Join(root, "scenario.yaml"), []byte("apiVersion: dynamo/v1alpha1\nkind: Scenario\nname: test\nsteps:\n- id: absent\n  assert:\n    target: {apiVersion: example.test/v1, kind: Widget, namespace: default, name: x}\n    count: 0\n"), 0600)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := New()
	a.OpenWorkspace = func(path string) (Workspace, error) {
		w, e := workspace.Open(path)
		if e != nil {
			return nil, e
		}
		return &verifierCloseWorkspace{w}, nil
	}
	a.UserHTTP = func(string) UserHTTP { return &verifierCancelledHTTP{cancel: cancel, failures: make(chan error)} }
	err := a.Run(parent, contracts.Options{Workdir: root, Tests: filepath.Join(root, "scenario.yaml"), Listen: "127.0.0.1:0"})
	var result *contracts.TestResult
	if !errors.As(err, &result) || result.Code != 3 {
		t.Fatalf("expected cleanup error: %v", err)
	}
	data, e := os.ReadFile(filepath.Join(root, result.ReportPath))
	if e != nil {
		t.Fatal(e)
	}
	var report scenario.Report
	if e = json.Unmarshal(data, &report); e != nil {
		t.Fatal(e)
	}
	if _, e := reportview.Validate(context.Background(), data, report.RunID); e != nil {
		t.Fatalf("real finalization report rejected: %v", e)
	}
	if report.Status != "error" || report.ExitCode == nil || *report.ExitCode != 3 {
		t.Fatalf("CLI result code=%d reason=%s but durable report status=%s exit=%v", result.Code, result.Reason, report.Status, report.ExitCode)
	}
}
