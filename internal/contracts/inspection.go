package contracts

import (
	"encoding/json"
	"regexp"
)

// ReadError exposes only a fixed diagnostic code across inspection ports.
type ReadError struct {
	Status int
	Code   string
}

func (e *ReadError) Error() string              { return e.Code }
func ReadFailure(status int, code string) error { return &ReadError{status, code} }

var opaqueID = regexp.MustCompile(`^[A-Z2-7]{26}$`)

func ValidID(id string) bool { return opaqueID.MatchString(id) }

type ResourceType struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Plural     string `json:"plural"`
	Scope      string `json:"scope"`
}
type ObjectSummary struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
}
type ResourceSnapshot struct {
	Items           []ObjectSummary `json:"items,omitempty"`
	ObjectJSON      string          `json:"objectJson,omitempty"`
	UID             string          `json:"uid,omitempty"`
	ResourceVersion string          `json:"resourceVersion,omitempty"`
}
type EventPage struct {
	Events        []json.RawMessage `json:"events"`
	NextAfter     string            `json:"nextAfter"`
	ObservedFirst *string           `json:"observedFirst"`
	ObservedLast  string            `json:"observedLast"`
	RetainedFirst *string           `json:"retainedFirst"`
	RetainedLast  *string           `json:"retainedLast"`
	Gap           bool              `json:"gap"`
}
type RunPage struct {
	IDs       []string `json:"ids"`
	NextAfter string   `json:"nextAfter"`
}

type SessionDescriptor struct {
	Writable      bool   `json:"writable"`
	ID            string `json:"id"`
	Scenario      string `json:"scenario,omitempty"`
	Index         int    `json:"index,omitempty"`
	State         string `json:"state"`
	OperatorState string `json:"operatorState"`
	APIAvailable  bool   `json:"apiAvailable"`
}
type Invocation struct {
	ID           string              `json:"invocationId"`
	CurrentRunID string              `json:"currentRunId,omitempty"`
	Sessions     []SessionDescriptor `json:"sessions"`
}
