package kubeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strings"
	"sync"
	"testing"
	"time"
)

func encoded(o any) string { b, _ := json.Marshal(o); return string(b) }
func copyObject(o map[string]any) map[string]any {
	b, _ := json.Marshal(o)
	x, _ := decodeObject(b)
	return x
}
func statusWidget() Registration { r := widget; r.Status = true; return r }
func TestStatusReplacementGenerationAndNoops(t *testing.T) {
	s := serve(t, []Registration{statusWidget()}, DefaultLimits(), nil)
	body := strings.Replace(object("one"), `"name":"one"`, `"name":"one","creationTimestamp":null,"uid":"","resourceVersion":null`, 1)
	var input map[string]any
	json.Unmarshal([]byte(body), &input)
	input["status"] = map[string]any{"injected": true}
	code, current := request(t, s, "POST", collection, encoded(input))
	if code != 201 {
		t.Fatal(code, current)
	}
	if _, ok := current["status"]; ok {
		t.Fatal("CREATE status not discarded")
	}
	m := current["metadata"].(map[string]any)
	if m["generation"] != json.Number("1") {
		t.Fatal(m)
	}
	uid, creation := m["uid"], m["creationTimestamp"]
	change := copyObject(current)
	change["status"] = map[string]any{"value": json.Number("9007199254740993")}
	change["spec"] = map[string]any{"wrong": true}
	change["metadata"].(map[string]any)["labels"] = map[string]any{"wrong": "ignored"}
	code, current = request(t, s, "PUT", collection+"/one/status", encoded(change))
	if code != 200 || current["spec"].(map[string]any)["number"] != "schema-invalid-marker" {
		t.Fatal(code, current)
	}
	if _, ok := current["metadata"].(map[string]any)["labels"]; ok {
		t.Fatal("status changed metadata")
	}
	originalRV := current["metadata"].(map[string]any)["resourceVersion"]
	noop := copyObject(current)
	noop["status"].(map[string]any)["value"] = json.Number("9007199254740993.000")
	code, unchanged := request(t, s, "PUT", collection+"/one/status", encoded(noop))
	if code != 200 || unchanged["metadata"].(map[string]any)["resourceVersion"] != originalRV || unchanged["status"].(map[string]any)["value"] != json.Number("9007199254740993") {
		t.Fatal(code, unchanged)
	}
	root := copyObject(current)
	delete(root["metadata"].(map[string]any), "uid")
	delete(root["metadata"].(map[string]any), "creationTimestamp")
	root["metadata"].(map[string]any)["generation"] = json.Number("999")
	root["spec"] = map[string]any{"number": json.Number("1")}
	root["status"] = nil
	code, current = request(t, s, "PUT", collection+"/one", encoded(root))
	if code != 200 {
		t.Fatal(code, current)
	}
	m = current["metadata"].(map[string]any)
	if m["uid"] != uid || m["creationTimestamp"] != creation || m["generation"] != json.Number("2") || current["status"] == nil {
		t.Fatal(current)
	}
	noop = copyObject(current)
	noop["spec"].(map[string]any)["number"] = json.Number("1.0")
	code, unchanged = request(t, s, "PUT", collection+"/one", encoded(noop))
	if code != 200 || unchanged["metadata"].(map[string]any)["resourceVersion"] != m["resourceVersion"] {
		t.Fatal(code, unchanged)
	}
	code, _ = request(t, s, "PUT", collection+"/one/status", encoded(change))
	if code != 409 {
		t.Fatal("stale write", code)
	}
	for _, field := range []string{"uid", "creationTimestamp"} {
		bad := copyObject(current)
		bad["metadata"].(map[string]any)[field] = "wrong"
		code, _ = request(t, s, "PUT", collection+"/one/status", encoded(bad))
		if code != 422 {
			t.Fatal(field, code)
		}
	}
	for _, field := range []string{"finalizers", "deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "generateName"} {
		bad := copyObject(current)
		bad["metadata"].(map[string]any)[field] = []any{"unsupported"}
		code, _ = request(t, s, "PUT", collection+"/one/status", encoded(bad))
		if code != 422 {
			t.Fatal(field, code)
		}
	}
	for _, rv := range []any{nil, "", json.Number("1")} {
		bad := copyObject(current)
		bad["metadata"].(map[string]any)["resourceVersion"] = rv
		code, _ = request(t, s, "PUT", collection+"/one", encoded(bad))
		if code != 422 {
			t.Fatal(rv, code)
		}
	}
	clear := copyObject(current)
	delete(clear, "status")
	code, current = request(t, s, "PUT", collection+"/one/status", encoded(clear))
	if code != 200 {
		t.Fatal(code, current)
	}
	if _, ok := current["status"]; ok {
		t.Fatal("status omission did not clear")
	}
	_, list := request(t, s, "GET", collection, "")
	if len(list["items"].([]any)) != 1 {
		t.Fatal("LIST contains history", list)
	}
}
func TestExactJSONStructuralEquality(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{{`1`, `1.0`, true}, {`-0`, `0e999999999999999999999`, true}, {`1e1000000000`, `10e999999999`, true}, {`9007199254740993`, `9007199254740992`, false}, {`{"x":1,"y":2}`, `{"y":2,"x":1.0}`, true}, {`[1,2]`, `[2,1]`, false}, {`{}`, `{"x":null}`, false}} {
		var a, b any
		d := json.NewDecoder(strings.NewReader(tc.a))
		d.UseNumber()
		d.Decode(&a)
		d = json.NewDecoder(strings.NewReader(tc.b))
		d.UseNumber()
		d.Decode(&b)
		if equalJSON(a, b) != tc.equal {
			t.Fatal(tc)
		}
	}
}
func TestDeletePreconditionsAndImmutableWatchReplay(t *testing.T) {
	s := serve(t, []Registration{statusWidget()}, DefaultLimits(), nil)
	_, list := request(t, s, "GET", collection, "")
	rv := list["metadata"].(map[string]any)["resourceVersion"].(string)
	_, obj := request(t, s, "POST", collection, object("one"))
	first := copyObject(obj)
	obj["spec"] = map[string]any{"value": "updated"}
	code, obj := request(t, s, "PUT", collection+"/one", encoded(obj))
	if code != 200 {
		t.Fatal(code)
	}
	obj["status"] = map[string]any{"ready": true}
	code, obj = request(t, s, "PUT", collection+"/one/status", encoded(obj))
	if code != 200 {
		t.Fatal(code)
	}
	for _, opts := range []string{`{"preconditions":{"uid":"wrong"}}`, `{"preconditions":{"resourceVersion":"old"}}`} {
		code, _ = request(t, s, "DELETE", collection+"/one", opts)
		if code != 409 {
			t.Fatal(code)
		}
	}
	for _, opts := range []string{`{"propagationPolicy":"Background"}`, `{"gracePeriodSeconds":1}`, `{"dryRun":["All"]}`, `{"orphanDependents":true}`, `{"unknown":true}`} {
		code, _ = request(t, s, "DELETE", collection+"/one", opts)
		if code != 422 {
			t.Fatal(code)
		}
	}
	code, _ = request(t, s, "DELETE", collection+"/one", `{"preconditions":{"uid":123}}`)
	if code != 400 {
		t.Fatal(code)
	}
	meta := obj["metadata"].(map[string]any)
	code, _ = request(t, s, "DELETE", collection+"/one", encoded(map[string]any{"preconditions": map[string]any{"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}, "gracePeriodSeconds": 0}))
	if code != 200 {
		t.Fatal(code)
	}
	code, _ = request(t, s, "GET", collection+"/one", "")
	if code != 404 {
		t.Fatal(code)
	}
	_, empty := request(t, s, "GET", collection, "")
	if len(empty["items"].([]any)) != 0 {
		t.Fatal(empty)
	}
	code, recreated := request(t, s, "POST", collection, object("one"))
	if code != 201 || recreated["metadata"].(map[string]any)["uid"] == meta["uid"] {
		t.Fatal(code, recreated)
	}
	code, _ = request(t, s, "DELETE", collection+"/one", encoded(map[string]any{"preconditions": map[string]any{"uid": meta["uid"]}}))
	if code != 409 {
		t.Fatal("old UID deleted replacement", code)
	}
	// Controlled handoff: all five mutations complete after LIST before WATCH.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+s.Address()+collection+"?watch=true&resourceVersion="+rv, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	decoder := json.NewDecoder(res.Body)
	for i, kind := range []string{"ADDED", "MODIFIED", "MODIFIED", "DELETED", "ADDED"} {
		var e struct {
			Type   string
			Object map[string]any
		}
		if err := decoder.Decode(&e); err != nil {
			t.Fatal(err)
		}
		m := e.Object["metadata"].(map[string]any)
		if e.Type != kind || m["resourceVersion"] != fmt.Sprint(i+2) {
			t.Fatal(i, e)
		}
		if i == 0 && !equalJSON(e.Object["spec"], first["spec"]) {
			t.Fatal("history mutated")
		}
		if i == 3 && m["uid"] != meta["uid"] {
			t.Fatal("deleted identity")
		}
	}
}
func TestAtomicWritersAndDelete(t *testing.T) {
	for _, mode := range []string{"status", "delete", "noops"} {
		t.Run(mode, func(t *testing.T) {
			s := serve(t, []Registration{statusWidget()}, DefaultLimits(), nil)
			_, base := request(t, s, "POST", collection, object("one"))
			left, right := copyObject(base), copyObject(base)
			left["spec"] = map[string]any{"number": 1}
			right["status"] = map[string]any{"ready": true}
			method, path, body := "PUT", collection+"/one/status", encoded(right)
			if mode == "delete" {
				method = "DELETE"
				path = collection + "/one"
				body = encoded(map[string]any{"preconditions": map[string]any{"resourceVersion": base["metadata"].(map[string]any)["resourceVersion"]}})
			}
			if mode == "noops" {
				left = base
				path = collection + "/one"
				body = encoded(base)
			}
			start := make(chan struct{})
			results := make(chan int, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				c, _ := request(t, s, "PUT", collection+"/one", encoded(left))
				results <- c
			}()
			go func() { defer wg.Done(); <-start; c, _ := request(t, s, method, path, body); results <- c }()
			close(start)
			wg.Wait()
			a, b := <-results, <-results
			if mode == "noops" {
				if a != 200 || b != 200 {
					t.Fatal(a, b)
				}
			} else if !((a == 200 && (b == 409 || b == 404)) || (b == 200 && (a == 409 || a == 404))) {
				t.Fatal(a, b)
			}
		})
	}
}
func TestMutationHistoryBoundAndNoGarbageCollector(t *testing.T) {
	limits := DefaultLimits()
	limits.History = 3
	s := serve(t, []Registration{widget}, limits, nil)
	_, parent := request(t, s, "POST", collection, object("parent"))
	child, _ := decodeObject([]byte(object("child")))
	child["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": widget.APIVersion(), "kind": widget.Kind, "name": "parent", "uid": parent["metadata"].(map[string]any)["uid"], "controller": true, "blockOwnerDeletion": true}}
	code, _ := request(t, s, "POST", collection, encoded(child))
	if code != 201 {
		t.Fatal(code)
	}
	code, _ = request(t, s, "DELETE", collection+"/parent", "")
	if code != 200 {
		t.Fatal(code)
	}
	code, obj := request(t, s, "GET", collection+"/child", "")
	if code != 200 {
		t.Fatal("implicit GC", code)
	}
	obj["spec"] = map[string]any{"changed": true}
	code, _ = request(t, s, "PUT", collection+"/child", encoded(obj))
	if code != 503 {
		t.Fatal(code)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.history) != 3 || len(s.objects) != 1 {
		t.Fatal("capacity changed state")
	}
}

func TestStatusDiscoveryScopesAndMalformedUpdates(t *testing.T) {
	cluster := statusWidget()
	cluster.Scope = "Cluster"
	cluster.Kind = "Global"
	cluster.ListKind = "GlobalList"
	cluster.Plural = "globals"
	s := serve(t, []Registration{widget, cluster}, DefaultLimits(), nil)
	_, discovery := request(t, s, "GET", "/apis/example.test/v1", "")
	names := map[string]bool{}
	for _, x := range discovery["resources"].([]any) {
		names[x.(map[string]any)["name"].(string)] = true
	}
	if names["widgets/status"] || !names["globals/status"] {
		t.Fatal(names)
	}
	body := strings.Replace(object("one"), `"Widget"`, `"Global"`, 1)
	code, obj := request(t, s, "POST", "/apis/example.test/v1/globals", body)
	if code != 201 {
		t.Fatal(code, obj)
	}
	code, _ = request(t, s, "GET", "/apis/example.test/v1/globals/one/status", "")
	if code != 200 {
		t.Fatal(code)
	}
	for _, path := range []string{collection + "/one/status", "/apis/example.test/v1/namespaces/demo/globals/one/status"} {
		code, _ = request(t, s, "GET", path, "")
		if code != 404 {
			t.Fatal(code, path)
		}
	}
	code, _ = request(t, s, "PUT", collection+"/missing", object("missing"))
	if code != 404 {
		t.Fatal(code)
	}
	bad := copyObject(obj)
	bad["metadata"].(map[string]any)["name"] = "different"
	code, _ = request(t, s, "PUT", "/apis/example.test/v1/globals/one", encoded(bad))
	if code != 400 {
		t.Fatal(code)
	}
	for _, method := range []string{"PATCH", "POST", "DELETE"} {
		code, _ = request(t, s, method, "/apis/example.test/v1/globals/one/status", encoded(obj))
		if code != 405 {
			t.Fatal(method, code)
		}
	}
}

func TestStatusNullMissingArraysAndMetadataOnlyGeneration(t *testing.T) {
	s := serve(t, []Registration{statusWidget()}, DefaultLimits(), nil)
	_, obj := request(t, s, "POST", collection, object("one"))
	history := func() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.history) }
	obj["status"] = nil
	code, obj := request(t, s, "PUT", collection+"/one/status", encoded(obj))
	if code != 200 || history() != 2 {
		t.Fatal(code, obj, history())
	}
	code, obj = request(t, s, "PUT", collection+"/one/status", encoded(obj))
	if code != 200 || history() != 2 {
		t.Fatal("null no-op")
	}
	delete(obj, "status")
	code, obj = request(t, s, "PUT", collection+"/one/status", encoded(obj))
	if code != 200 || history() != 3 {
		t.Fatal("missing differs from null")
	}
	obj["status"] = map[string]any{"values": []any{1, 2}}
	_, obj = request(t, s, "PUT", collection+"/one/status", encoded(obj))
	obj["status"] = map[string]any{"values": []any{2, 1}}
	_, obj = request(t, s, "PUT", collection+"/one/status", encoded(obj))
	if history() != 5 {
		t.Fatal("array order ignored")
	}
	obj["metadata"].(map[string]any)["labels"] = map[string]any{"new": "label"}
	_, obj = request(t, s, "PUT", collection+"/one", encoded(obj))
	if obj["metadata"].(map[string]any)["generation"] != json.Number("1") {
		t.Fatal("metadata/status changed generation")
	}
}

func TestMutationCaptureFailureDoesNotRollback(t *testing.T) {
	for _, kind := range []string{"resource.updated", "resource.statusUpdated", "resource.deleted"} {
		t.Run(kind, func(t *testing.T) {
			s := serve(t, []Registration{statusWidget()}, DefaultLimits(), func(o contracts.Observation) error {
				if o.Kind == kind {
					return fmt.Errorf("injected capture failure")
				}
				return nil
			})
			_, obj := request(t, s, "POST", collection, object("one"))
			method, path, body := "PUT", collection+"/one", obj
			if kind == "resource.statusUpdated" {
				path += "/status"
				obj["status"] = map[string]any{"value": true}
			} else if kind == "resource.deleted" {
				method = "DELETE"
				body = map[string]any{}
			} else {
				obj["spec"] = map[string]any{"changed": true}
			}
			code, _ := request(t, s, method, path, encoded(body))
			if code != 503 {
				t.Fatal(code)
			}
			select {
			case <-s.Failures():
			case <-time.After(time.Second):
				t.Fatal("capture failure not propagated")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.history) != 2 {
				t.Fatal("lost committed history")
			}
			if kind == "resource.deleted" && len(s.objects) != 0 {
				t.Fatal("delete rolled back")
			}
			if kind != "resource.deleted" {
				stored, _ := decodeObject(s.objects[widget.Key()+"/demo/one"].raw)
				field := "spec"
				if kind == "resource.statusUpdated" {
					field = "status"
				}
				if !equalJSON(stored[field], obj[field]) {
					t.Fatal("update rolled back")
				}
			}
		})
	}
}

func TestResourceNamedStatusRoutes(t *testing.T) {
	for _, scope := range []string{"Namespaced", "Cluster"} {
		for _, declared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/status=%v", scope, declared), func(t *testing.T) {
				reg := widget
				reg.Scope, reg.Status = scope, declared
				s := serve(t, []Registration{reg}, DefaultLimits(), nil)
				base := collection
				body := object("status")
				if scope == "Cluster" {
					base = "/apis/example.test/v1/widgets"
					o, _ := decodeObject([]byte(body))
					delete(o["metadata"].(map[string]any), "namespace")
					body = encoded(o)
				}
				code, obj := request(t, s, "POST", base, body)
				if code != 201 {
					t.Fatal(code, obj)
				}
				path := base + "/status"
				code, obj = request(t, s, "GET", path, "")
				if code != 200 {
					t.Fatal(code, obj)
				}
				obj["spec"] = map[string]any{"changed": true}
				code, obj = request(t, s, "PUT", path, encoded(obj))
				if code != 200 {
					t.Fatal(code, obj)
				}
				want := 404
				if declared {
					want = 200
				}
				code, _ = request(t, s, "GET", path+"/status", "")
				if code != want {
					t.Fatal(code, want)
				}
				obj["status"] = map[string]any{"observed": true}
				code, updated := request(t, s, "PUT", path+"/status", encoded(obj))
				if code != want {
					t.Fatal(code, updated)
				}
				if declared {
					obj = updated
					if obj["status"].(map[string]any)["observed"] != true {
						t.Fatal(obj)
					}
				}
				code, result := request(t, s, "DELETE", path, "")
				if code != 200 {
					t.Fatal(code, result)
				}
				code, _ = request(t, s, "GET", path, "")
				if code != 404 {
					t.Fatal(code)
				}
			})
		}
	}
}
