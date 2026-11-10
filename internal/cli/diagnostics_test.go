package cli

import (
	"bytes"
	"context"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
)

type diagnosticRunner struct{ fail bool }

func (r diagnosticRunner) Run(ctx context.Context, o contracts.Options) error {
	o.Diagnose(contracts.APIDiagnostic{SessionID: "session", Notice: "Earlier API rejections: UnsupportedQuery (400)\n", Terminal: "API request budget exhausted", Capture: "recorded", Cleanup: "complete"})
	if r.fail {
		return &contracts.TestResult{Code: 3, Status: "error", Reason: "api_failed", Counts: map[string]int{}}
	}
	return nil
}
func TestFinalDiagnosticDoesNotChangeOutcome(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var b bytes.Buffer
		code := Execute(context.Background(), []string{"--workdir", "unused"}, &b, diagnosticRunner{fail})
		want := 0
		if fail {
			want = 3
		}
		if code != want || !strings.Contains(b.String(), "API request budget exhausted") || !strings.Contains(b.String(), "cleanup=complete") {
			t.Fatal(code, b.String())
		}
	}
}
