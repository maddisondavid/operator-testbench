package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
)

type output struct {
	bytes.Buffer
	writeErr, syncErr, closeErr error
	short                       bool
	closed                      bool
}

func (o *output) Write(b []byte) (int, error) {
	if o.writeErr != nil {
		return 0, o.writeErr
	}
	if o.short {
		return 1, nil
	}
	return o.Buffer.Write(b)
}
func (o *output) Sync() error  { return o.syncErr }
func (o *output) Close() error { o.closed = true; return o.closeErr }
func TestEnvelopeIsolationAndOmission(t *testing.T) {
	t.Setenv("DYNAMO_TEST_SECRET", "secret-must-not-appear")
	for _, id := range []string{"one", "two"} {
		o := &output{}
		j := New(o, id)
		for _, state := range []string{"starting", "ready", "stopping", "stopped"} {
			if err := j.Append(state); err != nil {
				t.Fatal(err)
			}
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(o.String(), "secret-must-not-appear") {
			t.Fatal("secret leaked")
		}
		lines := strings.Split(strings.TrimSpace(o.String()), "\n")
		if len(lines) != 4 {
			t.Fatal(lines)
		}
		for i, line := range lines {
			var event contracts.Event
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal(err)
			}
			if event.SchemaVersion != 1 || event.SessionID != id || event.Sequence != uint64(i+1) || event.EventID == "" || event.Timestamp.IsZero() || event.Actor != "system" || event.CorrelationID != id || event.Payload.State == "" {
				t.Fatal(event)
			}
		}
		if err := j.Append("ready"); err == nil {
			t.Fatal("append after close")
		}
	}
}
func TestFailures(t *testing.T) {
	injected := errors.New("injected I/O failure")
	for _, o := range []*output{{writeErr: injected}, {syncErr: injected}, {closeErr: injected}, {short: true}} {
		j := New(o, "one")
		appendErr := j.Append("starting")
		closeErr := j.Close()
		if appendErr == nil && closeErr == nil {
			t.Fatal("failure hidden")
		}
		if !o.closed {
			t.Fatal("output not closed")
		}
		if o.short && !errors.Is(appendErr, io.ErrShortWrite) {
			t.Fatal(appendErr)
		}
	}
}
