package workbench

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"operator-dynamo/internal/workspace"
	"os"
	"path/filepath"
	"testing"
)

func TestReportUnionThroughRealHTTP(t *testing.T) {
	original, e := os.ReadFile("../../test/reports/test-mode-report.json")
	if e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	ws, e := workspace.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	h := handler(t)
	h.reports = ws
	s := observedServer(h)
	defer s.Close()
	for _, tc := range []struct {
		name   string
		value  any
		status int
	}{
		{"null", nil, 200}, {"empty", []any{}, 200}, {"paths", []any{"tests/suite.yaml"}, 200},
		{"string", "tests/suite.yaml", 422}, {"object", map[string]any{"path": "tests/suite.yaml"}, 422}, {"bool", true, 422}, {"number", json.Number("42"), 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var report map[string]any
			d := json.NewDecoder(bytes.NewReader(original))
			d.UseNumber()
			if e := d.Decode(&report); e != nil {
				t.Fatal(e)
			}
			report["scenarios"].([]any)[0].(map[string]any)["suitePaths"] = tc.value
			id := report["runId"].(string)
			dir := filepath.Join(root, "runs", id)
			if e := os.MkdirAll(dir, 0700); e != nil {
				t.Fatal(e)
			}
			raw, e := json.Marshal(report)
			if e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0600); e != nil {
				t.Fatal(e)
			}
			req, _ := http.NewRequest("GET", s.URL+"/ui/api/v1/runs/"+id, nil)
			req.Host = "198.51.100.10:18080"
			res, e := s.Client().Do(req)
			if e != nil {
				t.Fatal(e)
			}
			body, e := io.ReadAll(res.Body)
			res.Body.Close()
			if e != nil {
				t.Fatal(e)
			}
			if res.StatusCode != tc.status {
				t.Fatal(res.StatusCode, string(body))
			}
			var envelope map[string]any
			if e := json.Unmarshal(body, &envelope); e != nil {
				t.Fatal(e)
			}
			if tc.status == 422 {
				if envelope["error"].(map[string]any)["code"] != "invalid_report" {
					t.Fatal(string(body))
				}
				if envelope["reportJson"] != nil || envelope["reportView"] != nil {
					t.Fatal("invalid report leaked")
				}
			} else if envelope["reportJson"] != string(raw) {
				t.Fatal("stored bytes changed")
			}
			stored, e := os.ReadFile(filepath.Join(dir, "report.json"))
			if e != nil || !bytes.Equal(raw, stored) {
				t.Fatal("read mutated report", e)
			}
		})
	}
}
