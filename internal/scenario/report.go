package scenario

import (
	"encoding/json"
	"operator-dynamo/internal/contracts"
	"time"
)

type StepResult struct {
	ID          string   `json:"id"`
	Operation   string   `json:"operation"`
	DurationNS  int64    `json:"durationNs"`
	Outcome     string   `json:"outcome"`
	ReasonCode  string   `json:"reasonCode,omitempty"`
	RequestIDs  []string `json:"requestIds,omitempty"`
	SampleCount int      `json:"sampleCount,omitempty"`
}
type Cleanup struct {
	Outcome    string `json:"outcome"`
	ReasonCode string `json:"reasonCode,omitempty"`
	DurationNS int64  `json:"durationNs"`
}
type ScenarioResult struct {
	SourcePath   string       `json:"sourcePath"`
	Name         string       `json:"name"`
	SuitePaths   []string     `json:"suitePaths"`
	SessionID    string       `json:"sessionId,omitempty"`
	ArtifactPath string       `json:"artifactPath,omitempty"`
	StartedAt    *time.Time   `json:"startedAt,omitempty"`
	EndedAt      *time.Time   `json:"endedAt,omitempty"`
	DurationNS   int64        `json:"durationNs"`
	Status       string       `json:"status"`
	ReasonCode   string       `json:"reasonCode,omitempty"`
	Steps        []StepResult `json:"steps"`
	Cleanup      Cleanup      `json:"cleanup"`
}
type Report struct {
	APIVersion    string                  `json:"apiVersion"`
	Kind          string                  `json:"kind"`
	RunID         string                  `json:"runId"`
	SelectionPath string                  `json:"selectionPath"`
	Inputs        []contracts.InputDigest `json:"inputs"`
	StartedAt     time.Time               `json:"startedAt"`
	EndedAt       *time.Time              `json:"endedAt,omitempty"`
	DurationNS    int64                   `json:"durationNs"`
	Status        string                  `json:"status"`
	ReasonCode    string                  `json:"reasonCode,omitempty"`
	ExitCode      *int                    `json:"exitCode,omitempty"`
	Counts        map[string]int          `json:"counts"`
	Scenarios     []ScenarioResult        `json:"scenarios"`
}

func NewReport(plan *Plan, id string) *Report {
	r := &Report{APIVersion: "dynamo/v1alpha1", Kind: "TestRunReport", RunID: id, SelectionPath: plan.Selection, Inputs: plan.Inputs, StartedAt: time.Now().UTC(), Status: "running"}
	for _, s := range plan.Scenarios {
		v := ScenarioResult{SourcePath: s.SourcePath, Name: s.Name, SuitePaths: append([]string{}, s.SuitePaths...), Status: "pending", Cleanup: Cleanup{Outcome: "notStarted"}}
		for _, step := range s.Steps {
			v.Steps = append(v.Steps, StepResult{ID: step.ID, Operation: step.Operation, Outcome: "pending"})
		}
		r.Scenarios = append(r.Scenarios, v)
	}
	r.Recount()
	return r
}
func (r *Report) Recount() {
	r.Counts = map[string]int{"total": len(r.Scenarios), "pending": 0, "running": 0, "passed": 0, "failed": 0, "error": 0, "cancelled": 0, "skipped": 0}
	for _, s := range r.Scenarios {
		r.Counts[s.Status]++
	}
}

// Reserve serialized structural data plus bounded future IDs/times/diagnostics.
// No expected/actual fixture values are ever included in this bound or report.
func ValidateReportBound(plan *Plan) error {
	r := NewReport(plan, "XXXXXXXXXXXXXXXXXXXXXXXXXX")
	b, e := json.Marshal(r)
	if e != nil {
		return contracts.ErrInvalid
	}
	bound := len(b) + 4096
	for _, s := range plan.Scenarios {
		bound += 8192
		for range s.Steps {
			bound += 8192
		}
	} // includes 1024 bytes fully JSON-escaped detail and all generated fields.
	if bound > 16<<20 {
		return contracts.ErrInvalid
	}
	return nil
}
func (r *Report) Finish(status, reason string, code int, start time.Time) {
	now := time.Now().UTC()
	r.Status = status
	r.ReasonCode = reason
	r.ExitCode = &code
	r.EndedAt = &now
	r.DurationNS = time.Since(start).Nanoseconds()
	r.Recount()
}
