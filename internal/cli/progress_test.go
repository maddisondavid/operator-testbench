package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"operator-dynamo/internal/contracts"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

type memorySink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (m *memorySink) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.Write(p)
}
func (m *memorySink) WriteContext(c context.Context, p []byte) (int, error) {
	if e := c.Err(); e != nil {
		return 0, e
	}
	return m.Write(p)
}
func (m *memorySink) String() string { m.mu.Lock(); defer m.mu.Unlock(); return m.b.String() }

type runFunc func(context.Context, contracts.Options) error

func (f runFunc) Run(c context.Context, o contracts.Options) error { return f(c, o) }

type forbiddenWriter struct{ calls int }

func (w *forbiddenWriter) Write([]byte) (int, error) { w.calls++; panic("unsupported write") }
func TestVerboseAdmissionAndSafeDiagnostics(t *testing.T) {
	for _, args := range [][]string{{"--verbose", "--help"}, {"--verbose", "--bad"}, {"--verbose=bad"}, {"--verbose=false", "--bad"}} {
		w := &forbiddenWriter{}
		r := &runner{}
		if c := Execute(context.Background(), args, w, r); c != 2 || r.calls != 0 || w.calls != 0 {
			t.Fatal(c, r.calls, w.calls)
		}
	}
	for _, flag := range []string{"--verbose", "--verbose=true", "--verbose=false"} {
		w := &memorySink{}
		r := runFunc(func(c context.Context, o contracts.Options) error {
			if o.Console != nil {
				o.Console.Observe.Emit(contracts.Progress{Kind: contracts.Validating})
			}
			return errors.New("SECRET_DAEMON_RESPONSE")
		})
		if c := Execute(context.Background(), []string{"--workdir", "x", flag}, w, r); c != 3 {
			t.Fatal(c)
		}
		if strings.Contains(w.String(), "SECRET") || strings.Contains(w.String(), "Validating") != (flag != "--verbose=false") {
			t.Fatal(w.String())
		}
	}
}
func fdFlags(t *testing.T, f *os.File) uintptr {
	t.Helper()
	raw, e := f.SyscallConn()
	if e != nil {
		t.Fatal(e)
	}
	var flags uintptr
	raw.Control(func(fd uintptr) { flags, _, _ = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0) })
	return flags
}
func fillSink(t *testing.T, s BoundedWriter) {
	t.Helper()
	f := s.(*fileSink)
	buf := make([]byte, 4096)
	for i := 0; i < 100000; i++ {
		_, e := syscall.Write(f.fd, buf)
		if e == syscall.EAGAIN {
			return
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	t.Fatal("not bounded")
}
func TestBlockedPipeAndEarlyOutput(t *testing.T) {
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	defer w.Close()
	before := fdFlags(t, w)
	s, close, e := admit(w)
	if e != nil {
		t.Fatal(e)
	}
	fillSink(t, s)
	close()
	for _, args := range [][]string{{"--verbose", "--help"}, {"--verbose", "--bad"}, {"--verbose=bad"}} {
		start := time.Now()
		runner := &runner{}
		Execute(context.Background(), args, w, runner)
		if time.Since(start) > 500*time.Millisecond || runner.calls != 0 {
			t.Fatal("early output hung")
		}
	}
	if fdFlags(t, w) != before {
		t.Fatal("caller flags changed")
	}
	// Cancellation deadline cuts an in-flight write and joins, without closing caller fd.
	s, close, e = admit(w)
	if e != nil {
		t.Fatal(e)
	}
	o := newOutput(s, close)
	o.Observe(contracts.Progress{Kind: contracts.Validating})
	c, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	o.Finish(c, func(w io.Writer) { fmt.Fprintln(w, "final") })
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("deadline reset")
	}
	select {
	case <-o.done:
	default:
		t.Fatal("writer not joined")
	}
	if _, e = w.Stat(); e != nil {
		t.Fatal("caller closed")
	}
}
func TestTTYIndependentHandleCancellation(t *testing.T) {
	master, e := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer master.Close()
	var unlock int32
	var number uint32
	raw, _ := master.SyscallConn()
	raw.Control(func(fd uintptr) {
		_, _, e1 := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
		_, _, e2 := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number)))
		if e1 != 0 || e2 != 0 {
			t.Fatal(e1, e2)
		}
	})
	slave, e := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_WRONLY|syscall.O_NOCTTY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer slave.Close()
	before := fdFlags(t, slave)
	s, close, e := admit(slave)
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	fillSink(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The PTY can move bytes between internal queues after the first EAGAIN.
	// Keep the master unread and exhaust any newly available room until cancellation.
	for e == nil {
		_, e = s.WriteContext(ctx, make([]byte, 4096))
	}
	if e == nil || time.Since(start) > 200*time.Millisecond {
		t.Fatal(e)
	}
	if fdFlags(t, slave) != before {
		t.Fatal("TTY flags changed")
	}
}

