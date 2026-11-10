package operatorruntime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRedactionBoundariesAndDiscard(t *testing.T) {
	for _, step := range []int{1, 2, 7, 100000} {
		var lines [][]byte
		truncated := false
		r := newRedactor([][]byte{[]byte("secret"), []byte("secret-long"), []byte("across\nlines"), []byte("boundary\nSECRET")}, func(b []byte) { lines = append(lines, append([]byte(nil), b...)) }, func() { truncated = true })
		input := "secret-long and secret and across\nlines\n" + strings.Repeat("x", 65537) + "boundary\nSECRET visible\nlast"
		for i := 0; i < len(input); i += step {
			end := i + step
			if end > len(input) {
				end = len(input)
			}
			r.Write([]byte(input[i:end]))
		}
		r.End()
		output := string(bytes.Join(lines, []byte("\n")))
		if strings.Contains(output, "secret") || strings.Contains(output, "SECRET") || strings.Contains(output, "lines") || !strings.Contains(output, "[REDACTED] and [REDACTED] and [REDACTED]") || !strings.HasSuffix(output, "last") || !truncated {
			t.Fatal(step, output, truncated)
		}
	}
}
func framed(stream byte, data []byte) []byte {
	b := make([]byte, 8+len(data))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(data)))
	copy(b[8:], data)
	return b
}
func TestCaptureFramesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   []byte
		failed bool
	}{
		{"valid", append(append(framed(1, []byte("sec")), framed(2, []byte("stderr\n"))...), framed(1, []byte("ret\nlast\xff"))...), false},
		{"zero", framed(1, nil), false}, {"stream", framed(3, nil), true}, {"partialheader", []byte{1, 0}, true}, {"partialbody", framed(1, []byte("hello"))[:10], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var artifact bytes.Buffer
			c := &capture{source: io.NopCloser(bytes.NewReader(tc.data)), done: make(chan struct{}), patterns: [][]byte{[]byte("secret")}}
			c.run(context.Background(), func(_ context.Context, b []byte) error { artifact.Write(b); return nil })
			if (c.err != nil) != tc.failed {
				t.Fatal(c.err)
			}
			if strings.Contains(artifact.String(), "secret") {
				t.Fatal("secret leaked")
			}
			if tc.name == "valid" {
				var rows []outputRecord
				for _, line := range bytes.Split(bytes.TrimSpace(artifact.Bytes()), []byte("\n")) {
					var row outputRecord
					if e := json.Unmarshal(line, &row); e != nil {
						t.Fatal(e)
					}
					rows = append(rows, row)
				}
				if len(rows) != 3 || rows[0].Sequence != 1 {
					t.Fatal(rows)
				}
			}
		})
	}
	oversized := make([]byte, 8)
	oversized[0] = 1
	binary.BigEndian.PutUint32(oversized[4:], (1<<20)+1)
	c := &capture{source: io.NopCloser(bytes.NewReader(oversized)), done: make(chan struct{})}
	c.run(context.Background(), func(context.Context, []byte) error { return nil })
	if c.err == nil {
		t.Fatal("oversize frame accepted")
	}
}
func TestCaptureFloodSlowSinkAndWriteFailure(t *testing.T) {
	payload := bytes.Repeat(framed(1, append(bytes.Repeat([]byte("a"), 60000), '\n')), 150)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	c := &capture{source: io.NopCloser(bytes.NewReader(payload)), done: make(chan struct{})}
	first := true
	go c.run(ctx, func(ctx context.Context, b []byte) error {
		if first {
			first = false
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	<-entered
	// Wait for permanent quota truncation while the sink remains blocked.
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		truncated := c.truncated
		queued := c.queued
		c.mu.Unlock()
		if queued > 1<<20 {
			t.Fatal("queue unbounded")
		}
		if truncated {
			break
		}
		select {
		case <-deadline:
			t.Fatal("flood blocked reader")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	<-c.done
	if !c.truncated || c.err != nil {
		t.Fatal(c.err)
	}
	failed := &capture{source: io.NopCloser(bytes.NewReader(framed(1, []byte("line\n")))), done: make(chan struct{})}
	failed.run(context.Background(), func(context.Context, []byte) error { return errors.New("sink failure") })
	if failed.err == nil {
		t.Fatal("write failure ignored")
	}
}

func TestCaptureSinkCancellationJoinsWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	c := &capture{source: io.NopCloser(bytes.NewReader(framed(1, []byte("line\n")))), done: make(chan struct{})}
	go c.run(ctx, func(ctx context.Context, _ []byte) error { close(entered); <-ctx.Done(); return ctx.Err() })
	<-entered
	cancel()
	select {
	case <-c.done:
		if c.err == nil {
			t.Fatal("cancelled sink succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("writer abandoned")
	}
}

func TestCaptureExactArtifactCapAndCancelledMatcher(t *testing.T) {
	c := &capture{source: io.NopCloser(bytes.NewReader(framed(1, []byte("line\n")))), done: make(chan struct{}), bytes: (4 << 20) - 1}
	writes := 0
	c.run(context.Background(), func(context.Context, []byte) error { writes++; return nil })
	if writes != 0 || !c.truncated || c.records != 0 || c.bytes != (4<<20)-1 {
		t.Fatal("artifact cap not atomic")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := newRedactor([][]byte{bytes.Repeat([]byte("a"), 4096)}, func([]byte) { t.Fatal("cancelled matcher emitted") }, func() {})
	r.cancelled = func() bool { return ctx.Err() != nil }
	r.Write(bytes.Repeat([]byte("a"), 1<<20))
	if len(r.raw) != 0 || len(r.pending) != 0 {
		t.Fatal("cancelled matcher consumed input")
	}
}
