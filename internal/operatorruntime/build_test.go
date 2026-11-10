package operatorruntime

import (
	"strings"
	"testing"
)

func TestBuildTerminalProtocol(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	marker := `{"aux":{"ID":"` + id + `"}}` + "\n"
	for _, tc := range []struct {
		input string
		ok    bool
	}{{marker, true}, {`{"aux":{"ID":"sha256:` + strings.Repeat("b", 64) + `"}}` + "\n" + marker, true}, {`{"stream":"Successfully built fake"}` + "\n", false}, {marker + `{"error":"failed"}` + "\n", false}, {marker + `{"errorDetail":{"message":"failed"}}` + "\n", false}, {marker + `{"broken":`, false}, {`{"aux":{"ID":"bad"}}`, false}, {strings.Repeat("a", (1<<20)+1), false}} {
		v, e := readBuildResult(strings.NewReader(tc.input))
		if (e == nil) != tc.ok || tc.ok && v != id {
			t.Fatal(v, e)
		}
	}
}
func TestWorkflowConfiguration(t *testing.T) {
	for _, c := range []Config{{}, {Image: "a", Build: &BuildConfig{Context: "../src"}}, {Build: &BuildConfig{Context: ".."}}, {Build: &BuildConfig{Context: "../sessions/src"}}, {Build: &BuildConfig{Context: "../src", Dockerfile: "../Dockerfile"}}, {Image: "a", Files: []FileConfig{{Source: "../input", Path: "a"}, {Source: "../input", Path: "a/b"}}}, {Image: "a", Volumes: []VolumeConfig{{Name: "../bad"}}}, {Image: "a", Logs: LogConfig{Mode: "off", Redact: []string{"secret"}}}, {Image: "a", Logs: LogConfig{Mode: "capture", Redact: []string{""}}}} {
		if c.Validate() == nil {
			t.Fatalf("invalid config accepted %+v", c)
		}
	}
	for _, c := range []Config{{Image: "a", Env: map[string]string{"EMPTY": ""}}, {Build: &BuildConfig{Context: "../src"}}, {Image: "a", Logs: LogConfig{Mode: "capture"}, Env: map[string]string{"EMPTY": ""}}} {
		if c.Validate() != nil {
			t.Fatal(c)
		}
	}
}
