// Package contracts contains the small value vocabulary shared by adapters and components.
package contracts

import (
	"errors"
	"time"
)

// APIDiagnostic contains only API-owned bounded summary text, never raw errors or bodies.
type APIDiagnostic struct {
	SessionID, Notice, Terminal, Capture, Cleanup string
	Records                                       uint64
	Truncated                                     bool
}

type Options struct {
	Diagnose func(APIDiagnostic)
	Verbose  bool
	Console  *Console
	Workdir  string
	Tests    string
	UI       bool
	UIWrite  bool
	UIOrigin string
	Listen   string
}

type Session struct{ ID string }

type Metadata struct {
	SchemaVersion int        `json:"schemaVersion"`
	ID            string     `json:"id"`
	State         string     `json:"state"`
	StartedAt     time.Time  `json:"startedAt"`
	EndedAt       *time.Time `json:"endedAt,omitempty"`
}

// Lifecycle payloads deliberately contain no configuration, environment or error text.
type Event struct {
	SchemaVersion int              `json:"schemaVersion"`
	SessionID     string           `json:"sessionId"`
	EventID       string           `json:"eventId"`
	Sequence      uint64           `json:"sequence"`
	Timestamp     time.Time        `json:"timestamp"`
	Kind          string           `json:"kind"`
	Actor         string           `json:"actor"`
	CorrelationID string           `json:"correlationId"`
	CausationID   string           `json:"causationId,omitempty"`
	Payload       LifecyclePayload `json:"payload"`
}
type LifecyclePayload struct {
	RunID         string   `json:"runId,omitempty"`
	ScenarioID    string   `json:"scenarioId,omitempty"`
	StepID        string   `json:"stepId,omitempty"`
	ReasonCode    string   `json:"reasonCode,omitempty"`
	RequestIDs    []string `json:"requestIds,omitempty"`
	SampleCount   int      `json:"sampleCount,omitempty"`
	Artifact      string   `json:"artifact,omitempty"`
	ContextDigest string   `json:"contextDigest,omitempty"`
	BuildTag      string   `json:"buildTag,omitempty"`
	Bytes         int      `json:"bytes,omitempty"`
	Records       uint64   `json:"records,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
	State         string   `json:"state,omitempty"`
	Operation     string   `json:"operation,omitempty"`
	Resource      string   `json:"resource,omitempty"`
	Namespace     string   `json:"namespace,omitempty"`
	Name          string   `json:"name,omitempty"`
	Version       string   `json:"resourceVersion,omitempty"`
	Status        int      `json:"status,omitempty"`
	ContainerID   string   `json:"containerId,omitempty"`
	ImageID       string   `json:"imageId,omitempty"`
	Notice        string   `json:"notice,omitempty"`
	DurationNS    int64    `json:"durationNs,omitempty"`
}

// ErrInvalid identifies invalid user configuration without exposing its values.
var ErrInvalid = errors.New("invalid configuration")

// Observation contains allowlisted diagnostics only, never request bodies or environment.
type Observation struct {
	Kind          string
	CorrelationID string
	Payload       LifecyclePayload
}

// InputDigest records immutable run input identity, never content.
type InputDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type TestSummary struct{ Name, Status, Step, Reason string }
type TestResult struct {
	Code                       int
	Status, Reason, ReportPath string
	Scenarios                  []TestSummary
	Counts                     map[string]int
}

func (r *TestResult) Error() string { return "test run " + r.Status + ": " + r.Reason }
