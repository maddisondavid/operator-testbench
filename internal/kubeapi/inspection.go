package kubeapi

import (
	"context"
	"encoding/json"
	"operator-dynamo/internal/contracts"
	"sort"
	"strings"
	"time"
)

// storedObject is one authoritative immutable API record. Summary fields are
// extracted at publication, so diagnostic collections need not retain 256 MiB
// of object bodies while concurrent mutations replace their contents.
type storedObject struct {
	raw     json.RawMessage
	summary contracts.ObjectSummary
}

func storeObject(raw json.RawMessage) storedObject {
	var v struct {
		Metadata contracts.ObjectSummary `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &v)
	return storedObject{raw: raw, summary: v.Metadata}
}

// rawSnapshotLocked is shared by public GET/LIST and diagnostic reads. Stored
// JSON buffers are immutable after publication; no map or mutable slice escapes.
func (s *Server) rawSnapshotLocked(reg Registration, ns, name string) ([]json.RawMessage, bool) {
	if name != "" {
		raw, ok := s.objects[reg.Key()+"/"+ns+"/"+name]
		if !ok {
			return nil, false
		}
		return []json.RawMessage{raw.raw}, true
	}
	keys := []string{}
	prefix := reg.Key() + "/"
	if ns != "" {
		prefix += ns + "/"
	}
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	items := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		items = append(items, s.objects[key].raw)
	}
	return items, true
}
func (s *Server) InspectionTypes(ctx context.Context) ([]contracts.ResourceType, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	out := make([]contracts.ResourceType, 0, len(s.Registrations))
	for _, r := range s.Registrations {
		out = append(out, contracts.ResourceType{APIVersion: r.APIVersion(), Kind: r.Kind, Plural: r.Plural, Scope: r.Scope})
	}
	return out, nil
}
func (s *Server) Inspect(ctx context.Context, av, kind, ns, name string) (contracts.ResourceSnapshot, error) {
	var result contracts.ResourceSnapshot
	parts := strings.Split(av, "/")
	if len(parts) < 1 || len(parts) > 2 || len(av) > 320 || len(kind) > 253 || !kindName.MatchString(kind) {
		return result, contracts.ReadFailure(400, "invalid_type")
	}
	for i, p := range parts {
		if !ValidName(p) || (i == len(parts)-1 && strings.Contains(p, ".")) {
			return result, contracts.ReadFailure(400, "invalid_type")
		}
	}
	var reg Registration
	found := false
	for _, r := range s.Registrations {
		if r.APIVersion() == av && r.Kind == kind {
			reg = r
			found = true
			break
		}
	}
	if !found {
		return result, contracts.ReadFailure(404, "type_not_found")
	}
	if reg.Scope == "Namespaced" && !ValidName(ns) || reg.Scope == "Cluster" && ns != "" || name != "" && !ValidName(name) {
		return result, contracts.ReadFailure(400, "invalid_scope")
	}
	if reg.Key() == "v1/secrets" && name != "" {
		return result, contracts.ReadFailure(403, "secret_inspection_disabled")
	}
	for !s.mu.TryLock() {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	if e := ctx.Err(); e != nil {
		s.mu.Unlock()
		return result, e
	}
	if name != "" {
		stored, ok := s.objects[reg.Key()+"/"+ns+"/"+name]
		s.mu.Unlock()
		if !ok {
			return result, contracts.ReadFailure(404, "object_not_found")
		}
		result.ObjectJSON = string(stored.raw)
		result.UID = stored.summary.UID
		result.ResourceVersion = stored.summary.ResourceVersion
		return result, nil
	}
	result.Items = []contracts.ObjectSummary{}
	prefix := reg.Key() + "/"
	if ns != "" {
		prefix += ns + "/"
	}
	for key, stored := range s.objects {
		if strings.HasPrefix(key, prefix) {
			result.Items = append(result.Items, stored.summary)
		}
	}
	s.mu.Unlock()
	sort.Slice(result.Items, func(i, j int) bool {
		a, b := result.Items[i], result.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.UID < b.UID
	})
	return result, nil
}
