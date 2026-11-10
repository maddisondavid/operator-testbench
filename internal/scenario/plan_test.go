package scenario

import (
	"context"
	"errors"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/workspace"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var widgets = []Resource{{APIVersion: "example.test/v1", Kind: "Widget", Plural: "widgets", Scope: "Namespaced"}}

const scenarioYAML = `apiVersion: dynamo/v1alpha1
kind: Scenario
name: example
steps:
- id: absent
  assert:
    target: {apiVersion: example.test/v1, kind: Widget, namespace: default, name: example}
    count: 0
`

func testSource(t *testing.T) (*workspace.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	w, e := workspace.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { w.Close() })
	return w, root
}
func put(t *testing.T, root, name, text string) {
	t.Helper()
	p := filepath.Join(root, name)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestSelectionOrderAndIsolationInputs(t *testing.T) {
	w, root := testSource(t)
	put(t, root, "tests/z/scenario.yaml", scenarioYAML)
	put(t, root, "tests/a/scenario.yaml", scenarioYAML)
	put(t, root, "tests/not-a-definition.yaml", "ignored: true")
	plan, e := BuildPlan(context.Background(), w, "tests", widgets)
	if e != nil || len(plan.Scenarios) != 2 || plan.Scenarios[0].SourcePath != "tests/a/scenario.yaml" {
		t.Fatalf("plan=%+v error=%#v", plan, e)
	}
	put(t, root, "tests/suite.yaml", "apiVersion: dynamo/v1alpha1\nkind: Suite\nname: ordered\nentries: [z, a]\n")
	put(t, root, "tests/scenario.yaml", "deliberately invalid ignored content")
	plan, e = BuildPlan(context.Background(), w, "tests", widgets)
	if e != nil || len(plan.Scenarios) != 2 || plan.Scenarios[0].SourcePath != "tests/z/scenario.yaml" {
		t.Fatalf("plan=%+v error=%#v", plan, e)
	}
	put(t, root, "tests/z/scenario.yaml", "changed after snapshot")
	if plan.Scenarios[0].Name != "example" {
		t.Fatal("plan changed")
	}
}
func TestPreflightRejectsWholeInvalidPlan(t *testing.T) {
	for _, value := range []string{
		"apiVersion: dynamo/v1alpha1\nkind: Suite\nname: duplicate\nentries: [a, a]\n",
		"apiVersion: dynamo/v1alpha1\nkind: Suite\nname: cycle\nentries: [suite.yaml]\n",
		"apiVersion: dynamo/v1alpha1\nkind: Suite\nname: invalid\nentries: [a, missing]\n",
		"apiVersion: dynamo/v1alpha1\nkind: Suite\nname: escape\nentries: [../../outside]\n",
	} {
		w, root := testSource(t)
		put(t, root, "tests/a/scenario.yaml", scenarioYAML)
		put(t, root, "tests/suite.yaml", value)
		if _, e := BuildPlan(context.Background(), w, "tests", widgets); !errors.Is(e, contracts.ErrInvalid) {
			t.Fatal(e)
		}
	}
}
func TestStrictParsing(t *testing.T) {
	for _, input := range []string{
		scenarioYAML + "other: x\n", scenarioYAML + "name: duplicated\n", strings.Replace(scenarioYAML, "steps:", "steps: &alias", 1), strings.Replace(scenarioYAML, "count: 0", "count: 01", 1), scenarioYAML + "---\n{}", strings.Replace(scenarioYAML, "count: 0", "count: .nan", 1), strings.Replace(scenarioYAML, "steps:\n", "steps: []\nunused:\n", 1), strings.Replace(scenarioYAML, "count: 0", "count: 1e10001", 1),
	} {
		w, root := testSource(t)
		put(t, root, "scenario.yaml", input)
		if _, e := BuildPlan(context.Background(), w, "scenario.yaml", widgets); !errors.Is(e, contracts.ErrInvalid) {
			t.Fatalf("accepted invalid input: %v", e)
		}
	}
}
func TestContainedSymlinkHardlinkAndManagedSelection(t *testing.T) {
	w, root := testSource(t)
	put(t, root, "tests/scenario.yaml", scenarioYAML)
	os.Symlink("tests", filepath.Join(root, "alias"))
	os.Link(filepath.Join(root, "tests/scenario.yaml"), filepath.Join(root, "linked.yaml"))
	for _, p := range []string{"alias", "linked.yaml", "sessions", "runs", "../escape"} {
		if _, e := BuildPlan(context.Background(), w, p, widgets); !errors.Is(e, contracts.ErrInvalid) {
			t.Fatal(p, e)
		}
	}
}
func TestFixturesAndReferences(t *testing.T) {
	w, root := testSource(t)
	text := `apiVersion: dynamo/v1alpha1
kind: Scenario
name: fixture
steps:
- id: create
  create: {fixture: value.yaml}
- id: save
  assert:
    target: {apiVersion: example.test/v1, kind: Widget, namespace: default, name: example}
    count: 1
    fields: [{pointer: /spec/number, equals: 9007199254740995}]
    saveAs: first
- id: compare
  assert:
    target: {apiVersion: example.test/v1, kind: Widget, namespace: default, name: example}
    count: 1
    fields: [{pointer: /metadata/uid, equalsFrom: {observation: first, pointer: /metadata/uid}}]
`
	put(t, root, "scenario.yaml", text)
	put(t, root, "value.yaml", "apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: example, namespace: default}\nspec: {number: 9007199254740995}\n")
	plan, e := BuildPlan(context.Background(), w, "scenario.yaml", widgets)
	if e != nil || len(plan.Inputs) != 2 {
		t.Fatalf("plan=%+v error=%#v", plan, e)
	}
	put(t, root, "scenario.yaml", strings.Replace(text, "observation: first", "observation: future", 1))
	if _, e = BuildPlan(context.Background(), w, "scenario.yaml", widgets); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal(e)
	}
}

