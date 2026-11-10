package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"operator-dynamo/internal/contracts"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func reportFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	b, e := os.ReadFile("../../test/reports/test-mode-report.json")
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	json.Unmarshal(b, &v)
	return b, v["runId"].(string)
}
func TestReportCorpusAtomicReplaceAndUnsafeEntries(t *testing.T) {
	root := t.TempDir()
	w, e := Open(root)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	b, id := reportFixture(t)
	dir := filepath.Join(root, "runs", id)
	os.Mkdir(dir, 0700)
	path := filepath.Join(dir, "report.json")
	os.WriteFile(path, b, 0600)
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			p := filepath.Join(dir, "next")
			if os.WriteFile(p, b, 0600) != nil {
				return
			}
			if os.Rename(p, path) != nil {
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		raw, view, e := w.ReadReport(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		if string(raw) != string(b) || view["runId"] != id {
			t.Fatal("mixed report")
		}
	}
	wg.Wait()
	page, e := w.ListReports(context.Background(), "", 50)
	if e != nil || len(page.IDs) != 1 {
		t.Fatal(page, e)
	}
	os.Remove(path)
	os.Symlink(filepath.Join(root, "outside"), path)
	_, _, e = w.ReadReport(context.Background(), id)
	assertReportCode(t, e, 422)
	_, e = w.ListReports(context.Background(), "", 50)
	assertReportCode(t, e, 422)
	os.Remove(path)
	os.WriteFile(path, b, 0600)
	os.Link(path, filepath.Join(root, "linked"))
	_, _, e = w.ReadReport(context.Background(), id)
	assertReportCode(t, e, 422)
}
func assertReportCode(t *testing.T, e error, code int) {
	t.Helper()
	var r *contracts.ReadError
	if !errors.As(e, &r) || r.Status != code {
		t.Fatalf("%v want %d", e, code)
	}
}
