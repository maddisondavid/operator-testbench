package contracts

import "encoding/json"

// ResourceCommand contains raw JSON so adapters cannot round numeric values.
// Explicit preconditions describe intent, not proof of an earlier authenticated read.
type ResourceCommand struct {
	SessionID       string          `json:"sessionId"`
	ActionID        string          `json:"actionId"`
	Operation       string          `json:"operation"`
	APIVersion      string          `json:"apiVersion"`
	Kind            string          `json:"kind"`
	Namespace       string          `json:"namespace"`
	Name            string          `json:"name"`
	UID             string          `json:"uid"`
	ResourceVersion string          `json:"resourceVersion"`
	Object          json.RawMessage `json:"object"`
}
type CommandResult struct {
	SessionID       string `json:"sessionId"`
	ActionID        string `json:"actionId"`
	RequestID       string `json:"requestId,omitempty"`
	Status          int    `json:"status"`
	Disposition     string `json:"disposition"`
	Reason          string `json:"reason,omitempty"`
	SessionFailed   bool   `json:"sessionFailed,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"`
	ObjectJSON      string `json:"objectJson,omitempty"`
}
