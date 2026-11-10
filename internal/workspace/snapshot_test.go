package workspace

import (
	"context"
	"errors"
	"operator-dynamo/internal/contracts"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestContainedRuntimeSnapshots(t *testing.T) {
	dir := t.TempDir()
	w, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	os.MkdirAll(filepath.Join(dir, "src/nested"), 0750)
	os.WriteFile(filepath.Join(dir, "src/nested/executable"), []byte("captured"), 0755)
	all := func(string, bool) (bool, error) { return true, nil }
	files, e := w.Snapshot(context.Background(), "src", all)
	if e != nil || len(files) != 2 || !files[0].Dir || files[0].Mode != 0750 || files[1].Mode != 0755 {
		t.Fatal(files, e)
	}
	os.WriteFile(filepath.Join(dir, "src/nested/executable"), []byte("changed"), 0755)
	if string(files[1].Data) != "captured" {
		t.Fatal("snapshot changed")
	}
	os.Symlink("../resources", filepath.Join(dir, "alias"))
	if _, e = w.Snapshot(context.Background(), "alias", all); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("root alias", e)
	}
	os.Symlink("/etc/passwd", filepath.Join(dir, "src/link"))
	if _, e = w.Snapshot(context.Background(), "src", all); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("included symlink", e)
	}
	if _, e = w.Snapshot(context.Background(), "src", func(p string, d bool) (bool, error) { return p != "link", nil }); e != nil {
		t.Fatal("excluded symlink opened", e)
	}
	os.Remove(filepath.Join(dir, "src/link"))
	os.Link(filepath.Join(dir, "src/nested/executable"), filepath.Join(dir, "src/hard"))
	if _, e = w.Snapshot(context.Background(), "src", all); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("hardlink accepted", e)
	}
	os.Remove(filepath.Join(dir, "src/hard"))
	syscall.Mkfifo(filepath.Join(dir, "src/fifo"), 0600)
	if _, e = w.Snapshot(context.Background(), "src", all); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("fifo accepted", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = w.Snapshot(ctx, "src", all); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = w.ReadSource(context.Background(), "../outside", 1); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal(e)
	}
	if _, e = w.ReadSource(context.Background(), "src/nested/executable", 1); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("file limit", e)
	}
}
func TestPrivateBoundedOutput(t *testing.T) {
	dir := t.TempDir()
	w, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	if e = w.NewSession("output"); e != nil {
		t.Fatal(e)
	}
	if e = w.WriteOutput(context.Background(), "operator-output.jsonl", nil); e != nil {
		t.Fatal(e)
	}
	if e = w.AppendOutput(context.Background(), "operator-output.jsonl", []byte("sanitized\n")); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(filepath.Join(dir, "sessions/output/operator-output.jsonl"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e = w.AppendOutput(ctx, "operator-output.jsonl", nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	os.Symlink("/tmp/unowned", filepath.Join(dir, "sessions/output/alias"))
	if e = w.AppendOutput(context.Background(), "alias", nil); e == nil {
		t.Fatal("followed artifact symlink")
	}
}

func TestSnapshotSizeAndDepthLimits(t *testing.T) {
	root := t.TempDir()
	w, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	os.Mkdir(filepath.Join(root, "src"), 0700)
	f, e := os.Create(filepath.Join(root, "src/huge"))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Truncate((256 << 20) + 1); e != nil {
		t.Fatal(e)
	}
	f.Close()
	all := func(string, bool) (bool, error) { return true, nil }
	if _, e = w.Snapshot(context.Background(), "src", all); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("size limit", e)
	}
	os.Remove(filepath.Join(root, "src/huge"))
	p := filepath.Join(root, "src")
	for i := 0; i < 65; i++ {
		p = filepath.Join(p, "d")
		if e = os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = w.Snapshot(context.Background(), "src", all); !errors.Is(e, contracts.ErrInvalid) {
		t.Fatal("depth limit", e)
	}
}
