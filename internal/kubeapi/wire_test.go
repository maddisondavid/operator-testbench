package kubeapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
)

func protobufJob(t *testing.T, job *batchv1.Job) []byte {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = batchv1.AddToScheme(scheme)
	var buf bytes.Buffer
	if err := protobuf.NewSerializer(scheme, scheme).Encode(job, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func wireRequest(t *testing.T, s *Server, method, path, ct, accept string, data []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+s.Address()+path, bytes.NewReader(data))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", accept)
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatal(resp.Header)
	}
	var obj map[string]any
	d := json.NewDecoder(resp.Body)
	d.UseNumber()
	if err := d.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, obj
}
func TestJobWireSemanticsAndRejections(t *testing.T) {
	reg := Registration{Group: "batch", Version: "v1", Kind: "Job", ListKind: "JobList", Plural: "jobs", Singular: "job", Scope: "Namespaced"}
	s := serve(t, []Registration{reg, statusWidget()}, DefaultLimits(), nil)
	path := "/apis/batch/v1/namespaces/demo/jobs"
	n := int64(9007199254740993)
	yes := true
	job := &batchv1.Job{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}, ObjectMeta: metav1.ObjectMeta{Name: "wire", Namespace: "demo", OwnerReferences: []metav1.OwnerReference{{APIVersion: "example.test/v1", Kind: "Widget", Name: "owner", UID: "owner-uid", Controller: &yes}}}, Spec: batchv1.JobSpec{ActiveDeadlineSeconds: &n}}
	body := protobufJob(t, job)
	code, created := wireRequest(t, s, "POST", path, protobufMedia, "application/vnd.kubernetes.protobuf,application/json", body)
	if code != 201 {
		t.Fatal(code, created)
	}
	if created["spec"].(map[string]any)["activeDeadlineSeconds"] != json.Number("9007199254740993") {
		t.Fatal(created)
	}
	// Typed protobuf normalization must equal the same typed Job serialized as JSON.
	s2 := serve(t, []Registration{reg}, DefaultLimits(), nil)
	jsonBody, _ := json.Marshal(job)
	code, other := wireRequest(t, s2, "POST", path, "application/json", "", jsonBody)
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
	_, current := request(t, s, "GET", path+"/wire", "")
	raw, _ := json.Marshal(current)
	if err := json.Unmarshal(raw, job); err != nil {
		t.Fatal(err)
	}
	code, noop := wireRequest(t, s, "PUT", path+"/wire", protobufMedia, "*/*", protobufJob(t, job))
	if code != 200 || !equalJSON(current, noop) {
		t.Fatal(code, noop, current)
	}
	job.Labels = map[string]string{"changed": "true"}
	code, updated := wireRequest(t, s, "PUT", path+"/wire", protobufMedia, "application/json", protobufJob(t, job))
	if code != 200 || updated["metadata"].(map[string]any)["resourceVersion"] == current["metadata"].(map[string]any)["resourceVersion"] {
		t.Fatal(code, updated)
	}
	code, _ = wireRequest(t, s, "PUT", path+"/wire", protobufMedia, "", protobufJob(t, job))
	if code != 409 {
		t.Fatal(code)
	}
	// Every failed request below leaves current state and LIST version unchanged.
	_, before := request(t, s, "GET", path, "")
	wrong := job.DeepCopy()
	wrong.Kind = "CronJob"
	missing := job.DeepCopy()
	missing.APIVersion = ""
	missing.Kind = ""
	cases := []struct {
		method, path, ct, accept string
		body                     []byte
		want                     int
	}{
		{"POST", path, protobufMedia, "", []byte("bad"), 400},
		{"POST", path, protobufMedia, "", body[:len(body)/2], 400},
		{"POST", path, protobufMedia, "", protobufJob(t, wrong), 400},
		{"POST", path, protobufMedia, "", protobufJob(t, missing), 400},
		{"POST", collection, protobufMedia, "", body, 415},
		{"DELETE", path + "/wire", protobufMedia, "", body, 415},
		{"PUT", collection + "/wire/status", protobufMedia, "", body, 415},
		{"PUT", path + "/wire/status", protobufMedia, "", body, 404},
		{"PATCH", path + "/wire", protobufMedia, "", body, 405},
		{"POST", path, protobufMedia, protobufMedia, body, 406},
		{"POST", path, protobufMedia, "application/json;q=0,*/*;q=1", body, 406},
		{"POST", path, protobufMedia, "application/*;q=0,*/*;q=1", body, 406},
		{"POST", path, protobufMedia, "application/json;as=Table", body, 406},
		{"POST", path, protobufMedia, "", bytes.Repeat([]byte("a"), (1<<20)+1), 400},
	}
	for _, tc := range cases {
		code, obj := wireRequest(t, s, tc.method, tc.path, tc.ct, tc.accept, tc.body)
		if code != tc.want {
			t.Fatalf("%s %s want%d got%d %v", tc.method, tc.path, tc.want, code, obj)
		}
	}
	_, after := request(t, s, "GET", path, "")
	if !equalJSON(before, after) {
		t.Fatal("failure changed state/version", before, after)
	}
}
func TestJSONAccept(t *testing.T) {
	for _, v := range []string{"", "*/*", "application/*", "application/json", "application/vnd.kubernetes.protobuf, application/json", "application/json;q=0.3,*/*;q=0", "application/json;q=1,application/*;q=0"} {
		if !acceptsJSON(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"text/plain", "application/json;q=0", "application/json;q=0,*/*", "application/*;q=0,*/*", "application/json;as=Table", "application/json;q=NaN"} {
		if acceptsJSON(v) {
			t.Fatal(v)
		}
	}
}

func TestRepeatedAcceptBeforeMutation(t *testing.T) {
	s := serve(t, []Registration{widget}, DefaultLimits(), nil)
	_, obj := request(t, s, "POST", collection, object("existing"))
	_, before := request(t, s, "GET", collection, "")
	for _, method := range []string{"POST", "PUT"} {
		path := collection
		body := object("rejected")
		if method == "PUT" {
			path += "/existing"
			obj["spec"] = map[string]any{"changed": true}
			body = encoded(obj)
		}
		req, _ := http.NewRequest(method, "http://"+s.Address()+path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Add("Accept", "*/*;q=1")
		req.Header.Add("Accept", "application/json;q=0")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 406 {
			t.Fatal(res.StatusCode)
		}
	}
	_, after := request(t, s, "GET", collection, "")
	if !equalJSON(before, after) {
		t.Fatal("negotiation committed", before, after)
	}
}
