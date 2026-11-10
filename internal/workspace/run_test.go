package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunReportAndSessionReleaseKeepLock(t *testing.T) {
	root := t.TempDir()
	w, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	if e = w.NewRun("run"); e != nil {
		t.Fatal(e)
	}
	if e = w.WriteRun(context.Background(), map[string]any{"status": "running"}); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"first", "second"} {
		if e = w.NewSession(id); e != nil {
			t.Fatal(e)
		}
		if e = w.ReleaseSession(); e != nil {
			t.Fatal(e)
		}
		if other, e := Open(root); !errors.Is(e, ErrBusy) {
			if other != nil {
				other.Close()
			}
			t.Fatal("lock released between sessions", e)
		}
	}
	if e = w.WriteRun(context.Background(), map[string]any{"status": "passed"}); e != nil {
		t.Fatal(e)
	}
	i, e := os.Stat(filepath.Join(root, "runs/run/report.json"))
	if e != nil || i.Mode().Perm() != 0600 {
		t.Fatal(i, e)
	}
	os.Remove(filepath.Join(root, "runs/run/report.json"))
	os.Symlink("/tmp/never-follow-report", filepath.Join(root, "runs/run/report.json"))
	if e = w.WriteRun(context.Background(), map[string]any{}); e == nil {
		t.Fatal("report symlink followed")
	}
}
func TestDirectorySnapshotDetectsReplacement(t *testing.T) {
	w, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	w.root.Mkdir("tests", 0700)
	_, token, e := w.ListDirectory(context.Background(), "tests", 100)
	if e != nil {
		t.Fatal(e)
	}
	f, e := w.root.Create("tests/added")
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if e = w.CheckDirectory("tests", token); e == nil {
		t.Fatal("directory change missed")
	}
}

func TestTransferredRunOutputKeepsLockAfterWorkspaceClose(t *testing.T) {
	root := t.TempDir()
	w, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	if e = w.NewRun("run"); e != nil {
		t.Fatal(e)
	}
	output, e := w.TakeRunOutput()
	if e != nil {
		t.Fatal(e)
	}
	defer output.Close()
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if other, e := Open(root); !errors.Is(e, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatal("lock lost before terminal publication", e)
	}
	if e = output.Write(context.Background(), map[string]any{"status": "error", "reasonCode": "cleanup_failed"}); e != nil {
		t.Fatal(e)
	}
	if e = output.Close(); e != nil {
		t.Fatal(e)
	}
	other, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	other.Close()
}
