package operatorruntime

import (
	"context"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/workspace"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedInputsDoNotRereadAndDoNotAllocateArtifacts(t *testing.T) {
	root := t.TempDir()
	w, e := workspace.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	os.MkdirAll(filepath.Join(root, "src"), 0700)
	os.WriteFile(filepath.Join(root, "src/Dockerfile"), []byte("FROM scratch\nCOPY value /value\n"), 0600)
	os.WriteFile(filepath.Join(root, "src/value"), []byte("original"), 0600)
	cfg := Config{Build: &BuildConfig{Context: "../src"}, Files: []FileConfig{{Source: "../src/value", Path: "value"}}}
	prepared, e := Prepare(context.Background(), cfg, w)
	if e != nil {
		t.Fatal(e)
	}
	if len(prepared.Manifest()) != 2 {
		t.Fatal(prepared.Manifest())
	}
	entries, _ := os.ReadDir(filepath.Join(root, "sessions"))
	if len(entries) != 0 {
		t.Fatal("preflight created session")
	}
	os.RemoveAll(filepath.Join(root, "src"))
	for _, id := range []string{"first", "second"} {
		if e = w.NewSession(id); e != nil {
			t.Fatal(e)
		}
		r := &Runtime{Config: cfg, Inputs: w, Prepared: prepared}
		archive, e := r.preparedInputs(context.Background())
		if e != nil || len(archive) == 0 || string(r.inputFiles[0].Data) != "original" {
			t.Fatal(e)
		}
		r.buildInfo.Outcome = "mutated-only-this-session"
		w.ReleaseSession()
	}
	if prepared.evidence.Outcome != "prepared" {
		t.Fatal("mutable provenance shared")
	}
	if (Config{Image: "local", Files: []FileConfig{{Source: "../runs/input", Path: "x"}}}).Validate() != contracts.ErrInvalid {
		t.Fatal("runs not reserved")
	}
}

type changingPreparation struct {
	Inputs
	count int
}

func (p *changingPreparation) ReadSource(_ context.Context, name string, _ int64) (contracts.InputFile, error) {
	p.count++
	return contracts.InputFile{Name: name, Data: []byte{byte(p.count)}, Mode: 0600}, nil
}
func TestPreparationRejectsConflictingSnapshotsOfSameSource(t *testing.T) {
	source := &changingPreparation{}
	cfg := Config{Image: "local", Files: []FileConfig{{Source: "../input", Path: "a"}, {Source: "../input", Path: "b"}}}
	if _, e := Prepare(context.Background(), cfg, source); e != contracts.ErrInvalid {
		t.Fatal(e)
	}
}
