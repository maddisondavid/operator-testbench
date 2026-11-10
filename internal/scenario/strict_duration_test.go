package scenario

import (
	"context"
	"strings"
	"testing"
)

func TestVerifierNullDurations(t *testing.T) {
	for _, v := range []string{
		strings.Replace(scenarioYAML, "name: example", "name: example\ntimeout: null", 1),
		strings.Replace(scenarioYAML, "    count: 0", "    count: 0\n    timeout: null", 1),
	} {
		w, root := testSource(t)
		put(t, root, "scenario.yaml", v)
		if _, e := BuildPlan(context.Background(), w, "scenario.yaml", widgets); e == nil {
			t.Error("explicit null duration accepted as omitted/defaulted")
		}
	}
}
