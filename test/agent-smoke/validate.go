// Tooling adapter: reuse the product's report contract without a new result schema.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"operator-dynamo/internal/reportview"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"
)

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func validate(root, selection string, exit int) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err != nil {
		return "", fmt.Errorf("missing report")
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("ambiguous report")
	}
	startBytes, err := os.ReadFile(filepath.Join(root, "../started.txt"))
	if err != nil {
		return "", err
	}
	started, err := time.Parse(time.RFC3339Nano, string(startBytes))
	if err != nil {
		return "", err
	}
	id := entries[0].Name()
	path := filepath.Join(root, "runs", id, "report.json")
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 16<<20 {
		return "", fmt.Errorf("unsafe report")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	report, err := reportview.Validate(context.Background(), data, id)
	if err != nil {
		return "", err
	}
	if report["selectionPath"] != selection || report["endedAt"] == nil || report["status"] == "running" {
		return "", fmt.Errorf("report invocation mismatch or incomplete")
	}
	stamp, err := time.Parse(time.RFC3339Nano, report["startedAt"].(string))
	if err != nil || stamp.Before(started) {
		return "", fmt.Errorf("stale report")
	}
	code, ok := report["exitCode"].(string)
	if !ok || code != strconv.Itoa(exit) {
		return "", fmt.Errorf("process/report exit disagreement")
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, "../inputs.json"))
	if err != nil {
		return "", err
	}
	var manifest map[string]string
	if json.Unmarshal(manifestBytes, &manifest) != nil {
		return "", fmt.Errorf("invalid input manifest")
	}
	inputs, ok := report["inputs"].([]any)
	if !ok || len(inputs) == 0 {
		return "", fmt.Errorf("missing input binding")
	}
	plan, expectedInputs, err := expected(root, selection)
	if err != nil {
		return "", fmt.Errorf("selected plan cannot be validated: %w", err)
	}
	if len(inputs) != len(expectedInputs) {
		return "", fmt.Errorf("input set mismatch")
	}
	bound := map[string]bool{}
	for _, raw := range inputs {
		in := raw.(map[string]any)
		p := in["path"].(string)
		want := in["sha256"].(string)
		bound[p] = true
		if manifest[p] != want || expectedInputs[p] != want {
			return "", fmt.Errorf("input binding mismatch")
		}
		b, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if e != nil {
			return "", e
		}
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != want {
			return "", fmt.Errorf("input changed")
		}
	}
	if !bound["config/operator.yaml"] {
		return "", fmt.Errorf("missing configuration binding")
	}
	scenarios := report["scenarios"].([]any)
	if len(scenarios) == 0 || len(scenarios) != len(plan.Scenarios) {
		return "", fmt.Errorf("no scenarios")
	}
	for index, raw := range scenarios {
		s := raw.(map[string]any)
		p := s["sourcePath"].(string)
		if p != plan.Scenarios[index].SourcePath || s["name"] != plan.Scenarios[index].Name {
			return "", fmt.Errorf("selected scenario mismatch")
		}
		chain := []string{}
		if raw, ok := s["suitePaths"].([]any); ok {
			for _, p := range raw {
				chain = append(chain, p.(string))
			}
		}
		expectedChain := append([]string{}, plan.Scenarios[index].SuitePaths...)
		if !reflect.DeepEqual(chain, expectedChain) {
			return "", fmt.Errorf("suite ancestry mismatch")
		}
		steps := s["steps"].([]any)
		if len(steps) != len(plan.Scenarios[index].Steps) {
			return "", fmt.Errorf("step plan mismatch")
		}
		for n, raw := range steps {
			v := raw.(map[string]any)
			if v["id"] != plan.Scenarios[index].Steps[n].ID || v["operation"] != plan.Scenarios[index].Steps[n].Operation {
				return "", fmt.Errorf("step identity mismatch")
			}
		}
		if manifest[p] == "" || !bound[p] {
			return "", fmt.Errorf("scenario input absent")
		}
		if s["status"] == "passed" && (s["sessionId"] == nil || s["sessionId"] == "" || s["artifactPath"] == nil) {
			return "", fmt.Errorf("missing successful session identity")
		}
		if sid, ok := s["sessionId"].(string); ok && sid != "" {
			artifact, _ := s["artifactPath"].(string)
			if artifact != "sessions/"+sid {
				return "", fmt.Errorf("artifact identity mismatch")
			}
			if s["status"] == "passed" {
				if e := provenance(root, artifact, sid, started); e != nil {
					return "", e
				}
			}
			if st, e := os.Stat(filepath.Join(root, artifact, "events.jsonl")); e != nil || !st.Mode().IsRegular() {
				return "", fmt.Errorf("missing session journal")
			}
		}
	}
	if exit == 0 && report["status"] != "passed" {
		return "", fmt.Errorf("exit zero without passed report")
	}
	fmt.Printf("Recorded outcome: %s; Dynamo exit: %d\nReport: %s\n", report["status"], exit, path)
	for _, raw := range scenarios {
		s := raw.(map[string]any)
		if s["artifactPath"] != nil {
			artifact := filepath.Join(root, s["artifactPath"].(string))
			fmt.Printf("Artifacts: %s\n", artifact)
			if b, e := os.ReadFile(filepath.Join(artifact, "operator-build.json")); e == nil && len(b) <= 1<<20 {
				var build struct {
					DurationNS    int64  `json:"durationNs"`
					PreparationNS int64  `json:"preparationNs"`
					ImageID       string `json:"imageID"`
					Context       string `json:"contextSHA256"`
				}
				if json.Unmarshal(b, &build) == nil {
					fmt.Printf("Runtime build: %.3fs; build preparation: %.3fs; image: %q; context SHA256: %q\n", float64(build.DurationNS)/1e9, float64(build.PreparationNS)/1e9, build.ImageID, build.Context)
				}
			}
			cleanup := s["cleanup"].(map[string]any)
			fmt.Printf("Scenario duration ns: %s; cleanup: %s (%s ns)\n", s["durationNs"], cleanup["outcome"], cleanup["durationNs"])
		}
		for _, step := range s["steps"].([]any) {
			v := step.(map[string]any)
			if v["outcome"] == "failed" || v["outcome"] == "error" {
				fmt.Printf("Failed step: %q; reason: %q\n", v["id"], v["reasonCode"])
			}
		}
	}
	return path, nil
}
func main() {
	if len(os.Args) == 5 && os.Args[1] == "--configure" {
		if e := configure(os.Args[2], os.Args[3], os.Args[4]); e != nil {
			fmt.Fprintln(os.Stderr, "Configuration override failed")
			os.Exit(3)
		}
		return
	}
	if len(os.Args) != 4 {
		os.Exit(3)
	}
	code, e := strconv.Atoi(os.Args[3])
	if e != nil {
		os.Exit(3)
	}
	if _, e = validate(os.Args[1], os.Args[2], code); e != nil {
		fmt.Fprintln(os.Stderr, "Evidence validation failed:", e)
		os.Exit(3)
	}
}
