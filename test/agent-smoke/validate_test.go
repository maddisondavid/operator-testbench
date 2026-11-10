package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) (string, string, map[string]any) {
	t.Helper()
	b, e := os.ReadFile("fixture-report.json")
	if e != nil {
		t.Fatal(e)
	}
	var r map[string]any
	json.Unmarshal(b, &r)
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	os.MkdirAll(root, 0755)
	manifest := map[string]string{}
	for _, v := range r["inputs"].([]any) {
		in := v.(map[string]any)
		p := in["path"].(string)
		data, e := os.ReadFile(filepath.Join("../../examples/agent-smoke", p))
		if p == "operator-src/operator" {
			data = []byte("unit-test binding only; never executed")
			e = nil
		}
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(data)
		in["sha256"] = hex.EncodeToString(h[:])
		manifest[p] = in["sha256"].(string)
		f := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(f), 0755)
		os.WriteFile(f, data, 0644)
	}
	for _, v := range r["scenarios"].([]any) {
		s := v.(map[string]any)
		p := filepath.Join(root, s["artifactPath"].(string))
		os.MkdirAll(p, 0755)
		for _, name := range []string{"events.jsonl", "session.json", "operator-build.json"} {
			b, e := os.ReadFile("fixture-" + name)
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(p, name), b, 0644)
		}
	}
	b, _ = json.Marshal(manifest)
	os.WriteFile(filepath.Join(base, "inputs.json"), b, 0644)
	os.WriteFile(filepath.Join(base, "started.txt"), []byte("2026-01-01T00:00:00Z"), 0644)
	p := filepath.Join(root, "runs", r["runId"].(string), "report.json")
	os.MkdirAll(filepath.Dir(p), 0755)
	writeReport(t, p, r)
	return root, p, r
}
func writeReport(t *testing.T, p string, r map[string]any) {
	t.Helper()
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(p, b, 0644); e != nil {
		t.Fatal(e)
	}
}
func TestInvocationBinding(t *testing.T) {
	cases := map[string]func(string, string, map[string]any){
		"missing-session": func(root, p string, r map[string]any) {
			s := r["scenarios"].([]any)[0].(map[string]any)
			delete(s, "sessionId")
			delete(s, "artifactPath")
			writeReport(t, p, r)
		},
		"wrong-operation": func(root, p string, r map[string]any) {
			s := r["scenarios"].([]any)[0].(map[string]any)
			s["steps"].([]any)[0].(map[string]any)["operation"] = "delete"
			writeReport(t, p, r)
		},
		"missing-build": func(root, p string, r map[string]any) {
			s := r["scenarios"].([]any)[0].(map[string]any)
			os.Remove(filepath.Join(root, s["artifactPath"].(string), "operator-build.json"))
		},
		"missing": func(root, p string, r map[string]any) { os.Remove(p) },
		"stale": func(root, p string, r map[string]any) {
			os.WriteFile(filepath.Join(root, "../started.txt"), []byte("2099-01-01T00:00:00Z"), 0644)
		},
		"wrong-id": func(root, p string, r map[string]any) {
			r["runId"] = "AAAAAAAAAAAAAAAAAAAAAAAAAA"
			writeReport(t, p, r)
		},
		"selection": func(root, p string, r map[string]any) { r["selectionPath"] = "other"; writeReport(t, p, r) },
		"inputs": func(root, p string, r map[string]any) {
			os.WriteFile(filepath.Join(root, "config/operator.yaml"), []byte("changed"), 0644)
		},
		"missing-binding": func(root, p string, r map[string]any) { r["inputs"] = r["inputs"].([]any)[1:]; writeReport(t, p, r) },
		"incomplete":      func(root, p string, r map[string]any) { delete(r, "endedAt"); writeReport(t, p, r) },
		"duplicate":       func(root, p string, r map[string]any) { os.Mkdir(filepath.Join(root, "runs", "OTHER"), 0755) },
		"artifact": func(root, p string, r map[string]any) {
			s := r["scenarios"].([]any)[0].(map[string]any)
			os.Remove(filepath.Join(root, s["artifactPath"].(string), "events.jsonl"))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			root, p, r := fixture(t)
			change(root, p, r)
			if _, e := validate(root, "tests/smoke", 0); e == nil {
				t.Fatal("accepted mismatched evidence")
			}
		})
	}
	root, _, _ := fixture(t)
	if _, e := validate(root, "tests/smoke", 0); e != nil {
		t.Fatal(e)
	}
	if _, e := validate(root, "tests/smoke", 3); e == nil {
		t.Fatal("nonzero process accepted with passed report")
	}
}

func TestSiblingSuiteSelection(t *testing.T) {
	root, p, r := fixture(t)
	suite := []byte("apiVersion: dynamo/v1alpha1\nkind: Suite\nname: selected\nentries: [../tests/smoke]\n")
	os.MkdirAll(filepath.Join(root, "selected"), 0755)
	os.WriteFile(filepath.Join(root, "selected/suite.yaml"), suite, 0644)
	r["selectionPath"] = "selected"
	s := r["scenarios"].([]any)[0].(map[string]any)
	s["suitePaths"] = []string{"selected/suite.yaml"}
	r["inputs"] = append(r["inputs"].([]any), map[string]any{"path": "selected/suite.yaml", "sha256": digest(suite)})
	b, _ := os.ReadFile(filepath.Join(root, "../inputs.json"))
	var manifest map[string]string
	json.Unmarshal(b, &manifest)
	manifest["selected/suite.yaml"] = digest(suite)
	b, _ = json.Marshal(manifest)
	os.WriteFile(filepath.Join(root, "../inputs.json"), b, 0644)
	writeReport(t, p, r)
	if _, e := validate(root, "selected", 0); e != nil {
		t.Fatal(e)
	}
	s["suitePaths"] = []string{}
	writeReport(t, p, r)
	if _, e := validate(root, "selected", 0); e == nil {
		t.Fatal("incorrect suite ancestry admitted")
	}
}
