package app

import (
	"bytes"
	"context"
	"operator-dynamo/internal/workspace"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifierRepeatedConfigSourceFreeze(t *testing.T) {
	root := t.TempDir()
	w, e := workspace.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	name := "resources/repeated.yaml"
	os.WriteFile(filepath.Join(root, name), []byte("first snapshot"), 0600)
	r := &frozenReader{ctx: context.Background(), source: w, manifest: map[string]string{}}
	a, e := r.ReadInput(name)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(root, name), []byte("second snapshot"), 0600)
	b, e := r.ReadInput(name)
	if e == nil && !bytes.Equal(a, b) {
		t.Fatal("repeated config/CRD source read admitted conflicting bytes and overwrote manifest instead of freezing or rejecting")
	}
}
