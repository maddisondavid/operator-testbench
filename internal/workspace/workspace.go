// Package workspace owns filesystem containment, ownership and session artifacts.
// The supported runtime is Linux in Docker. Advisory locks are released by the OS
// on process death; the lock inode must never be removed while the workspace exists.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"operator-dynamo/internal/contracts"
)

var ErrBusy = errors.New("workspace busy")

type Workspace struct {
	root    *os.Root
	session *os.Root
	run     *os.Root
	lock    *os.File
}

func Open(path string) (_ *Workspace, err error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty workspace path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	w := &Workspace{root: root}
	defer func() {
		if err != nil {
			err = errors.Join(err, w.Close())
		}
	}()
	// Refuse a symlink lock; otherwise two paths could claim different ownership.
	if info, e := root.Lstat(".dynamo.lock"); e == nil && !info.Mode().IsRegular() {
		return nil, errors.New("workspace lock must be a regular file")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	w.lock, err = root.OpenFile(".dynamo.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("workspace lock: %w", err)
	}
	if err = syscall.Flock(int(w.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	for _, name := range []string{"config", "resources", "sessions", "runs"} {
		if err = root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, e := root.Lstat(name)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("managed directory %s must be a real directory", name)
		}
	}
	return w, nil
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\")
}

func (w *Workspace) NewSession(id string) error {
	if !validName(id) {
		return errors.New("invalid session ID")
	}
	if w.session != nil {
		return errors.New("workspace already has an active session")
	}
	// Root operations prevent escaping the root even during concurrent symlink replacement.
	name := "sessions/" + id
	if err := w.root.Mkdir(name, 0700); err != nil {
		return err
	}
	r, err := w.root.OpenRoot(name)
	if err != nil {
		return err
	}
	w.session = r
	return nil
}

func (w *Workspace) WriteMetadata(m contracts.Metadata) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return w.WriteArtifact("session.json", append(data, '\n'))
}

// WriteArtifact atomically replaces a flat session artifact; never follows its target.
func (w *Workspace) WriteArtifact(name string, data []byte) (err error) {
	if w.session == nil {
		return errors.New("session not allocated")
	}
	if !validName(name) {
		return errors.New("invalid artifact name")
	}
	if info, e := w.session.Lstat(name); e == nil && !info.Mode().IsRegular() {
		return errors.New("artifact target must be a regular file")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	// A fixed private temporary name is safe with one owner; exclusive create also
	// refuses symlinks and stale partial files rather than overwriting unknown input.
	tmp := "." + name + ".tmp"
	f, err := w.session.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = w.session.Remove(tmp)
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return w.session.Rename(tmp, name)
}

func (w *Workspace) CreateJournal() (*os.File, error) {
	if w.session == nil {
		return nil, errors.New("session not allocated")
	}
	return w.session.OpenFile("events.jsonl", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

// Close retains the session artifact handle if an earlier teardown step fails,
// allowing the application to correct its terminal metadata before retrying Close.
func (w *Workspace) Close() error {
	var errs []error
	if w.run != nil {
		errs = append(errs, w.run.Close())
		w.run = nil
	}
	if w.root != nil {
		errs = append(errs, w.root.Close())
		w.root = nil
	}
	if w.lock != nil {
		errs = append(errs, w.lock.Close())
		w.lock = nil
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if w.session != nil {
		err := w.session.Close()
		w.session = nil
		return err
	}
	return nil
}

// ReadInput confines reads including symlink traversal to the workspace root.
func (w *Workspace) ReadInput(name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, contracts.ErrInvalid
	}
	f, err := w.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, contracts.ErrInvalid
	}
	return io.ReadAll(io.LimitReader(f, (4<<20)+1))
}
