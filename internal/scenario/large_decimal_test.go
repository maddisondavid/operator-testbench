package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifierLargeDecimalParsing(t *testing.T) {
	for _, s := range []string{"1e1000", "1e-1000", "1e10000", "-1e1000"} {
		t.Run(s, func(t *testing.T) {
			m, e := parse([]byte("value: "+s+"\n"), "fixture.yaml")
			if e != nil {
				t.Fatal(e)
			}
			n, ok := m["value"].(json.Number)
			if !ok || string(n) != s {
				t.Fatalf("numeric literal %s became %T", s, m["value"])
			}
		})
	}
	for _, s := range []string{"1e10001", strings.Repeat("9", 1025)} {
		if _, e := parse([]byte("value: "+s+"\n"), "fixture.yaml"); e == nil {
			t.Errorf("out-of-profile numeric literal accepted (length %d)", len(s))
		}
	}
}

func TestQuotedLargeDecimalsRemainStrings(t *testing.T) {
	for _, s := range []string{"1e1000", "1e10001", strings.Repeat("9", 1025)} {
		m, e := parse([]byte("value: \""+s+"\"\n"), "fixture.yaml")
		if e != nil || m["value"] != s {
			t.Fatal("quoted number changed", e)
		}
	}
}