type heldSink struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	memorySink
}

func (s *heldSink) WriteContext(ctx context.Context, b []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.memorySink.WriteContext(ctx, b)
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func TestProgressPressureOrderingAndBounds(t *testing.T) {
	s := &heldSink{started: make(chan struct{}), release: make(chan struct{})}
	o := newOutput(s, func() {})
	o.Observe(contracts.Progress{Kind: contracts.Validating})
	<-s.started
	name, step := strings.Repeat("\n", 100000), strings.Repeat("\x00", 100000)
	for i := 0; i < 10000; i++ {
		o.Observe(contracts.Progress{Kind: contracts.Asserting, Index: 1, Total: 2, Name: name, Step: step})
	}
	if len(o.queue) > 256 {
		t.Fatal("queue")
	}
	close(s.release)
	c, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	o.Finish(c, func(w io.Writer) { fmt.Fprintln(w, "FINAL") })
	text := s.String()
	if strings.Count(text, truncationNotice) != 1 || !strings.HasSuffix(text, "FINAL\n") {
		t.Fatal("truncation/order")
	}
	for _, line := range strings.Split(text, "\n") {
		if len(line)+1 > 1024 {
			t.Fatal("line bound")
		}
	}
	before := text
	o.Observe(contracts.Progress{Kind: contracts.Validating})
	if s.String() != before {
		t.Fatal("post final progress")
	}
	// Text quota independently from queue capacity.
	m := &memorySink{}
	q := newOutput(m, func() {})
	q.mu.Lock()
	q.bytes = progressQuota - 1
	q.mu.Unlock()
	q.Observe(contracts.Progress{Kind: contracts.Validating})
	q.Finish(c, func(io.Writer) {})
	if m.String() != truncationNotice {
		t.Fatal(m.String())
	}
}

type partialSink struct{ calls int }

func (s *partialSink) WriteContext(context.Context, []byte) (int, error) {
	s.calls++
	return 1, io.ErrShortWrite
}
func TestPartialWriteDisablesAllFurtherOutput(t *testing.T) {
	s := &partialSink{}
	o := newOutput(s, func() {})
	o.Observe(contracts.Progress{Kind: contracts.Validating})
	o.Finish(context.Background(), func(w io.Writer) { fmt.Fprintln(w, "FINAL") })
	if s.calls != 1 {
		t.Fatal(s.calls)
	}
}
func TestBrokenPipeDoesNotOverrideDomainOutcome(t *testing.T) {
	r, w, _ := os.Pipe()
	r.Close()
	defer w.Close()
	run := &runner{err: &contracts.TestResult{Code: 1, Status: "failed", Reason: "assertion_timeout"}}
	if c := Execute(context.Background(), []string{"--verbose", "--workdir", "x", "--tests", "x/t"}, w, run); c != 1 || run.calls != 1 {
		t.Fatal(c, run.calls)
	}
}

func TestConcurrentProducerFinalizationAndParserBounds(t *testing.T) {
	m := &memorySink{}
	o := newOutput(m, func() {})
	var producers sync.WaitGroup
	for i := 0; i < 10; i++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for j := 0; j < 2000; j++ {
				o.Observe(contracts.Progress{Kind: contracts.Validating})
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	o.Finish(ctx, func(w io.Writer) { fmt.Fprintln(w, "FINAL") })
	producers.Wait()
	o.Finish(ctx, func(w io.Writer) { fmt.Fprintln(w, "DUPLICATE") })
	if !strings.HasSuffix(m.String(), "FINAL\n") || strings.Contains(m.String(), "DUPLICATE") {
		t.Fatal("final ordering")
	}
	var b cappedBuffer
	b.Write(bytes.Repeat([]byte("x"), 1<<20))
	if b.Len() != 64<<10 {
		t.Fatal(b.Len())
	}
}

func TestRegularFileAndUnsupportedDevices(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "console")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	before := fdFlags(t, f)
	if code := Execute(context.Background(), []string{"--verbose", "--help"}, f, &runner{}); code != 0 {
		t.Fatal(code)
	}
	if fdFlags(t, f) != before {
		t.Fatal("file flags changed")
	}
	b, e := os.ReadFile(f.Name())
	if e != nil || !bytes.Contains(b, []byte("verbose")) {
		t.Fatal(string(b), e)
	}
	null, e := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer null.Close()
	if c := Execute(context.Background(), []string{"--verbose", "--help"}, null, &runner{}); c != 2 {
		t.Fatal(c)
	}
}

type phaseSink struct {
	memorySink
	lines chan string
}

func (s *phaseSink) WriteContext(ctx context.Context, p []byte) (int, error) {
	n, e := s.memorySink.WriteContext(ctx, p)
	if e == nil {
		s.lines <- string(p)
	}
	return n, e
}
func TestMilestonesVisibleBeforeControlledPhaseRelease(t *testing.T) {
	s := &phaseSink{lines: make(chan string, 32)}
	phases := []contracts.ProgressKind{contracts.BuildStarting, contracts.ContainerStarting, contracts.ConnectionWaiting, contracts.Asserting}
	words := []string{"Building operator image", "Starting operator container", "Waiting for operator connection", "Waiting for assertion"}
	release := make(chan struct{})
	done := make(chan int, 1)
	r := runFunc(func(_ context.Context, o contracts.Options) error {
		for _, kind := range phases {
			o.Console.Observe.Emit(contracts.Progress{Kind: kind})
			<-release
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		o.Console.Complete(ctx, nil)
		return nil
	})
	go func() { done <- Execute(context.Background(), []string{"--verbose", "--workdir", "x"}, s, r) }()
	for _, word := range words {
		timeout := time.NewTimer(time.Second)
		found := false
		for !found {
			select {
			case line := <-s.lines:
				found = strings.Contains(line, word)
			case <-timeout.C:
				t.Fatal("phase start not live", word)
			}
		}
		timeout.Stop()
		release <- struct{}{}
	}
	if code := <-done; code != 0 {
		t.Fatal(code)
	}
}

func TestUIOnlyAdmissionAndTypedAddress(t *testing.T) {
	for _, args := range [][]string{{"--ui", "--bad"}, {"--ui=maybe"}, {"--ui", "--help"}, {"--ui-origin", "http://198.51.100.10:18080", "--workdir", "x"}} {
		w := &forbiddenWriter{}
		r := &runner{}
		if code := Execute(context.Background(), args, w, r); code != 2 || r.calls != 0 {
			t.Fatal(code, r.calls)
		}
	}
	out := &memorySink{}
	r := runFunc(func(ctx context.Context, o contracts.Options) error {
		if !o.UI {
			t.Fatal("UI missing")
		}
		o.Console.Observe.Emit(contracts.Progress{Kind: contracts.ContainerStarted})
		o.Console.Observe.Emit(contracts.Progress{Kind: contracts.UIAvailable, Name: "http://198.51.100.10:18080", Step: "0.0.0.0:8080"})
		o.Console.Complete(ctx, nil)
		return nil
	})
	if Execute(context.Background(), []string{"--workdir", "x", "--ui", "--verbose=false"}, out, r) != 0 {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "http://198.51.100.10:18080/ui/") || strings.Contains(out.String(), "Operator container started") {
		t.Fatal(out.String())
	}
}
