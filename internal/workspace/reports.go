package workspace

import (
	"context"
	"errors"
	"io"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/reportview"
	"os"
	"sort"
	"syscall"
)

func reportError(e error) error {
	if errors.Is(e, os.ErrNotExist) {
		return contracts.ReadFailure(404, "report_not_found")
	}
	if errors.Is(e, contracts.ErrInvalid) {
		return contracts.ReadFailure(422, "invalid_report")
	}
	return contracts.ReadFailure(500, "report_io_failed")
}
func reportStat(f *os.File) (os.FileInfo, error) {
	i, e := f.Stat()
	if e != nil {
		return nil, reportError(e)
	}
	s, ok := i.Sys().(*syscall.Stat_t)
	if !i.Mode().IsRegular() || !ok || s.Nlink != 1 {
		return nil, contracts.ReadFailure(422, "invalid_report")
	}
	return i, nil
}
func (w *Workspace) ListReports(ctx context.Context, after string, limit int) (contracts.RunPage, error) {
	p := contracts.RunPage{IDs: []string{}, NextAfter: after}
	if limit < 1 || limit > 100 || after != "" && !contracts.ValidID(after) {
		return p, contracts.ReadFailure(400, "invalid_query")
	}
	root, e := w.openSource("runs")
	if errors.Is(e, os.ErrNotExist) {
		return p, nil
	}
	if e != nil {
		return p, reportError(e)
	}
	defer root.Close()
	ids := []string{}
	visited := 0
	for {
		if e := ctx.Err(); e != nil {
			return p, e
		}
		entries, e := root.ReadDir(128)
		if e != nil && e != io.EOF {
			return p, reportError(e)
		}
		for _, entry := range entries {
			visited++
			if visited > 10000 {
				return p, contracts.ReadFailure(413, "catalog_capacity")
			}
			id := entry.Name()
			if !contracts.ValidID(id) {
				continue
			}
			if entry.Type().IsRegular() {
				continue
			}
			if !entry.IsDir() {
				return p, contracts.ReadFailure(422, "invalid_report")
			}
			f, e := w.openSource("runs/" + id + "/report.json")
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return p, reportError(e)
			}
			_, e = reportStat(f)
			f.Close()
			if e != nil {
				return p, e
			}
			if id > after {
				ids = append(ids, id)
			}
		}
		if e == io.EOF {
			break
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	p.IDs = ids
	if len(ids) > 0 {
		p.NextAfter = ids[len(ids)-1]
	}
	return p, nil
}
func (w *Workspace) ReadReport(ctx context.Context, id string) ([]byte, map[string]any, error) {
	if !contracts.ValidID(id) {
		return nil, nil, contracts.ReadFailure(400, "invalid_id")
	}
	var f *os.File
	var before os.FileInfo
	for {
		if e := ctx.Err(); e != nil {
			return nil, nil, e
		}
		var e error
		f, e = w.openSource("runs/" + id + "/report.json")
		if e != nil {
			return nil, nil, reportError(e)
		}
		before, e = f.Stat()
		if e != nil {
			f.Close()
			return nil, nil, reportError(e)
		}
		stat, ok := before.Sys().(*syscall.Stat_t)
		if ok && stat.Nlink == 0 {
			f.Close()
			continue
		}
		if !ok || !before.Mode().IsRegular() || stat.Nlink != 1 {
			f.Close()
			return nil, nil, contracts.ReadFailure(422, "invalid_report")
		}
		break
	}
	defer f.Close()
	if before.Size() > 16<<20 {
		return nil, nil, contracts.ReadFailure(413, "representation_too_large")
	}
	data := make([]byte, int(before.Size()))
	for offset := 0; offset < len(data); {
		if e := ctx.Err(); e != nil {
			return nil, nil, e
		}
		end := offset + 32768
		if end > len(data) {
			end = len(data)
		}
		n, e := io.ReadFull(f, data[offset:end])
		offset += n
		if e != nil {
			return nil, nil, contracts.ReadFailure(409, "report_changed")
		}
	}
	after, e := f.Stat()
	if e != nil {
		return nil, nil, reportError(e)
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	// A rename may unlink the pinned old inode. Its content remains a valid snapshot.
	if !os.SameFile(before, after) || before.Size() != after.Size() || a.Mtim != b.Mtim || b.Nlink > 1 || (b.Nlink == 1 && a.Ctim != b.Ctim) {
		return nil, nil, contracts.ReadFailure(409, "report_changed")
	}
	view, e := reportview.Validate(ctx, data, id)
	if e != nil {
		return nil, nil, e
	}
	return data, view, nil
}
