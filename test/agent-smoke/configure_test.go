package main

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEndpointRewriteStrict(t *testing.T) {
	for _, bad := range []string{"api: {listen: x, listen: y, advertiseURL: z}", "api: &a {listen: x, advertiseURL: y}\nother: *a", "api: {listen: 7, advertiseURL: y}", "api: {listen: x, advertiseURL: y}\n---\na: b"} {
		root := t.TempDir()
		os.Mkdir(filepath.Join(root, "config"), 0755)
		p := filepath.Join(root, "config/operator.yaml")
		os.WriteFile(p, []byte(bad), 0644)
		if configure(root, "198.51.100.20", "18093") == nil {
			t.Fatal("accepted ambiguity")
		}
		got, _ := os.ReadFile(p)
		if string(got) != bad {
			t.Fatal("mutated invalid config")
		}
	}
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "config"), 0755)
	p := filepath.Join(root, "config/operator.yaml")
	original := []byte("operator:\n  env: {KEEP: 'false'}\napi: {listen: '0.0.0.0:18089', advertiseURL: 'http://198.51.100.10:18089', builtins: [v1/configmaps]}\n")
	os.WriteFile(p, original, 0644)
	if e := configure(root, "198.51.100.20", "18093"); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(p)
	var before, after map[string]any
	yaml.Unmarshal(original, &before)
	yaml.Unmarshal(b, &after)
	api := before["api"].(map[string]any)
	api["listen"] = "0.0.0.0:18093"
	api["advertiseURL"] = "http://198.51.100.20:18093"
	if !reflect.DeepEqual(before, after) {
		t.Fatal("changed unrelated fields")
	}
}
