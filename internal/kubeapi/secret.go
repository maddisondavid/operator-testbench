package kubeapi

import "encoding/base64"

// Secret values exist only on the ordinary API path. Failures contain fixed
// reasons; inspection uses ObjectSummary and refuses body reads.
func validateSecret(obj map[string]any) string {
	for _, k := range []string{"stringData", "status"} {
		if _, ok := obj[k]; ok {
			return "UnsupportedSecret"
		}
	}
	if obj["immutable"] == true {
		return "UnsupportedImmutable"
	}
	if v, ok := obj["type"]; ok {
		if v != "Opaque" {
			return "UnsupportedSecret"
		}
	} else {
		obj["type"] = "Opaque"
	}
	if data, ok := obj["data"]; ok && data != nil {
		m, ok := data.(map[string]any)
		if !ok {
			return "InvalidSecret"
		}
		for _, v := range m {
			text, ok := v.(string)
			if !ok {
				return "InvalidSecret"
			}
			if _, err := base64.StdEncoding.DecodeString(text); err != nil {
				return "InvalidSecret"
			}
		}
	}
	return ""
}
