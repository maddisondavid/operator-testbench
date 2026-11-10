package kubeapi

import (
	"encoding/json"
	"fmt"
	"mime"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
)

const protobufMedia = "application/vnd.kubernetes.protobuf"

// Only the routed pinned native representation is registered; no defaults/conversion.
func decodeNative(data []byte, reg Registration) ([]byte, error) {
	expected := schema.GroupVersionKind{Group: reg.Group, Version: reg.Version, Kind: reg.Kind}
	var native runtime.Object
	switch reg.Key() {
	case "batch/v1/jobs":
		native = &batchv1.Job{}
	case "v1/configmaps":
		native = &corev1.ConfigMap{}
	default:
		return nil, fmt.Errorf("unsupported protobuf resource")
	}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(expected, native)
	obj, gvk, err := protobuf.NewSerializer(scheme, scheme).Decode(data, nil, nil)
	if err != nil {
		return nil, err
	}
	if gvk == nil || *gvk != expected {
		return nil, fmt.Errorf("invalid native envelope")
	}
	obj.GetObjectKind().SetGroupVersionKind(expected)
	return json.Marshal(obj)
}

// Most-specific matching range controls JSON quality, including explicit q=0.
func acceptsJSON(header string) bool {
	if strings.TrimSpace(header) == "" {
		return true
	}
	best, quality := -1, 0.0
	for _, part := range strings.Split(header, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if raw, exists := params["q"]; exists {
			q, err = strconv.ParseFloat(raw, 64)
			if err != nil || q < 0 || q > 1 {
				continue
			}
		}
		supported := true
		for k := range params {
			if k != "q" {
				supported = false
			}
		}
		if !supported {
			continue
		}
		specificity := -1
		switch media {
		case "*/*":
			specificity = 0
		case "application/*":
			specificity = 1
		case "application/json":
			specificity = 2
		}
		if specificity > best {
			best, quality = specificity, q
		} else if specificity == best && q > quality {
			quality = q
		}
	}
	return best >= 0 && quality > 0
}
