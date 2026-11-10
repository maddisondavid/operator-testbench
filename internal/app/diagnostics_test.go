package app

import (
	"context"
	"errors"
	"operator-dynamo/internal/contracts"
	"testing"
)

type diagnosticComponent struct {
	stub
	data contracts.APIDiagnostic
}

func (c *diagnosticComponent) Diagnostics() contracts.APIDiagnostic { return c.data }

type diagnosticJournal struct {
	memoryJournal
	observations []contracts.Observation
	fail         bool
}

func (j *diagnosticJournal) Record(o contracts.Observation) error {
	if j.fail {
		return errors.New("capture failure")
	}
	j.observations = append(j.observations, o)
	return nil
}
func TestDiagnosticCaptureFailureAndLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		calls := []string{}
		j := &diagnosticJournal{fail: fail}
		work := &memoryWorkspace{}
		c := &diagnosticComponent{stub: stub{name: "api", calls: &calls, startErr: errors.New("startup failed")}, data: contracts.APIDiagnostic{Notice: "Earlier API rejection: UnsupportedQuery", Records: 1}}
		a := &Application{OpenWorkspace: func(string) (Workspace, error) { return work, nil }, OpenJournal: func(Workspace, string) (Journal, error) { return j, nil }, Components: func(contracts.Options) []Component { return []Component{c} }}
		var got []contracts.APIDiagnostic
		err := a.Run(context.Background(), contracts.Options{Workdir: "unused", Diagnose: func(d contracts.APIDiagnostic) { got = append(got, d) }})
		if err == nil || len(got) != 1 || got[0].SessionID == "" || !j.closed || !work.closed {
			t.Fatal(err, got, j)
		}
		if fail {
			if got[0].Capture != "failed" || got[0].Cleanup != "failed" {
				t.Fatal(got)
			}
		} else {
			if got[0].Capture != "recorded" || len(j.observations) != 1 || j.observations[0].Kind != "api.rejections.summary" {
				t.Fatal(got, j.observations)
			}
		}
	}
}

func TestDiagnosticFlushFailureIsNotRecordedSuccess(t *testing.T) {
	calls := []string{}
	j := &diagnosticJournal{}
	j.closeErr = errors.New("flush failure")
	work := &memoryWorkspace{}
	c := &diagnosticComponent{stub: stub{name: "api", calls: &calls, startErr: errors.New("startup failed")}, data: contracts.APIDiagnostic{Notice: "Earlier rejection", Records: 1}}
	a := &Application{OpenWorkspace: func(string) (Workspace, error) { return work, nil }, OpenJournal: func(Workspace, string) (Journal, error) { return j, nil }, Components: func(contracts.Options) []Component { return []Component{c} }}
	var got contracts.APIDiagnostic
	if err := a.Run(context.Background(), contracts.Options{Diagnose: func(d contracts.APIDiagnostic) { got = d }}); err == nil || got.Capture != "failed" || got.Cleanup != "failed" {
		t.Fatal(err, got)
	}
}
