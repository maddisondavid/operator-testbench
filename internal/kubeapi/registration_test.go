package kubeapi

import (
	"fmt"
	"strings"
	"testing"
)

const definition = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.test
spec:
  group: example.test
  scope: Namespaced
  names:
    plural: widgets
    kind: Widget
  conversion:
    strategy: None
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
    subresources:
      status: {}
`

func TestImportNativeCRDs(t *testing.T) {
	regs, err := ImportCRDs([]byte(definition))
	if err != nil || len(regs) != 1 || regs[0].ListKind != "WidgetList" || !regs[0].Status {
		t.Fatal(regs, err)
	}
	if ValidateRegistrations(append(regs, regs...)) == nil {
		t.Fatal("duplicate accepted")
	}
	for _, replacement := range []struct{ old, new string }{{"strategy: None", "strategy: Webhook"}, {"scope: Namespaced", "scope: Invalid"}, {"served: true", "served: false"}, {"storage: true", "storage: false"}, {"widgets.example.test", "wrong.example.test"}, {"kind: Widget", "kind: bad-kind"}} {
		if _, err := ImportCRDs([]byte(strings.Replace(definition, replacement.old, replacement.new, 1))); err == nil {
			t.Fatal(replacement)
		}
	}
	cluster := strings.Replace(definition, "scope: Namespaced", "scope: Cluster", 1)
	if _, err := ImportCRDs([]byte(cluster)); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportCRDs([]byte(definition + "  - name: v2\n    served: true\n    storage: false\n")); err == nil {
		t.Fatal("multiple versions accepted")
	}
}

func TestInactiveVersionSelectionAndAtomicRejection(t *testing.T) {
	inactive := "  - name: v0\n    served: false\n    storage: false\n    subresources:\n      status: {ignored: true}\n"
	// Selected version is not necessarily first, and inactive status cannot confer capability.
	base := strings.Replace(definition, "  versions:\n", "  versions:\n"+inactive, 1)
	regs, e := ImportCRDs([]byte(base))
	if e != nil || len(regs) != 1 || regs[0].Version != "v1" || !regs[0].Status {
		t.Fatal(regs, e)
	}
	noStatus := strings.Replace(base, "    subresources:\n      status: {}\n", "", 1)
	regs, e = ImportCRDs([]byte(noStatus))
	if e != nil || regs[0].Status {
		t.Fatal(regs, e)
	}
	for _, bad := range []string{
		strings.Replace(base, "name: v0", "name: v1", 1),
		strings.Replace(base, "name: v0", "name: bad.version", 1),
		strings.Replace(base, "    served: false\n", "", 1),
		strings.Replace(base, "    storage: false\n", "", 1),
		strings.Replace(base, "served: false", "served: null", 1),
		strings.Replace(base, "storage: false", "storage: null", 1),
		strings.Replace(base, "served: false", "served: \"false\"", 1),
		strings.Replace(base, "storage: false", "storage: 0", 1),
		strings.Replace(base, "served: false", "served: true", 1),
		strings.Replace(base, "storage: false", "storage: true", 1),
		strings.ReplaceAll(base, "false", "true"),
		strings.ReplaceAll(base, "true", "false"),
		strings.Replace(base, "strategy: None", "strategy: Webhook", 1),
		strings.Replace(base, "strategy: None", "strategy: None\n    webhook: {}", 1),
	} {
		if got, err := ImportCRDs([]byte(definition + "---\n" + bad)); err == nil || got != nil {
			t.Fatal("partially admitted invalid document", bad, got, err)
		}
	}
}

func TestInactiveVersionsHaveNoRoutesOrStatus(t *testing.T) {
	input := strings.Replace(definition, "    subresources:\n      status: {}\n", "", 1) + "  - name: v0\n    served: false\n    storage: false\n    subresources:\n      status: {}\n"
	regs, err := ImportCRDs([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, regs, DefaultLimits(), nil)
	if code, body := request(t, s, "GET", "/apis/example.test", ""); code != 200 || strings.Contains(fmt.Sprint(body), "v0") {
		t.Fatal(code, body)
	}
	for _, path := range []string{"/apis/example.test/v0", "/apis/example.test/v0/namespaces/demo/widgets", "/apis/example.test/v0/namespaces/demo/widgets?watch=true&resourceVersion=0"} {
		if code, _ := request(t, s, "GET", path, ""); code != 404 {
			t.Fatal(path, code)
		}
	}
	if code, _ := request(t, s, "POST", collection, object("one")); code != 201 {
		t.Fatal(code)
	}
	if code, _ := request(t, s, "GET", collection+"/one/status", ""); code != 404 {
		t.Fatal(code)
	}
	if code, body := request(t, s, "GET", collection, ""); code != 200 || len(body["items"].([]any)) != 1 {
		t.Fatal(code, body)
	}
}

func TestVersionNamesAreStringsBeforeDecoding(t *testing.T) {
	for _, raw := range []string{"7", "true", "false", "null", "[]", "{}", ""} {
		for _, input := range []string{
			strings.Replace(definition, "name: v1", "name: "+raw, 1),
			definition + "  - name: " + raw + "\n    served: false\n    storage: false\n",
		} {
			if got, err := ImportCRDs([]byte(definition + "---\n" + input)); err == nil || got != nil {
				t.Fatalf("name %q admitted: %v %v", raw, got, err)
			}
		}
	}
	// Preserve actual valid string names; no new version-format policy.
	for _, raw := range []string{"v1", "\"7\"", "\"true\""} {
		if _, err := ImportCRDs([]byte(strings.Replace(definition, "name: v1", "name: "+raw, 1))); err != nil {
			t.Fatal(raw, err)
		}
	}
}
