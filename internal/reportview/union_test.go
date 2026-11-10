package reportview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"operator-dynamo/internal/contracts"
	"os"
	"reflect"
	"testing"
)

func TestSuitePathsUnionAdmission(t *testing.T) {
	original, err := os.ReadFile("../../test/reports/test-mode-report.json")
	if err != nil {
		t.Fatal(err)
	}
	paths32 := make([]any, 32)
	for i := range paths32 {
		paths32[i] = "tests/suite.yaml"
	}
	paths33 := append(append([]any{}, paths32...), "tests/overflow.yaml")
	var nested any = "tests/suite.yaml"
	for i := 0; i < 70; i++ {
		nested = []any{nested}
	}
	tests := []struct {
		name  string
		value any
		valid bool
	}{
		{"max_depth_paths", paths32, true}, {"too_many_paths", paths33, false}, {"nested_depth_limit", nested, false},
		{"null", nil, true}, {"empty_array", []any{}, true},
		{"valid_paths", []any{"tests/suite.yaml", "tests/nested/suite.yaml"}, true},
		{"string", "tests/suite.yaml", false},
		{"object", map[string]any{"path": "tests/suite.yaml"}, false},
		{"boolean", true, false}, {"number", json.Number("42"), false},
		{"array_number", []any{json.Number("42")}, false},
		{"array_object", []any{map[string]any{"path": "tests/suite.yaml"}}, false},
		{"array_boolean", []any{true}, false}, {"array_null", []any{nil}, false},
		{"array_nested", []any{[]any{"tests/suite.yaml"}}, false},
		{"array_escape", []any{"../suite.yaml"}, false},
		{"array_mixed", []any{"tests/suite.yaml", true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			d := json.NewDecoder(bytes.NewReader(original))
			d.UseNumber()
			if e := d.Decode(&m); e != nil {
				t.Fatal(e)
			}
			m["scenarios"].([]any)[0].(map[string]any)["suitePaths"] = tc.value
			b, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			view, e := Validate(context.Background(), b, m["runId"].(string))
			if tc.valid {
				if e != nil {
					t.Fatalf("valid input rejected: %v", e)
				}
				got := view["scenarios"].([]any)[0].(map[string]any)["suitePaths"]
				if !reflect.DeepEqual(got, tc.value) {
					t.Fatalf("valid representation changed: %#v", got)
				}
				t.Log("accepted valid suitePaths")
				return
			}
			if e == nil {
				got := view["scenarios"].([]any)[0].(map[string]any)["suitePaths"]
				t.Fatalf("invalid suitePaths admitted; inputType=%T returnedType=%T returnedValue=%v", tc.value, got, got)
			}
			var re *contracts.ReadError
			if !errors.As(e, &re) || re.Status != 422 || re.Code != "invalid_report" {
				t.Fatalf("wrong rejection: %v", e)
			}
			t.Logf("invalid suitePaths rejected: %v", e)
		})
	}
}
