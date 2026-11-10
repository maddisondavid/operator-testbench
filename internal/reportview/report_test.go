package reportview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActualLegacyReportsAndInvalidVariants(t *testing.T) {
	paths, e := filepath.Glob("../../test/reports/*report.json")
	if e != nil || len(paths) < 4 {
		t.Fatal("missing historical corpus", e)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			b, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			var v map[string]any
			json.Unmarshal(b, &v)
			id := v["runId"].(string)
			view, e := Validate(context.Background(), b, id)
			if e != nil {
				t.Fatal(e)
			}
			if _, ok := view["durationNs"].(string); !ok {
				t.Fatal("numeric leaf not a string")
			}
			for _, change := range []func(map[string]any){func(m map[string]any) { m["status"] = map[string]any{} }, func(m map[string]any) { m["unknown"] = true }, func(m map[string]any) { m["counts"].(map[string]any)["total"] = 900 }, func(m map[string]any) { m["runId"] = "other" }, func(m map[string]any) { m["selectionPath"] = "../escape" }, func(m map[string]any) {
				m["scenarios"].([]any)[0].(map[string]any)["steps"].([]any)[0].(map[string]any)["sampleCount"] = json.Number("1e999999999")
			}} {
				json.Unmarshal(b, &v)
				change(v)
				bad, _ := json.Marshal(v)
				if _, e = Validate(context.Background(), bad, id); e == nil {
					t.Fatal("invalid report accepted", string(bad))
				}
			}
			duplicate := strings.Replace(string(b), "{", "{\"runId\":\"duplicate\",", 1)
			if _, e = Validate(context.Background(), []byte(duplicate), id); e == nil {
				t.Fatal("duplicate accepted")
			}
		})
	}
}
func TestBoundedExactIntegers(t *testing.T) {
	for _, s := range []string{"1e999999999", "9223372036854775808", "-1", "0.1"} {
		if _, ok := integer(json.Number(s)); ok {
			t.Fatal(s)
		}
	}
	for s, want := range map[string]int64{"1.0": 1, "0e999999999999999999999999": 0, "9.007199254740993e15": 9007199254740993, "9223372036854775807": 9223372036854775807} {
		if got, ok := integer(json.Number(s)); !ok || got != want {
			t.Fatal(s, got)
		}
	}
}

func TestLegitimateIntermediateAndPrimaryCleanupOutcomes(t *testing.T) {
	b, e := os.ReadFile("../../test/reports/test-mode-report.json")
	if e != nil {
		t.Fatal(e)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	id := m["runId"].(string)
	scenarios := m["scenarios"].([]any)
	m["status"] = "error"
	m["exitCode"] = 3
	scenarios[0].(map[string]any)["cleanup"] = map[string]any{"outcome": "error", "durationNs": 1, "reasonCode": "cleanup_failed"}
	raw, _ := json.Marshal(m)
	if _, e = Validate(context.Background(), raw, id); e != nil {
		t.Fatal("primary passed with cleanup failure", e)
	}
	m["status"] = "running"
	delete(m, "endedAt")
	delete(m, "exitCode")
	s := scenarios[1].(map[string]any)
	s["status"] = "pending"
	delete(s, "sessionId")
	delete(s, "artifactPath")
	s["cleanup"] = map[string]any{"outcome": "notStarted", "durationNs": 0}
	for _, step := range s["steps"].([]any) {
		step.(map[string]any)["outcome"] = "pending"
	}
	counts := m["counts"].(map[string]any)
	counts["passed"] = 1
	counts["pending"] = 1
	raw, _ = json.Marshal(m)
	if _, e = Validate(context.Background(), raw, id); e != nil {
		t.Fatal("intermediate with earlier cleanup failure", e)
	}
}
