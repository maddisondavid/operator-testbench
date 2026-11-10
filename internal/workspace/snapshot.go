package workspace

import (
	"context"
	"errors"
	"io"
	"operator-dynamo/internal/contracts"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// openSource traverses directory descriptors with O_NOFOLLOW at every component.
func (w *Workspace) openSource(name string) (*os.File, error) {
	if !filepath.IsLocal(name) || strings.ContainsRune(name, 0) {
		return nil, contracts.ErrInvalid
	}
	current, err := w.root.Open(".")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for i, p := range parts {
		flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		fd, e := syscall.Openat(int(current.Fd()), p, flags, 0)
		current.Close()
		if e != nil {
			if errors.Is(e, syscall.ELOOP) || errors.Is(e, syscall.ENOTDIR) {
				return nil, contracts.ErrInvalid
			}
			return nil, e
		}
		current = os.NewFile(uintptr(fd), p)
	}
	return current, nil
}
func readSnapshot(ctx context.Context, f *os.File, limit int64) (contracts.InputFile, error) {
	var result contracts.InputFile
	info, err := f.Stat()
	if err != nil {
		return result, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Size() > limit {
		return result, contracts.ErrInvalid
	}
	var data []byte
	buf := make([]byte, 32768)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n, e := f.Read(buf)
		data = append(data, buf[:n]...)
		if int64(len(data)) > limit {
			return result, contracts.ErrInvalid
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return result, e
		}
	}
	after, err := f.Stat()
	if err != nil {
		return result, err
	}
	a := after.Sys().(*syscall.Stat_t)
	if after.Size() != info.Size() || int64(len(data)) != info.Size() || !os.SameFile(info, after) || a.Mtim != stat.Mtim || a.Ctim != stat.Ctim || a.Nlink != 1 {
		return result, contracts.ErrInvalid
	}
	return contracts.InputFile{Mode: int64(info.Mode().Perm() & 0777), Data: data}, nil
}
func (w *Workspace) ReadSource(ctx context.Context, name string, limit int64) (contracts.InputFile, error) {
	f, e := w.openSource(name)
	if e != nil {
		return contracts.InputFile{}, e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil {
		return contracts.InputFile{}, e
	}
	v, e := readSnapshot(ctx, f, limit)
	if e == nil {
		current, err := w.openSource(name)
		if err != nil {
			return contracts.InputFile{}, contracts.ErrInvalid
		}
		after, err := current.Stat()
		current.Close()
		if err != nil || !os.SameFile(before, after) {
			return contracts.InputFile{}, contracts.ErrInvalid
		}
	}
	v.Name = name
	return v, e
}
func (w *Workspace) Snapshot(ctx context.Context, name string, include func(string, bool) (bool, error)) ([]contracts.InputFile, error) {
	root, e := w.openSource(name)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	st, e := root.Stat()
	if e != nil {
		return nil, e
	}
	if !st.IsDir() {
		return nil, contracts.ErrInvalid
	}
	var files []contracts.InputFile
	visited, included := 0, 0
	var total int64
	var walk func(*os.File, string, int) error
	walk = func(dir *os.File, prefix string, depth int) error {
		if depth > 64 {
			return contracts.ErrInvalid
		}
		var entries []os.DirEntry
		for {
			if e := ctx.Err(); e != nil {
				return e
			}
			batch, e := dir.ReadDir(128)
			visited += len(batch)
			if visited > 100000 {
				return contracts.ErrInvalid
			}
			entries = append(entries, batch...)
			if e == io.EOF {
				break
			}
			if e != nil {
				return e
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			if e := ctx.Err(); e != nil {
				return e
			}
			p := entry.Name()
			if prefix != "" {
				p = prefix + "/" + p
			}
			if strings.Count(p, "/")+1 > 64 {
				return contracts.ErrInvalid
			}
			selected, e := include(p, entry.IsDir())
			if e != nil {
				return e
			}
			if selected {
				included++
				if included > 20000 {
					return contracts.ErrInvalid
				}
			}
			if !entry.IsDir() && !selected {
				continue
			}
			flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC
			if entry.IsDir() {
				flags |= syscall.O_DIRECTORY
			}
			fd, e := syscall.Openat(int(dir.Fd()), entry.Name(), flags, 0)
			if e != nil {
				if errors.Is(e, syscall.ELOOP) || errors.Is(e, syscall.ENOTDIR) {
					return contracts.ErrInvalid
				}
				return e
			}
			f := os.NewFile(uintptr(fd), p)
			if entry.IsDir() {
				if selected {
					info, err := f.Stat()
					if err != nil {
						f.Close()
						return err
					}
					files = append(files, contracts.InputFile{Name: p, Dir: true, Mode: int64(info.Mode().Perm() & 0777)})
				}
				e = walk(f, p, depth+1)
			} else {
				var v contracts.InputFile
				v, e = readSnapshot(ctx, f, (256<<20)-total)
				if e == nil {
					v.Name = p
					files = append(files, v)
					total += int64(len(v.Data))
				}
			}
			f.Close()
			if e != nil {
				return e
			}
		}
		return nil
	}
	e = walk(root, "", 0)
	return files, e
}
func (w *Workspace) WriteOutput(ctx context.Context, name string, data []byte) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	limit := 4 << 20
	if name == "operator-build-policy.txt" {
		limit = 256 << 20
	}
	if len(data) > limit {
		return contracts.ErrInvalid
	}
	return w.WriteArtifact(name, data)
}

// AppendOutput supports only private, bounded regular session artifacts.
func (w *Workspace) AppendOutput(ctx context.Context, name string, data []byte) (result error) {
	if e := ctx.Err(); e != nil {
		return e
	}
	if !validName(name) || w.session == nil {
		return contracts.ErrInvalid
	}
	f, e := w.session.OpenFile(name, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Sys().(*syscall.Stat_t).Nlink != 1 || st.Size()+int64(len(data)) > 4<<20 {
		return contracts.ErrInvalid
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	n, e := f.Write(data)
	if e == nil && n != len(data) {
		e = io.ErrShortWrite
	}
	return e
}
