package kubeapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"mime"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strconv"
	"strings"
	"time"
)

// Numbers are compared as coefficient/exponent pairs, never expanded powers or
// float64 values. Even enormous JSON exponents remain bounded by the input size.
func canonicalNumber(n json.Number) string {
	text := strings.ToLower(string(n))
	mantissa, exponent, has := strings.Cut(text, "e")
	exp := new(big.Int)
	if has {
		exp.SetString(exponent, 10)
	}
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	if point := strings.IndexByte(mantissa, '.'); point >= 0 {
		exp.Sub(exp, big.NewInt(int64(len(mantissa)-point-1)))
		mantissa = strings.ReplaceAll(mantissa, ".", "")
	}
	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		return "0"
	}
	trimmed := strings.TrimRight(mantissa, "0")
	exp.Add(exp, big.NewInt(int64(len(mantissa)-len(trimmed))))
	return sign + trimmed + "e" + exp.String()
}
func equalJSON(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case string:
		v, ok := b.(string)
		return ok && a == v
	case bool:
		v, ok := b.(bool)
		return ok && a == v
	case json.Number:
		v, ok := b.(json.Number)
		return ok && canonicalNumber(a) == canonicalNumber(v)
	case []any:
		v, ok := b.([]any)
		if !ok || len(a) != len(v) {
			return false
		}
		for i := range a {
			if !equalJSON(a[i], v[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		v, ok := b.(map[string]any)
		if !ok || len(a) != len(v) {
			return false
		}
		for k, x := range a {
			y, exists := v[k]
			if !exists || !equalJSON(x, y) {
				return false
			}
		}
		return true
	}
	return false
}
func equalField(a, b map[string]any, key string) bool {
	x, ok := a[key]
	y, exists := b[key]
	return ok == exists && equalJSON(x, y)
}
func decodeObject(data []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var out map[string]any
	err := d.Decode(&out)
	return out, err
}
func emptyServerValue(v any) bool { return v == nil || v == "" }
func validMetadata(meta map[string]any, ordinary bool) bool {
	for _, k := range []string{"deletionTimestamp", "deletionGracePeriodSeconds", "generateName", "managedFields"} {
		if _, ok := meta[k]; ok {
			return false
		}
	}
	if x, ok := meta["finalizers"]; ok && x != nil {
		v, ok := x.([]any)
		if !ok || len(v) > 0 {
			return false
		}
	}
	if !ordinary {
		return true
	}
	for _, k := range []string{"labels", "annotations"} {
		if x, ok := meta[k]; ok && x != nil {
			m, ok := x.(map[string]any)
			if !ok {
				return false
			}
			for _, v := range m {
				if _, ok := v.(string); !ok {
					return false
				}
			}
		}
	}
	if x, ok := meta["ownerReferences"]; ok && x != nil {
		refs, ok := x.([]any)
		if !ok {
			return false
		}
		controllers := 0
		for _, x := range refs {
			ref, ok := x.(map[string]any)
			if !ok {
				return false
			}
			for _, k := range []string{"apiVersion", "kind", "name", "uid"} {
				v, ok := ref[k].(string)
				if !ok || v == "" {
					return false
				}
			}
			for k, v := range ref {
				switch k {
				case "apiVersion", "kind", "name", "uid":
				case "controller", "blockOwnerDeletion":
					b, ok := v.(bool)
					if !ok {
						return false
					}
					if k == "controller" && b {
						controllers++
					}
				default:
					return false
				}
			}
		}
		if controllers > 1 {
			return false
		}
	}
	return true
}
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, reg Registration, ns, name string, statusOnly bool, corr string, p *contracts.LifecyclePayload) {
	locked := false
	unlock := func() {
		if locked {
			s.mu.Unlock()
			locked = false
		}
	}
	defer unlock()
	isPatch := r.Method == "PATCH"
	reject := func(code int, reason string) { unlock(); p.Status = code; status(w, code, reason) }
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.Limits.BodyBytes))
	if err != nil {
		reject(400, "BadRequest")
		return
	}
	if len(bytes.TrimSpace(data)) == 0 && r.Method == "DELETE" {
		data = []byte(`{}`)
	}
	ct, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == protobufMedia && mediaErr == nil {
		if statusOnly || (r.Method != "POST" && r.Method != "PUT") || !((reg.Key() == "batch/v1/jobs" && reg.Kind == "Job") || (reg.Key() == "v1/configmaps" && reg.Kind == "ConfigMap")) {
			reject(415, "UnsupportedMediaType")
			return
		}
		data, err = decodeNative(data, reg)
		if err != nil {
			reject(400, "BadRequest")
			return
		}
	} else if ct != "application/json" && !(isPatch && ((ct == strategicMedia && deploymentPatch(reg)) || (ct == jsonPatchMedia && configMapPatch(reg)))) && !(r.Method == "DELETE" && r.Header.Get("Content-Type") == "") || mediaErr != nil && r.Header.Get("Content-Type") != "" {
		reject(415, "UnsupportedMediaType")
		return
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var document any
	if err := d.Decode(&document); err != nil {
		reject(400, "BadRequest")
		return
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		reject(400, "BadRequest")
		return
	}
	obj, objectOK := document.(map[string]any)
	if isPatch {
		if (deploymentPatch(reg) && ct != strategicMedia) || (configMapPatch(reg) && ct != jsonPatchMedia) {
			reject(415, "UnsupportedMediaType")
			return
		}
		var operations []any
		if configMapPatch(reg) {
			var ok bool
			operations, ok = document.([]any)
			if !ok || len(operations) > 128 {
				reject(400, "BadRequest")
				return
			}
		} else if !objectOK {
			reject(400, "BadRequest")
			return
		}
		s.mu.Lock()
		locked = true
		var code int
		var reason string
		if configMapPatch(reg) {
			obj, code, reason = s.patchConfigMapLocked(reg.Key()+"/"+ns+"/"+name, operations)
		} else {
			obj, code, reason = s.patchDeploymentLocked(reg.Key()+"/"+ns+"/"+name, obj)
		}
		if code != 0 {
			reject(code, reason)
			return
		}
	} else if !objectOK {
		reject(400, "BadRequest")
		return
	}
	if reg.Key() == "v1/secrets" && r.Method != "DELETE" {
		if reason := validateSecret(obj); reason != "" {
			reject(422, reason)
			return
		}
	}
	if reg.Group == "apps" && r.Method != "DELETE" {
		if _, present := obj["status"]; present {
			reject(422, "UnsupportedStatus")
			return
		}
	}
	if reg.Key() == "v1/configmaps" && obj["immutable"] == true && r.Method != "DELETE" {
		reject(422, "UnsupportedImmutable")
		return
	}
	var meta map[string]any
	if r.Method != "DELETE" {
		if obj["apiVersion"] != reg.APIVersion() || obj["kind"] != reg.Kind {
			reject(identityCode(isPatch), "InvalidIdentity")
			return
		}
		var ok bool
		meta, ok = obj["metadata"].(map[string]any)
		if !ok {
			reject(identityCode(isPatch), "InvalidMetadata")
			return
		}
		bodyName, ok := meta["name"].(string)
		if !ok || !ValidName(bodyName) || (name != "" && name != bodyName) {
			reject(identityCode(isPatch), "InvalidIdentity")
			return
		}
		name = bodyName
		p.Name = name
		if v, ok := meta["namespace"]; ok && v != ns {
			reject(identityCode(isPatch), "InvalidNamespace")
			return
		}
		if _, command := r.Context().Value(commandKey{}).(*commandCall); command && ns != "" && meta["namespace"] != ns {
			reject(422, "InvalidPreconditions")
			return
		}
		if ns != "" {
			meta["namespace"] = ns
		}
		if !validMetadata(meta, !statusOnly) {
			reject(422, "UnsupportedMetadata")
			return
		}
	}
	key := reg.Key() + "/" + ns + "/" + name
	if !locked {
		if _, command := r.Context().Value(commandKey{}).(*commandCall); command {
			for !s.mu.TryLock() {
				select {
				case <-r.Context().Done():
					reject(408, "RequestCancelled")
					return
				case <-time.After(time.Millisecond):
				}
			}
		} else {
			s.mu.Lock()
		}
		locked = true
	}
	if code, reason := s.commandPreconditions(r.Context(), key, obj, meta); code != 0 {
		reject(code, reason)
		return
	}
	code, reason, raw, kind, noop := s.applyLocked(r.Context(), r.Method, reg, key, obj, meta, statusOnly)
	if code >= 400 {
		unlock()
		reject(code, reason)
		if code == 503 {
			s.fail(errors.New("API mutation capacity exhausted"))
		}
		return
	}
	if noop {
		unlock()
		p.Status = 200
		p.State = "unchanged"
		stored, _ := decodeObject(raw)
		p.Version = stored["metadata"].(map[string]any)["resourceVersion"].(string)
		writeJSON(w, 200, raw)
		return
	}
	// applyLocked changes state; history/publication use exactly the same commit lock.
	e := event{reg: reg.Key(), namespace: ns, kind: kind, version: s.version, object: raw}
	s.history = append(s.history, e)
	overflow := false
	for sub := range s.subscriptions {
		if sub.reg == e.reg && (sub.namespace == "" || sub.namespace == ns) {
			select {
			case sub.ch <- e:
			default:
				overflow = true
			}
		}
	}
	unlock()
	p.Status = code
	p.Version = strconv.FormatUint(e.version, 10)
	p.State = "committed"
	eventKind := map[string]string{"ADDED": "resource.created", "MODIFIED": "resource.updated", "DELETED": "resource.deleted"}[kind]
	if statusOnly {
		eventKind = "resource.statusUpdated"
	}
	captured := s.record(eventKind, corr, *p)
	if overflow {
		s.fail(errors.New("API watch delivery capacity exhausted"))
	}
	if !captured || overflow {
		reject(503, "CommittedButSessionFailed")
		return
	}
	if kind == "DELETED" {
		writeJSON(w, 200, map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success", "code": 200})
		return
	}
	writeJSON(w, code, raw)
}

// applyLocked validates before mutation; a failure never advances state or history.
func (s *Server) applyLocked(ctx context.Context, method string, reg Registration, key string, obj, meta map[string]any, statusOnly bool) (int, string, json.RawMessage, string, bool) {
	fail := func(code int, reason string) (int, string, json.RawMessage, string, bool) {
		return code, reason, nil, "", false
	}
	stored, exists := s.objects[key]
	storedRaw := stored.raw
	if method == "POST" && exists {
		return fail(409, "AlreadyExists")
	}
	if method != "POST" && !exists {
		return fail(404, "NotFound")
	}
	var old, oldMeta map[string]any
	if exists {
		old, _ = decodeObject(storedRaw)
		oldMeta = old["metadata"].(map[string]any)
	}
	kind := "ADDED"
	code := 201
	switch method {
	case "POST":
		for _, k := range []string{"uid", "resourceVersion", "creationTimestamp"} {
			if v, ok := meta[k]; ok && !emptyServerValue(v) {
				return fail(422, "ServerOwnedMetadata")
			}
		}
		if v, ok := meta["generation"]; ok && v != nil {
			n, ok := v.(json.Number)
			if !ok || canonicalNumber(n) != "0" {
				return fail(422, "ServerOwnedMetadata")
			}
		}
		if reg.Status {
			delete(obj, "status")
		}
		meta["uid"] = rand.Text()
		meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
		meta["generation"] = json.Number("1")
	case "PUT", "PATCH":
		rv, ok := meta["resourceVersion"].(string)
		if !ok || rv == "" {
			return fail(422, "InvalidResourceVersion")
		}
		if rv != oldMeta["resourceVersion"] {
			return fail(409, "Conflict")
		}
		for _, k := range []string{"uid", "creationTimestamp"} {
			if v, ok := meta[k]; ok && !emptyServerValue(v) && v != oldMeta[k] {
				return fail(422, "ImmutableMetadata")
			}
		}
		if statusOnly {
			replacement, present := obj["status"]
			obj = old
			meta = oldMeta
			if present {
				obj["status"] = replacement
			} else {
				delete(obj, "status")
			}
		} else {
			for _, k := range []string{"uid", "creationTimestamp", "generation"} {
				meta[k] = oldMeta[k]
			}
			if !equalField(obj, old, "spec") {
				n, _ := strconv.ParseUint(string(oldMeta["generation"].(json.Number)), 10, 64)
				meta["generation"] = json.Number(strconv.FormatUint(n+1, 10))
			}
			if reg.Status {
				if v, ok := old["status"]; ok {
					obj["status"] = v
				} else {
					delete(obj, "status")
				}
			}
		}
		// Decode independently: status isolation above intentionally reused the old map.
		original, _ := decodeObject(storedRaw)
		if equalJSON(obj, original) {
			if c, reason := s.commitAllowed(ctx); c != 0 {
				return fail(c, reason)
			}
			return 200, "", storedRaw, "", true
		}
		kind = "MODIFIED"
		code = 200
	case "DELETE":
		if c, reason := validateDelete(obj, oldMeta); c != 0 {
			return fail(c, reason)
		}
		if f, ok := oldMeta["finalizers"].([]any); ok && len(f) > 0 {
			return fail(422, "UnsupportedFinalizers")
		}
		obj = old
		meta = oldMeta
		kind = "DELETED"
		code = 200
	}
	if len(s.history) >= s.Limits.History || (method == "POST" && len(s.objects) >= s.Limits.Resources) {
		return fail(503, "SessionCapacityExceeded")
	}
	meta["resourceVersion"] = strconv.FormatUint(s.version+1, 10)
	raw, _ := json.Marshal(obj)
	if method == "PATCH" && int64(len(raw)) > s.Limits.BodyBytes {
		return fail(422, "InvalidPatch")
	}
	prepared := storeObject(raw)
	if c, reason := s.commitAllowed(ctx); c != 0 {
		return fail(c, reason)
	}
	s.version++
	if method == "DELETE" {
		delete(s.objects, key)
	} else {
		s.objects[key] = prepared
	}
	return code, "", raw, kind, false
}
func validateDelete(options, meta map[string]any) (int, string) {
	for k, v := range options {
		switch k {
		case "apiVersion":
			if v != "v1" && v != "meta.k8s.io/v1" {
				return 400, "InvalidDeleteOptions"
			}
		case "kind":
			if v != "DeleteOptions" {
				return 400, "InvalidDeleteOptions"
			}
		case "gracePeriodSeconds":
			n, ok := v.(json.Number)
			if !ok {
				return 400, "InvalidDeleteOptions"
			}
			count, err := strconv.ParseInt(string(n), 10, 64)
			if err != nil || count < 0 {
				return 400, "InvalidDeleteOptions"
			}
			if count != 0 {
				return 422, "UnsupportedDeleteOptions"
			}
		case "preconditions":
			m, ok := v.(map[string]any)
			if !ok {
				return 400, "InvalidDeleteOptions"
			}
			for field, value := range m {
				if field != "uid" && field != "resourceVersion" {
					return 400, "InvalidDeleteOptions"
				}
				s, ok := value.(string)
				if !ok || s == "" {
					return 400, "InvalidDeleteOptions"
				}
			}
		default:
			return 422, "UnsupportedDeleteOptions"
		}
	}
	if m, ok := options["preconditions"].(map[string]any); ok {
		for field, value := range m {
			if value != meta[field] {
				return 409, "Conflict"
			}
		}
	}
	return 0, ""
}
