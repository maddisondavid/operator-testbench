package kubeapi

import (
	"bytes"
	"context"
	"encoding/json"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"net/http"
	"testing"
	"time"
)

func protobufConfigMap(t *testing.T, cm *corev1.ConfigMap) []byte {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	var buf bytes.Buffer
	if e := protobuf.NewSerializer(scheme, scheme).Encode(cm, &buf); e != nil {
		t.Fatal(e)
	}
	return buf.Bytes()
}
func TestCoreWireAndReplacement(t *testing.T) {
	cmreg, _ := Builtin("v1/configmaps")
	nsreg, _ := Builtin("v1/namespaces")
	s := serve(t, []Registration{cmreg, nsreg}, DefaultLimits(), nil)
	path := "/api/v1/namespaces/source/configmaps"
	cm := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "status", Namespace: "source", Annotations: map[string]string{"keep": "yes", "remove": "yes"}}, Data: map[string]string{"keep": "one", "remove": "old"}, BinaryData: map[string][]byte{"keep": {0, 255}, "remove": {1}}}
	code, created := wireRequest(t, s, "POST", path, protobufMedia, "application/vnd.kubernetes.protobuf,application/json", protobufConfigMap(t, cm))
	if code != 201 {
		t.Fatal(code, created)
	}
	s2 := serve(t, []Registration{cmreg}, DefaultLimits(), nil)
	raw, _ := json.Marshal(cm)
	code, other := wireRequest(t, s2, "POST", path, "application/json", "", raw)
	if code != 201 {
		t.Fatal(code, other)
	}
	for _, o := range []map[string]any{created, other} {
		m := o["metadata"].(map[string]any)
		delete(m, "uid")
		delete(m, "creationTimestamp")
	}
	if !equalJSON(created, other) {
		t.Fatal(created, other)
	}
	_, current := request(t, s, "GET", path+"/status", "")
	raw, _ = json.Marshal(current)
	_ = json.Unmarshal(raw, cm)
	code, noOp := wireRequest(t, s, "PUT", path+"/status", protobufMedia, "", protobufConfigMap(t, cm))
	if code != 200 || !equalJSON(current, noOp) {
		t.Fatal(code, noOp)
	}
	cm.Data = map[string]string{"keep": "two"}
	cm.BinaryData = map[string][]byte{"keep": {2, 3}}
	cm.Annotations = map[string]string{"keep": "new"}
	code, updated := wireRequest(t, s, "PUT", path+"/status", protobufMedia, "", protobufConfigMap(t, cm))
	if code != 200 || len(updated["data"].(map[string]any)) != 1 || len(updated["binaryData"].(map[string]any)) != 1 || len(updated["metadata"].(map[string]any)["annotations"].(map[string]any)) != 1 {
		t.Fatal(code, updated)
	}
	if updated["metadata"].(map[string]any)["uid"] != current["metadata"].(map[string]any)["uid"] {
		t.Fatal("UID changed")
	}
	if code, _ = wireRequest(t, s, "PUT", path+"/status", protobufMedia, "", protobufConfigMap(t, cm)); code != 409 {
		t.Fatal(code)
	}
	_, before := request(t, s, "GET", "/api/v1/configmaps", "")
	yes := true
	bad := cm.DeepCopy()
	bad.Immutable = &yes
	bad.ResourceVersion = updated["metadata"].(map[string]any)["resourceVersion"].(string)
	wrong := cm.DeepCopy()
	wrong.Kind = "Namespace"
	for _, tc := range []struct {
		method, path, accept string
		data                 []byte
		code                 int
	}{
		{"PUT", path + "/status", "", protobufConfigMap(t, bad), 422},
		{"POST", path, "", protobufConfigMap(t, wrong), 400},
		{"POST", path, "", []byte("k8s\\x00truncated"), 400},
		{"POST", path, "application/json;q=0", protobufConfigMap(t, cm), 406},
		{"POST", "/api/v1/namespaces", "", protobufConfigMap(t, cm), 415},
		{"PUT", path + "/status/status", "", protobufConfigMap(t, cm), 404},
		{"DELETE", path + "/status", "", protobufConfigMap(t, cm), 415},
	} {
		if c, o := wireRequest(t, s, tc.method, tc.path, protobufMedia, tc.accept, tc.data); c != tc.code {
			t.Fatal(tc, c, o)
		}
	}
	_, after := request(t, s, "GET", "/api/v1/configmaps", "")
	if !equalJSON(before, after) {
		t.Fatal("invalid writes mutated state/history")
	}
	for _, p := range []string{"/api", "/api/v1"} {
		if c, _ := request(t, s, "GET", p, ""); c != 200 {
			t.Fatal(p, c)
		}
	}
	if c, _ := request(t, s, "POST", "/api/v1/namespaces", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"source"}}`); c != 201 {
		t.Fatal(c)
	}
	if c, _ := request(t, s, "GET", "/api/v1/namespaces/source", ""); c != 200 {
		t.Fatal(c)
	}
	if c, _ := request(t, s, "GET", "/api/v1/namespaces/source/namespaces", ""); c != 404 {
		t.Fatal(c)
	}
}
func TestExplicitCoreWatchScope(t *testing.T) {
	cm, _ := Builtin("v1/configmaps")
	ns, _ := Builtin("v1/namespaces")
	s := serve(t, []Registration{cm, ns}, DefaultLimits(), nil)
	s.RequireAllNamespaces = true
	open := func(list, watch string) func() {
		t.Helper()
		_, o := request(t, s, "GET", list, "")
		rv := o["metadata"].(map[string]any)["resourceVersion"].(string)
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+watch+"?watch=true&resourceVersion="+rv, nil)
		res, e := http.DefaultClient.Do(req)
		if e != nil || res.StatusCode != 200 {
			t.Fatal(e, res)
		}
		return func() { cancel(); res.Body.Close() }
	}
	n := open("/api/v1/namespaces", "/api/v1/namespaces")
	defer n()
	scoped := open("/api/v1/namespaces/source/configmaps", "/api/v1/namespaces/source/configmaps")
	defer scoped()
	select {
	case <-s.Connected:
		t.Fatal("scoped watch connected")
	case <-time.After(20 * time.Millisecond):
	}
	mismatched := open("/api/v1/namespaces/source/configmaps", "/api/v1/configmaps")
	defer mismatched()
	select {
	case <-s.Connected:
		t.Fatal("scoped LIST connected global WATCH")
	case <-time.After(20 * time.Millisecond):
	}
	full := open("/api/v1/configmaps", "/api/v1/configmaps")
	defer full()
	select {
	case <-s.Connected:
	case <-time.After(time.Second):
		t.Fatal("global pair not connected")
	}
}
