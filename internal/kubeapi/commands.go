package kubeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strings"
)

type commandKey struct{}
type commandCall struct {
	command       contracts.ResourceCommand
	state         string
	captureFailed bool
}

// CloseCommands linearizes closure with mutation, never with response delivery.
func (s *Server) CloseCommands() { s.mu.Lock(); s.commandsClosed = true; s.mu.Unlock() }

// Command is a narrow API-owned adapter to the ordinary request/mutation path.
// No alternate resource store, accounting, validation or event publication exists.
func (s *Server) Command(ctx context.Context, cmd contracts.ResourceCommand) (contracts.CommandResult, error) {
	result := contracts.CommandResult{SessionID: cmd.SessionID, ActionID: cmd.ActionID, Disposition: "rejected"}
	reject := func(code int, reason string) (contracts.CommandResult, error) {
		result.Status = code
		result.Reason = reason
		return result, nil
	}
	if cmd.Operation != "create" && cmd.Operation != "update" {
		return reject(405, "unsupported_operation")
	}
	var reg Registration
	found := false
	for _, r := range s.Registrations {
		if r.APIVersion() == cmd.APIVersion && r.Kind == cmd.Kind {
			reg = r
			found = true
			break
		}
	}
	if !found {
		return reject(404, "type_not_found")
	}
	if reg.Key() == "v1/secrets" {
		return reject(403, "secret_editing_disabled")
	}
	if !ValidName(cmd.Name) || (reg.Scope == "Cluster" && cmd.Namespace != "") || (reg.Scope == "Namespaced" && (!ValidName(cmd.Namespace) || len(cmd.Namespace) > 63 || strings.Contains(cmd.Namespace, "."))) {
		return reject(400, "invalid_target")
	}
	if len(cmd.Object) == 0 || int64(len(cmd.Object)) > s.Limits.BodyBytes {
		return reject(413, "object_too_large")
	}
	path := "/apis/" + reg.APIVersion() + "/"
	if reg.Group == "" {
		path = "/api/" + reg.Version + "/"
	}
	if reg.Scope == "Namespaced" {
		path += "namespaces/" + cmd.Namespace + "/"
	}
	path += reg.Plural
	method := "POST"
	if cmd.Operation == "update" {
		method = "PUT"
		path += "/" + cmd.Name
	}
	call := &commandCall{command: cmd}
	request, _ := http.NewRequestWithContext(context.WithValue(ctx, commandKey{}, call), method, path, bytes.NewReader(cmd.Object))
	request.Header.Set("Content-Type", "application/json")
	writer := &commandResponse{headers: make(http.Header)}
	s.ServeHTTP(writer, request)
	result.RequestID = writer.headers.Get("X-Dynamo-Request-ID")
	result.Status = writer.status
	if call.state == "committed" {
		result.Disposition = "committed"
	} else if call.state == "unchanged" {
		result.Disposition = "no-op"
	}
	if (call.state == "committed" || call.state == "unchanged") && (writer.status >= 500 || call.captureFailed) {
		if call.state == "committed" {
			result.Disposition = "committed-but-session-failed"
		}
		result.SessionFailed = true
		result.Status = 503
		result.Reason = "CommittedButSessionFailed"
		if call.state == "unchanged" {
			result.Reason = "UnchangedButSessionFailed"
		}
	}
	var status struct{ Reason string }
	_ = json.Unmarshal(writer.body.Bytes(), &status)
	if result.Disposition == "rejected" {
		result.Reason = status.Reason
	}
	if writer.status >= 200 && writer.status < 300 {
		result.ObjectJSON = writer.body.String()
		var value struct {
			Metadata struct {
				UID             string
				ResourceVersion string
			}
		}
		_ = json.Unmarshal(writer.body.Bytes(), &value)
		result.UID = value.Metadata.UID
		result.ResourceVersion = value.Metadata.ResourceVersion
	}
	if writer.overflow {
		result.Disposition = "unknown"
		result.ObjectJSON = ""
		result.Reason = "response_capacity"
		result.Status = 503
	}
	return result, nil
}

type commandResponse struct {
	headers  http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *commandResponse) Header() http.Header { return w.headers }
func (w *commandResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *commandResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(b) > 8<<20 {
		w.overflow = true
		return 0, io.ErrShortBuffer
	}
	return w.body.Write(b)
}

// Caller holds the publication lock. These extra preconditions do not change
// the ordinary Kubernetes client's root UPDATE compatibility contract.
func (s *Server) commandPreconditions(ctx context.Context, key string, obj, meta map[string]any) (int, string) {
	if code, reason := s.commitAllowed(ctx); code != 0 {
		return code, reason
	}
	call, _ := ctx.Value(commandKey{}).(*commandCall)
	if call == nil {
		return 0, ""
	}
	c := call.command
	if obj["apiVersion"] != c.APIVersion || obj["kind"] != c.Kind || meta["name"] != c.Name {
		return 422, "InvalidPreconditions"
	}
	ns, _ := meta["namespace"].(string)
	if ns != c.Namespace {
		return 422, "InvalidPreconditions"
	}
	if c.Operation == "create" {
		if c.UID != "" || c.ResourceVersion != "" {
			return 422, "InvalidPreconditions"
		}
		return 0, ""
	}
	if c.UID == "" || c.ResourceVersion == "" || len(c.UID) > 256 || len(c.ResourceVersion) > 256 || meta["uid"] != c.UID || meta["resourceVersion"] != c.ResourceVersion {
		return 422, "InvalidPreconditions"
	}
	stored, exists := s.objects[key]
	if !exists {
		return 404, "NotFound"
	}
	if stored.summary.UID != c.UID || stored.summary.ResourceVersion != c.ResourceVersion {
		return 409, "Conflict"
	}
	return 0, ""
}

func (s *Server) commitAllowed(ctx context.Context) (int, string) {
	if s.commandsClosed {
		return 410, "SessionEnded"
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return 503, "SessionFailed"
	}
	if ctx.Err() != nil {
		return 408, "RequestCancelled"
	}
	return 0, ""
}