func TestExplicitSuiteDoesNotScanUndeclaredSibling(t *testing.T) {
	w, root := testSource(t)
	put(t, root, "tests/suite.yaml", "apiVersion: dynamo/v1alpha1\nkind: Suite\nname: selected\nentries: [chosen.yaml]\n")
	put(t, root, "tests/chosen.yaml", scenarioYAML)
	os.Symlink("/not-selected", filepath.Join(root, "tests/undeclared"))
	plan, e := BuildPlan(context.Background(), w, "tests", widgets)
	if e != nil || len(plan.Scenarios) != 1 {
		t.Fatal(plan, e)
	}
}
func TestPlanBudgets(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		w, root := testSource(t)
		p := strings.Repeat("d/", 65) + "scenario.yaml"
		put(t, root, p, scenarioYAML)
		if _, e := BuildPlan(context.Background(), w, "d", widgets); !errors.Is(e, contracts.ErrInvalid) {
			t.Fatal(e)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		w, root := testSource(t)
		put(t, root, "scenario.yaml", strings.Repeat(" ", 1<<20)+scenarioYAML)
		if _, e := BuildPlan(context.Background(), w, "scenario.yaml", widgets); !errors.Is(e, contracts.ErrInvalid) {
			t.Fatal(e)
		}
	})
	t.Run("empty", func(t *testing.T) {
		w, root := testSource(t)
		put(t, root, "tests/unused.yaml", "not: selected")
		if _, e := BuildPlan(context.Background(), w, "tests", widgets); !errors.Is(e, contracts.ErrInvalid) {
			t.Fatal(e)
		}
	})
	t.Run("report", func(t *testing.T) {
		p := &Plan{}
		for i := 0; i < 100; i++ {
			s := Scenario{Name: "x"}
			for j := 0; j < 100; j++ {
				s.Steps = append(s.Steps, Step{ID: "x"})
			}
			p.Scenarios = append(p.Scenarios, s)
		}
		if ValidateReportBound(p) == nil {
			t.Fatal("oversized structural report reservation accepted")
		}
	})
}
