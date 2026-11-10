package cli

import (
	"bytes"
	"context"
	"errors"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
)

type runner struct {
	calls   int
	options contracts.Options
	err     error
}

func (r *runner) Run(_ context.Context, o contracts.Options) error {
	r.calls++
	r.options = o
	return r.err
}
func TestArgumentsAndOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		code, calls int
		message     string
	}{
		{"help", []string{"--help"}, 0, 0, "Usage:"},
		{"missing", nil, 2, 0, "required"},
		{"empty", []string{"--workdir="}, 2, 0, "required"},
		{"unknown", []string{"--unknown"}, 2, 0, "flag"},
		{"positional", []string{"--workdir", "x", "extra"}, 2, 0, "positional"},
		{"invalid bool", []string{"--ui=maybe"}, 2, 0, "invalid"},
		{"bad address", []string{"--workdir", "x", "--listen", "bad"}, 2, 0, "HOST:PORT"},
		{"bad port", []string{"--workdir", "x", "--listen", ":65536"}, 2, 0, "port"},
		{"negative port", []string{"--workdir", "x", "--listen", ":-1"}, 2, 0, "port"},
		{"empty tests", []string{"--workdir", "x", "--tests="}, 2, 0, "nonempty"},
		{"tests", []string{"--workdir", "x", "--tests", "missing"}, 0, 1, "Operator Dynamo"},
		{"write needs UI", []string{"--workdir", "x", "--ui-write", "--ui-origin", "http://127.0.0.1:8080"}, 2, 0, "--ui"},
		{"write needs origin", []string{"--workdir", "x", "--ui", "--ui-write"}, 2, 0, "--ui-origin"},
		{"write excludes tests", []string{"--workdir", "x", "--ui", "--ui-write", "--ui-origin", "http://127.0.0.1:8080", "--tests", "suite"}, 2, 0, "--tests"},
		{"write enabled", []string{"--workdir", "x", "--ui", "--ui-write", "--ui-origin", "http://127.0.0.1:8080"}, 0, 1, "Operator Dynamo"},
		{"ui", []string{"--workdir", "x", "--ui"}, 0, 1, "Operator Dynamo"},
		{"valid", []string{"--workdir", "x", "--listen", ":0"}, 0, 1, "Operator Dynamo"},
		{"ui false", []string{"--workdir", "x", "--ui=false"}, 0, 1, "Operator Dynamo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{}
			var out memorySink
			code := Execute(context.Background(), tc.args, &out, r)
			if code != tc.code || r.calls != tc.calls || !strings.Contains(out.String(), tc.message) {
				t.Fatalf("code=%d calls=%d output=%s", code, r.calls, out.String())
			}
			if r.calls > 0 && r.options.Workdir != "x" {
				t.Fatal(r.options)
			}
		})
	}
	r := &runner{err: errors.New("cleanup failed")}
	var out bytes.Buffer
	if code := Execute(context.Background(), []string{"--workdir", "x"}, &out, r); code != 3 || !strings.Contains(out.String(), "infrastructure failed") {
		t.Fatal(code, out.String())
	}
}

func TestReportFailureDiagnosticOverridesAnyPriorArtifact(t *testing.T) {
	r := &runner{err: &contracts.TestResult{Code: 3, Status: "error", Reason: "report_failed", ReportPath: "runs/run/report.json"}}
	var out bytes.Buffer
	if code := Execute(context.Background(), []string{"--workdir", "x", "--tests", "x/tests"}, &out, r); code != 3 || !strings.Contains(out.String(), "this run is unsuccessful") || strings.Contains(out.String(), "tests: passed") {
		t.Fatal(code, out.String())
	}
}
