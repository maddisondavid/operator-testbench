package workspace

import (
	"errors"
	"operator-dynamo/internal/contracts"
	"os"
	"path/filepath"
	"testing"
)

func TestInitializationOwnershipAndArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "config", "user.yaml")
	if err = os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(root); !errors.Is(err, ErrBusy) {
		t.Fatalf("second owner: %v", err)
	}
	if err = w.NewSession("one"); err != nil {
		t.Fatal(err)
	}
	if err = w.WriteMetadata(contracts.Metadata{ID: "one", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err = w.WriteMetadata(contracts.Metadata{ID: "one", State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "/escape", ".", "a/b", "a\\b"} {
		if err = w.WriteArtifact(name, nil); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err = os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(root, "sessions", "one", "link")); err != nil {
		t.Fatal(err)
	}
	if err = w.WriteArtifact("link", []byte("bad")); err == nil {
		t.Fatal("followed symlink")
	}
	data, _ := os.ReadFile(outside)
	if string(data) != "keep" {
		t.Fatal("external file changed")
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err = w.NewSession("two"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(sentinel)
	if err != nil || string(data) != "keep" {
		t.Fatal("input changed", err)
	}
	if _, err = os.Stat(filepath.Join(root, "sessions", "one", "session.json")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "resources", "sessions"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal(name, info, err)
		}
	}
}
func TestInvalidRootsAndManagedSymlinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file); err == nil {
		t.Fatal("accepted file root")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("accepted empty root")
	}
	for _, name := range []string{"config", "resources", "sessions", ".dynamo.lock"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Symlink(t.TempDir(), filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(root); err == nil {
				t.Fatal("accepted managed symlink")
			}
		})
	}
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err = w.NewSession("../escape"); err == nil {
		t.Fatal("accepted invalid session")
	}
}
func TestUnwritableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("run required filesystem tests as a non-root user")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if _, err := Open(root); err == nil {
		t.Fatal("accepted unwritable root")
	}
}
func TestAtomicFailurePreservesMetadata(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err = w.NewSession("one"); err != nil {
		t.Fatal(err)
	}
	if err = w.WriteArtifact("session.json", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "sessions", "one", ".session.json.tmp"), []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = w.WriteArtifact("session.json", []byte("new")); err == nil {
		t.Fatal("expected exclusive-create failure")
	}
	data, err := os.ReadFile(filepath.Join(root, "sessions", "one", "session.json"))
	if err != nil || string(data) != "old" {
		t.Fatal(string(data), err)
	}
}

func TestCloseFailureRetainsArtifactForFailureRecording(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.NewSession("one"); err != nil {
		t.Fatal(err)
	}
	// Cause a real descriptor-close error rather than merely mocking its result.
	if err = w.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err == nil {
		t.Fatal("expected close error")
	}
	if err = w.WriteMetadata(contracts.Metadata{ID: "one", State: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
}
