package kubeapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

const strategicMedia = "application/strategic-merge-patch+json"

func deploymentPatch(r Registration) bool {
	return r.Key() == "apps/v1/deployments" && r.Kind == "Deployment"
}
func identityCode(patch bool) int {
	if patch {
		return 422
	}
	return 400
}
func mutationQueryValid(s *Server, r *http.Request, reg Registration, name string, statusOnly bool) bool {
	if r.Method != "PATCH" {
		return s.queryValid(r, name) && !((r.Method != "GET" || statusOnly) && r.URL.RawQuery != "")
	}
	if configMapPatch(reg) && !statusOnly && name != "" {
		return r.URL.RawQuery == ""
	}
	if !deploymentPatch(reg) || statusOnly || name == "" {
		return false
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	for k, v := range q {
		if k != "fieldManager" || len(v) != 1 || len(v[0]) == 0 || len(v[0]) > 128 {
			return false
		}
		for _, c := range []byte(v[0]) {
			if c < 32 || c > 126 {
				return false
			}
		}
	}
	return true
}

// The pinned map API operates directly on UseNumber values: no typed-object or
// float64 marshal round trip can erase unrelated fields or round integers.
// The caller holds the same commit lock through validation and publication.
func (s *Server) patchDeploymentLocked(key string, patch map[string]any) (map[string]any, int, string) {
	stored, ok := s.objects[key]
	if !ok {
		return nil, 404, "NotFound"
	}
	old, _ := decodeObject(stored.raw)
	oldMeta := old["metadata"].(map[string]any)
	if _, present := patch["status"]; present {
		return nil, 422, "UnsupportedStatus"
	}
	if meta, ok := patch["metadata"].(map[string]any); ok {
		for _, k := range []string{"deletionTimestamp", "deletionGracePeriodSeconds", "generateName", "managedFields"} {
			if _, present := meta[k]; present {
				return nil, 422, "UnsupportedMetadata"
			}
		}
		if value, present := meta["namespace"]; present && value != oldMeta["namespace"] {
			return nil, 422, "InvalidNamespace"
		}
		if rv, present := meta["resourceVersion"]; present {
			v, ok := rv.(string)
			if !ok || v == "" {
				return nil, 422, "InvalidResourceVersion"
			}
			if v != oldMeta["resourceVersion"] {
				return nil, 409, "Conflict"
			}
		}
		for _, k := range []string{"uid", "creationTimestamp"} {
			if v, present := meta[k]; present && v != oldMeta[k] {
				return nil, 422, "ImmutableMetadata"
			}
		}
	}
	schema, _ := strategicpatch.NewPatchMetaFromStruct(appsv1.Deployment{})
	if err := validateDirectives(patch, schema, nil); err != nil {
		return nil, 422, "InvalidPatch"
	}
	merged, err := strategicpatch.StrategicMergeMapPatchUsingLookupPatchMeta(old, patch, schema)
	if err != nil || merged == nil {
		return nil, 422, "InvalidPatch"
	}
	meta, ok := merged["metadata"].(map[string]any)
	if !ok {
		return nil, 422, "InvalidMetadata"
	}
	rv, ok := meta["resourceVersion"].(string)
	if !ok || rv == "" {
		return nil, 422, "InvalidResourceVersion"
	}
	encoded, err := json.Marshal(merged)
	if err != nil || int64(len(encoded)) > s.Limits.BodyBytes {
		return nil, 422, "InvalidPatch"
	}
	return merged, 0, ""
}

func hasStrategy(strategies []string, name string) bool {
	for _, s := range strategies {
		if s == name {
			return true
		}
	}
	return false
}

var errDirective = errors.New("invalid strategic directive")

// Validate even branches newly introduced by a patch: the upstream merger may
// simply copy such branches, otherwise silently dropping invalid directives.
func validateDirectives(m map[string]any, schema strategicpatch.LookupPatchMeta, strategies []string) error {
	for k, v := range m {
		if strings.HasPrefix(k, "$") {
			switch {
			case k == "$patch":
				if v != "merge" && v != "replace" && v != "delete" {
					return errDirective
				}
			case k == "$retainKeys":
				list, ok := v.([]any)
				if !ok || !hasStrategy(strategies, "retainKeys") {
					return errDirective
				}
				for _, x := range list {
					if _, ok := x.(string); !ok {
						return errDirective
					}
				}
			case strings.HasPrefix(k, "$setElementOrder/") || strings.HasPrefix(k, "$deleteFromPrimitiveList/"):
				_, field, _ := strings.Cut(k, "/")
				sub, meta, err := schema.LookupPatchMetadataForSlice(field)
				if err != nil || !hasStrategy(meta.GetPatchStrategies(), "merge") {
					return errDirective
				}
				list, ok := v.([]any)
				if !ok {
					return errDirective
				}
				if strings.HasPrefix(k, "$deleteFromPrimitiveList/") && meta.GetPatchMergeKey() != "" {
					return errDirective
				}
				for _, x := range list {
					if meta.GetPatchMergeKey() != "" {
						item, ok := x.(map[string]any)
						if !ok || containsDirective(item) {
							return errDirective
						}
						if !validMergeKey(sub, meta.GetPatchMergeKey(), item[meta.GetPatchMergeKey()]) {
							return errDirective
						}
					} else {
						switch x.(type) {
						case string, json.Number, bool:
						default:
							return errDirective
						}
					}
				}
			default:
				return errDirective
			}
			continue
		}
		switch child := v.(type) {
		case map[string]any:
			sub, meta, err := schema.LookupPatchMetadataForStruct(k)
			if err != nil {
				if containsDirective(child) {
					return errDirective
				}
				continue
			}
			if err := validateDirectives(child, sub, meta.GetPatchStrategies()); err != nil {
				return err
			}
		case []any:
			sub, meta, err := schema.LookupPatchMetadataForSlice(k)
			for _, x := range child {
				if obj, ok := x.(map[string]any); ok {
					if err == nil && hasStrategy(meta.GetPatchStrategies(), "merge") && meta.GetPatchMergeKey() != "" {
						marker, _ := obj["$patch"].(string)
						if marker == "merge" {
							return errDirective
						}
						if marker != "replace" {
							switch obj[meta.GetPatchMergeKey()].(type) {
							case string, json.Number:
							default:
								return errDirective
							}
						}
					}
					if err != nil {
						if containsDirective(obj) {
							return errDirective
						}
						continue
					}
					if err := validateDirectives(obj, sub, meta.GetPatchStrategies()); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
func containsDirective(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if strings.HasPrefix(k, "$") || containsDirective(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if containsDirective(v) {
				return true
			}
		}
	}
	return false
}

// Order entries identify typed merge keys, not arbitrary comparable JSON values.
func validMergeKey(schema strategicpatch.LookupPatchMeta, key string, value any) bool {
	field, _, err := schema.LookupPatchMetadataForStruct(key)
	if err != nil {
		return false
	}
	typed, ok := field.(strategicpatch.PatchMetaFromStruct)
	if !ok {
		return false
	}
	switch typed.T.Kind() {
	case reflect.String:
		_, ok := value.(string)
		return ok
	case reflect.Int, reflect.Int32, reflect.Int64:
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := strconv.ParseInt(string(n), 10, typed.T.Bits())
		return err == nil
	}
	return false
}
