package journal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProjectionOmissionsAndEviction(t *testing.T) {
	w := NewWindows()
	w.Register("a")
	page, e := w.Page(context.Background(), "a", 0, 10)
	if e != nil || page.Gap || page.ObservedFirst != nil || page.ObservedLast != "0" {
		t.Fatal(page, e)
	}
	w.append("a", 1, []byte(`{"sequence":1,"payload":{"integer":9007199254740993}}`))
	w.append("a", 2, []byte(strings.Repeat("x", 65537)))
	p, e := w.Page(context.Background(), "a", 0, 10)
	if e != nil || !p.Gap || p.NextAfter != "2" || !strings.Contains(string(p.Events[0]), `"9007199254740993"`) {
		t.Fatal(p, e)
	}
	p, e = w.Page(context.Background(), "a", 1, 10)
	if e != nil || !p.Gap || len(p.Events) != 0 || p.NextAfter != "2" {
		t.Fatal(p, e)
	}
	for i := uint64(3); i <= 1100; i++ {
		b, _ := json.Marshal(map[string]any{"sequence": i})
		w.append("a", i, b)
	}
	p, e = w.Page(context.Background(), "a", 0, 200)
	if e != nil || !p.Gap || len(p.Events) != 200 || len(w.windows["a"].events) != 1024 {
		t.Fatal(p, e)
	}
	if _, e = w.Page(context.Background(), "a", 1101, 1); e == nil {
		t.Fatal("future cursor")
	}
}
func TestGlobalProjectionEvictsCompletedFirst(t *testing.T) {
	w := NewWindows()
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		w.Register(id)
		for n := uint64(1); n <= 16; n++ {
			w.append(id, n, []byte(strings.Repeat("x", 65536)))
		}
		w.Complete(id)
	}
	if w.bytes > 8<<20 || len(w.windows["a"].events) != 0 {
		t.Fatal(w.bytes)
	}
	p, e := w.Page(context.Background(), "a", 0, 100)
	if e != nil || p.RetainedFirst != nil || !p.Gap || p.NextAfter != "16" {
		t.Fatal(p, e)
	}
}

func TestOnlySuccessfulAppendsReachProjection(t *testing.T) {
	for _, failure := range []bool{false, true} {
		sink := &output{}
		if failure {
			sink.short = true
		}
		j := New(sink, "a")
		w := NewWindows()
		w.Register("a")
		j.SetProjection(w)
		e := j.Append("starting")
		p, readErr := w.Page(context.Background(), "a", 0, 10)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if failure {
			if e == nil || p.ObservedLast != "0" || len(p.Events) != 0 {
				t.Fatal(e, p)
			}
		} else if e != nil || p.ObservedLast != "1" || len(p.Events) != 1 {
			t.Fatal(e, p)
		}
	}
}
