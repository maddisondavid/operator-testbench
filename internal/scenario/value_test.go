package scenario

import (
	"encoding/json"
	"testing"
)

func TestPointerAndExactValues(t *testing.T) {
	v := map[string]any{"": nil, "01": "object", "-": true, "a/b": map[string]any{"~": []any{json.Number("9007199254740995")}}}
	for _, tc := range []struct {
		p      string
		exists bool
	}{{"", true}, {"/", true}, {"/01", true}, {"/-", true}, {"/a~1b/~0/0", true}, {"/a~1b/~0/01", false}, {"/a~1b/~0/-", false}, {"/a~1b/~0/999999999999999999999", false}, {"/missing/x", false}} {
		if !validPointer(tc.p) {
			t.Fatal(tc.p)
		}
		_, ok := pointer(v, tc.p)
		if ok != tc.exists {
			t.Fatal(tc, ok)
		}
	}
	for _, p := range []string{"no-slash", "/~", "/~2"} {
		if validPointer(p) {
			t.Fatal(p)
		}
	}
	for _, tc := range []struct {
		a, b  any
		equal bool
	}{{json.Number("1"), json.Number("1.0"), true}, {json.Number("9007199254740995"), json.Number("9007199254740996"), false}, {map[string]any{}, map[string]any{"x": nil}, false}, {[]any{true, false}, []any{false, true}, false}, {json.Number("1e99999999999999999999"), json.Number("10e99999999999999999998"), true}} {
		if equalJSON(tc.a, tc.b) != tc.equal {
			t.Fatal(tc)
		}
	}
}
