package kubeapi

import (
	"encoding/json"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
)

// The upstream retained-key defect must never become API replacement behavior.
func TestReplicatorBothMapReplacementForEachWire(t *testing.T) {
	for _, media := range []string{"application/json", protobufMedia} {
		t.Run(media, func(t *testing.T) {
			reg, _ := Builtin("v1/configmaps")
			s := serve(t, []Registration{reg}, DefaultLimits(), nil)
			path := "/api/v1/namespaces/example/configmaps"
			object := &core.ConfigMap{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: meta.ObjectMeta{Name: "example", Namespace: "example"}, Data: map[string]string{"message": "initial", "remove": "old"}, BinaryData: map[string][]byte{"bytes": {0, 255}, "remove-binary": {3}}}
			encode := func() []byte {
				if media == protobufMedia {
					return protobufConfigMap(t, object)
				}
				b, e := json.Marshal(object)
				if e != nil {
					t.Fatal(e)
				}
				return b
			}
			code, created := wireRequest(t, s, "POST", path, media, "", encode())
			if code != 201 {
				t.Fatal(code, created)
			}
			bytes, _ := json.Marshal(created)
			if e := json.Unmarshal(bytes, object); e != nil {
				t.Fatal(e)
			}
			oldRV := object.ResourceVersion
			uid := object.UID
			object.Data = map[string]string{"message": "updated"}
			object.BinaryData = map[string][]byte{"bytes": {1, 2}}
			code, updated := wireRequest(t, s, "PUT", path+"/example", media, "", encode())
			if code != 200 {
				t.Fatal(code, updated)
			}
			if !equalJSON(updated["data"], map[string]any{"message": "updated"}) || !equalJSON(updated["binaryData"], map[string]any{"bytes": "AQI="}) {
				t.Fatal(updated)
			}
			m := updated["metadata"].(map[string]any)
			if m["uid"] != string(uid) || m["resourceVersion"] == oldRV {
				t.Fatal(m)
			}
			if code, _ = wireRequest(t, s, "PUT", path+"/example", media, "", encode()); code != 409 {
				t.Fatal("stale", code)
			}
			object.ResourceVersion = m["resourceVersion"].(string)
			code, noOp := wireRequest(t, s, "PUT", path+"/example", media, "", encode())
			if code != 200 || !equalJSON(noOp, updated) {
				t.Fatal(code, noOp)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.history) != 2 || s.history[0].kind != "ADDED" || s.history[1].kind != "MODIFIED" {
				t.Fatal("unexpected mutation history", s.history)
			}
		})
	}
}
