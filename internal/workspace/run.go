package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"operator-dynamo/internal/contracts"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// Entry and directory fingerprints contain only structural filesystem metadata.
type Entry = contracts.FileEntry

func fingerprint(i os.FileInfo) string {
	s := i.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Mtim.Sec, s.Mtim.Nsec, s.Ctim.Sec, s.Ctim.Nsec)
}
func (w *Workspace) Inspect(name string) (Entry, error) {
	f, e := w.openSource(name)
	if e != nil {
		return Entry{}, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil {
		return Entry{}, e
	}
	return Entry{Name: name, Directory: i.IsDir(), Regular: i.Mode().IsRegular()}, nil
}
func (w *Workspace) ListDirectory(ctx context.Context, name string, limit int) ([]Entry, string, error) {
	f, e := w.openSource(name)
	if e != nil {
		return nil, "", e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return nil, "", e
	}
	if !before.IsDir() {
		return nil, "", contracts.ErrInvalid
	}
	var entries []Entry
	for {
		if e = ctx.Err(); e != nil {
			return nil, "", e
		}
		batch, err := f.ReadDir(128)
		for _, v := range batch {
			if len(entries) >= limit {
				return nil, "", contracts.ErrInvalid
			}
			if v.Type() != 0 && !v.IsDir() {
				return nil, "", contracts.ErrInvalid
			}
			fd, e := syscall.Openat(int(f.Fd()), v.Name(), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if e != nil {
				if errors.Is(e, syscall.ELOOP) {
					return nil, "", contracts.ErrInvalid
				}
				return nil, "", e
			}
			child := os.NewFile(uintptr(fd), v.Name())
			i, e := child.Stat()
			child.Close()
			if e != nil {
				return nil, "", e
			}
			if !i.IsDir() && !i.Mode().IsRegular() {
				return nil, "", contracts.ErrInvalid
			}
			entries = append(entries, Entry{Name: v.Name(), Directory: i.IsDir(), Regular: i.Mode().IsRegular()})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
	}
	after, e := f.Stat()
	if e != nil {
		return nil, "", e
	}
	if fingerprint(before) != fingerprint(after) {
		return nil, "", contracts.ErrInvalid
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, fingerprint(after), nil
}
func (w *Workspace) CheckDirectory(name, token string) error {
	f, e := w.openSource(name)
	if e != nil {
		return e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil {
		return e
	}
	if fingerprint(i) != token {
		return contracts.ErrInvalid
	}
	return nil
}
func (w *Workspace) RelativeSelection(selection string) (string, error) {
	absolute, e := filepath.Abs(selection)
	if e != nil {
		return "", e
	}
	relative, e := filepath.Rel(w.root.Name(), absolute)
	if e != nil || !filepath.IsLocal(relative) {
		return "", contracts.ErrInvalid
	}
	return filepath.ToSlash(relative), nil
}
func (w *Workspace) ReleaseSession() error {
	if w.session == nil {
		return nil
	}
	e := w.session.Close()
	if e == nil {
		w.session = nil
	}
	return e
}
func (w *Workspace) NewRun(id string) error {
	if !validName(id) || w.run != nil {
		return contracts.ErrInvalid
	}
	name := "runs/" + id
	if e := w.root.Mkdir(name, 0700); e != nil {
		return e
	}
	r, e := w.root.OpenRoot(name)
	if e != nil {
		return e
	}
	w.run = r
	return nil
}
func (w *Workspace) WriteRun(ctx context.Context, value any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	if len(b) > 16<<20 {
		return contracts.ErrInvalid
	}
	if w.run == nil {
		return errors.New("run not allocated")
	}
	return writeAtomic(ctx, w.run, "report.json", append(b, '\n'))
}
func writeAtomic(ctx context.Context, root *os.Root, name string, data []byte) (err error) {
	if i, e := root.Lstat(name); e == nil && !i.Mode().IsRegular() {
		return contracts.ErrInvalid
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	tmp := "." + name + ".tmp"
	f, e := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer func() {
		if err != nil {
			_ = root.Remove(tmp)
		}
	}()
	n, e := f.Write(data)
	if e == nil && n != len(data) {
		e = io.ErrShortWrite
	}
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	return root.Rename(tmp, name)
}

// RunOutput keeps the report directory and invocation lock alive after input and
// session handles close. It is transferred only once all producers have stopped.
type RunWriter interface {
	Write(context.Context, any) error
	Close() error
}

type RunOutput struct {
	root *os.Root
	lock *os.File
}

func (w *Workspace) TakeRunOutput() (RunWriter, error) {
	if w.run == nil || w.lock == nil {
		return nil, errors.New("run output unavailable")
	}
	out := &RunOutput{root: w.run, lock: w.lock}
	w.run = nil
	w.lock = nil
	return out, nil
}
func (o *RunOutput) Write(ctx context.Context, value any) error {
	if o.root == nil {
		return errors.New("run output closed")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	if len(b) > 16<<20 {
		return contracts.ErrInvalid
	}
	return writeAtomic(ctx, o.root, "report.json", append(b, '\n'))
}
func (o *RunOutput) Close() error {
	var errs []error
	if o.root != nil {
		errs = append(errs, o.root.Close())
		o.root = nil
	}
	if o.lock != nil {
		errs = append(errs, o.lock.Close())
		o.lock = nil
	}
	return errors.Join(errs...)
}
