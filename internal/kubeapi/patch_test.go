package kubeapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

const deployments = "/apis/apps/v1/namespaces/demo/deployments"
const deployment = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"one","annotations":{"keep":"yes"}},"spec":{"replicas":9007199254740993,"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":1}},"template":{"metadata":{"annotations":{"keep":"yes"}},"spec":{"containers":[{"name":"one","image":"old","env":[{"name":"A","value":"keep"}]},{"name":"two","image":"keep"}]}}}}`

func TestDeploymentStrategicPatch(t *testing.T) {
	reg, _ := Builtin("apps/v1/deployments")
	s := serve(t, []Registration{reg}, DefaultLimits(), nil)
	if code, o := request(t, s, "POST", deployments, deployment); code != 201 {
		t.Fatal(code, o)
	}
	patch := func(body string) (int, map[string]any) {
		return wireRequest(t, s, "PATCH", deployments+"/one?fieldManager=Reloader", strategicMedia, "", []byte(body))
	}
	code, o := patch(`{"spec":{"template":{"spec":{"containers":[{"name":"one","image":"new","env":[{"name":"B","value":"added"}]}]}}}}`)
	if code != 200 {
		t.Fatal(code, o)
	}
	spec := o["spec"].(map[string]any)
	if spec["replicas"] != json.Number("9007199254740993") {
		t.Fatal("number rounded", o)
	}
	cs := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	if len(cs) != 2 || len(cs[0].(map[string]any)["env"].([]any)) != 2 || cs[1].(map[string]any)["image"] != "keep" {
		t.Fatal(o)
	}
	_, before := request(t, s, "GET", deployments+"/one", "")
	if code, got := patch(`{}`); code != 200 || !equalJSON(before, got) {
		t.Fatal(code, got)
	}
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"metadata":{"resourceVersion":null}}`, 422}, {`{"metadata":{"resourceVersion":"stale"}}`, 409},
		{`{"metadata":{"resourceVersion":1}}`, 422}, {`{"metadata":null}`, 422}, {`{"metadata":{"uid":null}}`, 422},
		{`{"metadata":{"name":"other"}}`, 422}, {`{"status":{}}`, 422}, {`{"status":null}`, 422}, {`{"metadata":{"namespace":null}}`, 422}, {`{"metadata":{"managedFields":null}}`, 422},
		{`{"spec":{"$bad":1}}`, 422}, {`{"spec":{"$retainKeys":["template"]}}`, 422},
		{`{"spec":{"template":{"spec":{"containers":[{"name":"one","$patch":"bad"}]}}}}`, 422},
		{`{"spec":{"$setElementOrder/noSuchField":[]}}`, 422},
		{`{} {}`, 400}, {`[]`, 400},
	} {
		if code, got := patch(tc.body); code != tc.code {
			t.Fatalf("%s: %d %v", tc.body, code, got)
		}
	}
	_, after := request(t, s, "GET", deployments+"/one", "")
	if !equalJSON(before, after) {
		t.Fatal("rejection mutated", after)
	}
	code, o = patch(`{"spec":{"strategy":{"$retainKeys":["type"],"type":"Recreate"},"template":{"metadata":{"annotations":{"keep":null}},"spec":{"containers":[{"name":"two","$patch":"delete"}]}}}}`)
	if code != 200 {
		t.Fatal(code, o)
	}
	strategy := o["spec"].(map[string]any)["strategy"].(map[string]any)
	if len(strategy) != 1 || strategy["type"] != "Recreate" {
		t.Fatal(o)
	}
	// Independent unconditional patches must both survive, never stale-image replace.
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, o := patch(fmt.Sprintf(`{"metadata":{"annotations":{"writer%d":"yes"}}}`, i))
			if code != 200 {
				t.Error(code, o)
			}
		}()
	}
	wg.Wait()
	_, o = request(t, s, "GET", deployments+"/one", "")
	a := o["metadata"].(map[string]any)["annotations"].(map[string]any)
	if a["writer0"] != "yes" || a["writer1"] != "yes" {
		t.Fatal(o)
	}
	for _, query := range []string{"fieldManager=", "fieldManager=x&fieldManager=y", "fieldManager=" + strings.Repeat("x", 129), "fieldManager=%01", "force=true"} {
		if code, o := wireRequest(t, s, "PATCH", deployments+"/one?"+query, strategicMedia, "", []byte(`{}`)); code != 400 {
			t.Fatal(query, code, o)
		}
	}
	if code, o := wireRequest(t, s, "PATCH", deployments+"/one", "application/json", "", []byte(`{}`)); code != 415 {
		t.Fatal(code, o)
	}
}

