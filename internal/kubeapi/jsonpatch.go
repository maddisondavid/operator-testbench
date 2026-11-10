package kubeapi

import (
	"encoding/json"
	"strconv"
	"strings"
)

const jsonPatchMedia = "application/json-patch+json"

func configMapPatch(r Registration) bool { return r.Key() == "v1/configmaps" && r.Kind == "ConfigMap" }
func pointerTokens(path string) ([]string, bool) {
	if path == "" {
		return nil, true
	}
	if !strings.HasPrefix(path, "/") {
		return nil, false
	}
	tokens := strings.Split(path[1:], "/")
	for i, text := range tokens {
		var b strings.Builder
		for j := 0; j < len(text); j++ {
			if text[j] != '~' {
				b.WriteByte(text[j])
				continue
			}
			j++
			if j == len(text) {
				return nil, false
			}
			switch text[j] {
			case '0':
				b.WriteByte('~')
			case '1':
				b.WriteByte('/')
			default:
				return nil, false
			}
		}
		tokens[i] = b.String()
	}
	return tokens, true
}
func patchIndex(s string, n int, appendOK bool) (int, bool) {
	if s == "-" {
		return n, appendOK
	}
	if s == "" || len(s) > 1 && s[0] == '0' {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	v, e := strconv.Atoi(s)
	return v, e == nil && v >= 0 && (v < n || appendOK && v == n)
}
func pointerValue(doc any, path []string) (any, bool) {
	for _, key := range path {
		switch v := doc.(type) {
		case map[string]any:
			var ok bool
			doc, ok = v[key]
			if !ok {
				return nil, false
			}
		case []any:
			i, ok := patchIndex(key, len(v), false)
			if !ok {
				return nil, false
			}
			doc = v[i]
		default:
			return nil, false
		}
	}
	return doc, true
}
func editPointer(doc any, path []string, op string, value any) (any, bool) {
	if len(path) == 0 {
		if op == "remove" {
			return nil, true
		}
		return value, true
	}
	key := path[0]
	switch v := doc.(type) {
	case map[string]any:
		old, exists := v[key]
		if len(path) > 1 {
			if !exists {
				return nil, false
			}
			child, ok := editPointer(old, path[1:], op, value)
			if !ok {
				return nil, false
			}
			v[key] = child
			return v, true
		}
		if op != "add" && !exists {
			return nil, false
		}
		if op == "remove" {
			delete(v, key)
		} else {
			v[key] = value
		}
		return v, true
	case []any:
		index, ok := patchIndex(key, len(v), len(path) == 1 && op == "add")
		if !ok {
			return nil, false
		}
		if len(path) > 1 {
			child, ok := editPointer(v[index], path[1:], op, value)
			if !ok {
				return nil, false
			}
			v[index] = child
			return v, true
		}
		switch op {
		case "add":
			v = append(v, nil)
			copy(v[index+1:], v[index:])
			v[index] = value
		case "replace":
			v[index] = value
		case "remove":
			v = append(v[:index], v[index+1:]...)
		}
		return v, true
	}
	return nil, false
}
func cloneJSON(v any) any {
	switch v := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(v))
		for k, x := range v {
			m[k] = cloneJSON(x)
		}
		return m
	case []any:
		a := make([]any, len(v))
		for i, x := range v {
			a[i] = cloneJSON(x)
		}
		return a
	default:
		return v
	}
}
func pathPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func (s *Server) patchConfigMapLocked(key string, operations []any) (map[string]any, int, string) {
	stored, exists := s.objects[key]
	if !exists {
		return nil, 404, "NotFound"
	}
	obj, _ := decodeObject(stored.raw)
	oldMeta := obj["metadata"].(map[string]any)
	rv := oldMeta["resourceVersion"]
	uid, created := oldMeta["uid"], oldMeta["creationTimestamp"]
	var doc any = obj
	for _, raw := range operations {
		op, ok := raw.(map[string]any)
		if !ok {
			return nil, 400, "BadRequest"
		}
		verb, ok := op["op"].(string)
		if !ok {
			return nil, 400, "BadRequest"
		}
		path, ok := op["path"].(string)
		if !ok {
			return nil, 400, "BadRequest"
		}
		tokens, ok := pointerTokens(path)
		if !ok {
			return nil, 400, "BadRequest"
		}
		value, hasValue := op["value"]
		switch verb {
		case "test":
			if !hasValue {
				return nil, 400, "BadRequest"
			}
			got, exists := pointerValue(doc, tokens)
			if !exists || !equalJSON(got, value) {
				return nil, 422, "PatchTestFailed"
			}
			continue
		case "add", "replace":
			if !hasValue {
				return nil, 400, "BadRequest"
			}
		case "remove":
		case "copy", "move":
			from, valid := op["from"].(string)
			if !valid {
				return nil, 400, "BadRequest"
			}
			source, valid := pointerTokens(from)
			if !valid {
				return nil, 400, "BadRequest"
			}
			value, valid = pointerValue(doc, source)
			if !valid {
				return nil, 422, "InvalidPatch"
			}
			value = cloneJSON(value)
			if verb == "move" {
				if len(tokens) > len(source) && pathPrefix(source, tokens) {
					return nil, 422, "InvalidPatch"
				}
				if pathPrefix(source, []string{"metadata", "resourceVersion"}) && from != path {
					return nil, 422, "InvalidResourceVersion"
				}
				doc, valid = editPointer(doc, source, "remove", nil)
				if !valid {
					return nil, 422, "InvalidPatch"
				}
			}
			verb = "add"
		default:
			return nil, 400, "BadRequest"
		}
		var valid bool
		doc, valid = editPointer(doc, tokens, verb, value)
		if !valid {
			return nil, 422, "InvalidPatch"
		}
		current, ok := doc.(map[string]any)
		if !ok {
			return nil, 422, "InvalidPatch"
		}
		meta, ok := current["metadata"].(map[string]any)
		if !ok {
			return nil, 422, "InvalidMetadata"
		}
		supplied, ok := meta["resourceVersion"].(string)
		if !ok || supplied == "" {
			return nil, 422, "InvalidResourceVersion"
		}
		if supplied != rv {
			return nil, 409, "Conflict"
		}
		encoded, err := json.Marshal(doc)
		if err != nil || int64(len(encoded)) > s.Limits.BodyBytes {
			return nil, 422, "InvalidPatch"
		}
	}
	result, ok := doc.(map[string]any)
	if !ok {
		return nil, 422, "InvalidPatch"
	}
	meta, ok := result["metadata"].(map[string]any)
	if !ok || meta["uid"] != uid || meta["creationTimestamp"] != created {
		return nil, 422, "ImmutableMetadata"
	}
	return result, 0, ""
}
