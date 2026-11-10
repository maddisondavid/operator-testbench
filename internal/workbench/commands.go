package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/uiaddress"
	"operator-dynamo/internal/uijson"
	"regexp"
	"strings"
	"time"
)

const commandLimit = (1 << 20) + (16 << 10)

var actionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (h *Handler) EnableWrites(origin string) error {
	normalized, err := uiaddress.Origin(origin)
	if err != nil {
		return err
	}
	h.writeAuthority, err = uiaddress.BrowserOrigin(normalized)
	return err
}
func (h *Handler) command(ctx context.Context, w http.ResponseWriter, r *http.Request, control *http.ResponseController, authority string, unreadBody *bool) {
	// An early rejection never drains an untrusted/stalled upload.
	bodyRead := false
	reject := func(code int, reason string) {
		if !bodyRead {
			r.Close = true
			w.Header().Set("Connection", "close")
			_ = control.SetReadDeadline(time.Now())
		}
		problem(w, code, reason)
	}
	if h.writeAuthority == "" {
		reject(403, "writes_disabled")
		return
	}
	if r.Method != "POST" {
		reject(405, "method_not_allowed")
		return
	}
	origins := r.Header.Values("Origin")
	headers := r.Header.Values("X-Dynamo-Write")
	if len(origins) != 1 || len(headers) != 1 || headers[0] != "1" || authority != h.writeAuthority {
		reject(403, "write_origin_required")
		return
	}
	origin, err := uiaddress.BrowserOrigin(origins[0])
	if err != nil || origin != h.writeAuthority {
		reject(403, "forbidden_origin")
		return
	}
	values := r.Header.Values("Content-Type")
	if len(values) != 1 {
		reject(415, "unsupported_media_type")
		return
	}
	media, _, err := mime.ParseMediaType(values[0])
	if err != nil || media != "application/json" {
		reject(415, "unsupported_media_type")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/ui/api/v1/sessions/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/ui/api/v1/sessions/") || len(parts) != 2 || parts[1] != "commands" || !contracts.ValidID(parts[0]) || r.URL.RawQuery != "" {
		reject(400, "invalid_command_route")
		return
	}
	if r.ContentLength > commandLimit {
		reject(413, "command_too_large")
		return
	}
	if !h.admit() {
		reject(429, "rate_limited")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		reject(429, "capacity")
		return
	}
	deadline, _ := ctx.Deadline()
	_ = control.SetReadDeadline(deadline)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = control.SetReadDeadline(time.Now()) })
	raw, err := io.ReadAll(io.LimitReader(r.Body, commandLimit+1))
	if !stop() {
		<-done
	}
	if err != nil || ctx.Err() != nil || !time.Now().Before(deadline) {
		reject(504, "request_timeout")
		return
	}
	if len(raw) > commandLimit {
		reject(413, "command_too_large")
		return
	}
	bodyRead = true
	*unreadBody = false
	_ = control.SetReadDeadline(time.Time{})
	command, err := decodeCommand(raw)
	if err != nil || !actionID.MatchString(command.ActionID) || command.SessionID != parts[0] {
		reject(400, "invalid_command")
		return
	}
	if len(command.Object) > 1<<20 {
		reject(413, "object_too_large")
		return
	}
	if command.APIVersion == "v1" && command.Kind == "Secret" {
		reject(403, "secret_editing_disabled")
		return
	}
	catalog, ok := h.catalog.(CommandCatalog)
	if !ok {
		reject(403, "writes_disabled")
		return
	}
	result, err := catalog.ExecuteCommand(ctx, parts[0], command)
	if err != nil {
		var re *contracts.ReadError
		if errors.As(err, &re) {
			reject(re.Status, re.Code)
		} else {
			reject(504, "outcome_unknown")
		}
		return
	}
	// Do not replace a known committed result with a read-lease closure error.
	payload, err := uijson.Encode(ctx, map[string]any{"apiVersion": "dynamo.ui/v1alpha1", "result": result})
	if err != nil {
		reject(504, "outcome_unknown")
		return
	}
	if ctx.Err() != nil {
		reject(504, "outcome_unknown")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(result.Status)
	_, _ = w.Write(payload)
}
func decodeCommand(raw []byte) (contracts.ResourceCommand, error) {
	var result contracts.ResourceCommand
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return result, contracts.ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return result, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return result, contracts.ErrInvalid
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return result, err
		}
	}
	if _, err := d.Token(); err != nil {
		return result, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return result, contracts.ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		return result, err
	}
	return result, nil
}