func TestWorkloadCatalogAndPatchBounds(t *testing.T) {
	regs := []Registration{}
	for _, k := range []string{"deployments", "daemonsets", "statefulsets"} {
		r, ok := Builtin("apps/v1/" + k)
		if !ok {
			t.Fatal(k)
		}
		regs = append(regs, r)
	}
	s := serve(t, regs, DefaultLimits(), nil)
	for _, reg := range regs {
		path := "/apis/apps/v1/namespaces/demo/" + reg.Plural
		body := strings.ReplaceAll(deployment, "Deployment", reg.Kind)
		if c, o := request(t, s, "POST", path, body); c != 201 {
			t.Fatal(c, o)
		}
		_, o := request(t, s, "GET", path+"/one", "")
		o["status"] = map[string]any{}
		if c, o := request(t, s, "PUT", path+"/one", encoded(o)); c != 422 {
			t.Fatal(c, o)
		}
		if c, o := request(t, s, "GET", path+"/one/status", ""); c != 404 {
			t.Fatal(c, o)
		}
		if reg.Kind != "Deployment" {
			if c, o := wireRequest(t, s, "PATCH", path+"/one", strategicMedia, "", []byte(`{}`)); c != 405 {
				t.Fatal(c, o)
			}
		}
		if c, o := wireRequest(t, s, "PUT", path+"/one", protobufMedia, "", []byte("k8s\x00")); c != 415 {
			t.Fatal(c, o)
		}
	}
	patch := func(body string) (int, map[string]any) {
		return wireRequest(t, s, "PATCH", deployments+"/one", strategicMedia, "", []byte(body))
	}
	// Parallel order preserves both keyed entries, and primitive deletion is only
	// accepted for a merge-strategy primitive field (nested metadata finalizers).
	c, o := patch(`{"spec":{"template":{"spec":{"$setElementOrder/containers":[{"name":"two"},{"name":"one"}]}}}}`)
	if c != 200 {
		t.Fatal(c, o)
	}
	cs := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	if cs[0].(map[string]any)["name"] != "two" {
		t.Fatal(o)
	}
	c, o = patch(`{"spec":{"template":{"metadata":{"finalizers":["a","b"]}}}}`)
	if c != 200 {
		t.Fatal(c, o)
	}
	c, o = patch(`{"spec":{"template":{"metadata":{"$deleteFromPrimitiveList/finalizers":["a"]}}}}`)
	if c != 200 {
		t.Fatal(c, o)
	}
	if encoded(o["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["finalizers"]) != `["b"]` {
		t.Fatal(o)
	}
	c, o = patch(`{"metadata":{"annotations":{"large":"` + strings.Repeat("a", 600000) + `"}}}`)
	if c != 200 {
		t.Fatal(c)
	}
	before := copyObject(o)
	c, o = patch(`{"metadata":{"annotations":{"other":"` + strings.Repeat("b", 600000) + `"}}}`)
	if c != 422 {
		t.Fatal(c)
	}
	_, o = request(t, s, "GET", deployments+"/one", "")
	if !equalJSON(before, o) {
		t.Fatal("oversize mutation")
	}
	// Two effective writers with one explicit RV have exactly one winner.
	rv := o["metadata"].(map[string]any)["resourceVersion"]
	codes := make(chan int, 2)
	for i := range 2 {
		go func() {
			c, _ := patch(fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"annotations":{"race":"%d"}}}`, rv, i))
			codes <- c
		}()
	}
	a, b := <-codes, <-codes
	if !(a == 200 && b == 409 || a == 409 && b == 200) {
		t.Fatal(a, b)
	}
}

func TestOrderDirectivePayloadRejectionIsAtomic(t *testing.T) {
	reg, _ := Builtin("apps/v1/deployments")
	for _, entry := range []string{
		`{"name":"two","$notSupported":"x"}`,
		`{"name":{"bad":"value"}}`,
		`{"name":["two"]}`, `{"name":true}`, `{"name":2}`, `{"name":null}`, `{}`,
		`{"name":"two","nested":{"$notSupported":"x"}}`,
		`{"name":"two","$patch":"delete"}`,
	} {
		t.Run(entry, func(t *testing.T) {
			s := serve(t, []Registration{reg}, DefaultLimits(), nil)
			if c, o := request(t, s, "POST", deployments, deployment); c != 201 {
				t.Fatal(c, o)
			}
			_, before := request(t, s, "GET", deployments+"/one", "")
			s.mu.Lock()
			version, history := s.version, len(s.history)
			s.mu.Unlock()
			body := `{"spec":{"template":{"spec":{"$setElementOrder/containers":[` + entry + `,{"name":"one"}]}}}}`
			c, o := wireRequest(t, s, "PATCH", deployments+"/one", strategicMedia, "", []byte(body))
			if c != 422 {
				t.Fatal(c, o)
			}
			_, after := request(t, s, "GET", deployments+"/one", "")
			if !equalJSON(before, after) {
				t.Fatal("invalid order changed state")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.version != version || len(s.history) != history {
				t.Fatal("invalid order changed version/history")
			}
		})
	}
}
