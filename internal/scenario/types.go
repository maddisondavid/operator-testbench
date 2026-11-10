// Package scenario owns contained selection, test sequencing and assertions.
// It consumes session/action ports and never imports Docker or API stores.
package scenario

import (
	"context"
	"operator-dynamo/internal/contracts"

	"time"
)

type Source interface {
	ReadSource(context.Context, string, int64) (contracts.InputFile, error)
	Inspect(string) (contracts.FileEntry, error)
	ListDirectory(context.Context, string, int) ([]contracts.FileEntry, string, error)
	CheckDirectory(string, string) error
}
type Resource struct{ APIVersion, Kind, ListKind, Plural, Scope string }
type Target struct {
	APIVersion  string            `json:"apiVersion"`
	Kind        string            `json:"kind"`
	Namespace   string            `json:"namespace,omitempty"`
	Name        string            `json:"name,omitempty"`
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
	Resource    Resource          `json:"-"`
}
type Reference struct {
	Observation string `json:"observation"`
	Pointer     string `json:"pointer"`
}
type Field struct {
	Pointer    string     `json:"pointer"`
	Equals     any        `json:"equals,omitempty"`
	EqualsSet  bool       `json:"-"`
	EqualsFrom *Reference `json:"equalsFrom,omitempty"`
	Exists     *bool      `json:"exists,omitempty"`
}
type Assertion struct {
	Target  Target
	Count   int
	Fields  []Field
	SaveAs  string
	Timeout time.Duration
}
type Step struct {
	ID, Operation, FixturePath string
	Fixture                    map[string]any
	Target                     Target
	Assert                     *Assertion
}
type Scenario struct {
	SourcePath, Name string
	SuitePaths       []string
	Steps            []Step
	Timeout          time.Duration
}
type Plan struct {
	Selection string
	Scenarios []Scenario
	Inputs    []contracts.InputDigest
}
type Error struct {
	Reason, Path string
	Line, Column int
	Invalid      bool
}

func (e *Error) Error() string { return e.Reason }
func (e *Error) Unwrap() error {
	if e.Invalid {
		return contracts.ErrInvalid
	}
	return nil
}
func invalid(path string) *Error { return &Error{Reason: "invalid_input", Path: path, Invalid: true} }

type Response struct {
	Code      int
	Object    map[string]any
	RequestID string
}
type Gateway interface {
	Request(context.Context, string, Target, any) (Response, error)
	Close()
}
type Session interface {
	ID() string
	Gateway() Gateway
	Failure() string
	Record(string, contracts.LifecyclePayload) error
	Stop(context.Context) error
}
type Factory interface {
	Start(context.Context, string) (Session, error)
}
