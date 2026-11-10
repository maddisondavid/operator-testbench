package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/uiaddress"
	"strconv"
	"strings"
	"sync"
	"time"
)

const progressQuota = 1 << 20
const truncationNotice = "Progress truncated; final outcome and report remain authoritative.\n"

type consoleOutput struct {
	sink              BoundedWriter
	release           func()
	life              context.Context
	cancel            context.CancelFunc
	queue             chan string
	done              chan struct{}
	mu                sync.Mutex
	closed, truncated bool
	bytes             int
	disabled          bool // serialized writer ownership: worker, then Finish after join
	once              sync.Once
}

func newOutput(s BoundedWriter, release func()) *consoleOutput {
	ctx, cancel := context.WithCancel(context.Background())
	o := &consoleOutput{sink: s, release: release, life: ctx, cancel: cancel, queue: make(chan string, 256), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		for line := range o.queue {
			o.write(o.life, []byte(line))
		}
	}()
	return o
}
func (o *consoleOutput) write(ctx context.Context, b []byte) {
	if o.disabled {
		return
	}
	c, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	n, e := o.sink.WriteContext(c, b)
	if e != nil || n != len(b) {
		o.disabled = true
		o.mu.Lock()
		o.truncated = true
		o.mu.Unlock()
		o.cancel()
	}
}
func safeID(v string) string {
	clipped := false
	if len(v) > 64 {
		v = v[:64]
		clipped = true
	}
	q := strconv.QuoteToASCII(v)
	for len(q) > 220 {
		v = v[:len(v)-1]
		clipped = true
		q = strconv.QuoteToASCII(v)
	}
	if clipped {
		q += "[truncated]"
	}
	return q
}
func renderProgress(p contracts.Progress) string {
	if p.Kind == contracts.UIAvailable {
		origin, e := uiaddress.Origin(p.Name)
		if e != nil {
			return ""
		}
		line := "Workbench available: " + origin + "/ui/"
		host, _, _ := net.SplitHostPort(p.Step)
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			line += " (wildcard listener)"
		}
		return line + "\n"
	}
	messages := map[contracts.ProgressKind]string{
		contracts.Validating: "Validating configuration", contracts.ValidatingTests: "Validating configuration and tests", contracts.Selected: "Selected scenarios",
		contracts.Preparing: "Preparing operator image", contracts.BuildStarting: "Building operator image", contracts.BuildComplete: "Operator image built",
		contracts.ContainerStarting: "Starting operator container", contracts.ContainerStarted: "Operator container started", contracts.ConnectionWaiting: "Waiting for operator connection", contracts.OperatorConnected: "Operator connected",
		contracts.ScenarioRunning: "Running scenario", contracts.Creating: "Creating resource", contracts.Updating: "Updating resource", contracts.Deleting: "Deleting resource", contracts.Asserting: "Waiting for assertion",
		contracts.StepPassed: "Step passed", contracts.StepFailed: "Step failed", contracts.CleanupStarting: "Stopping session and cleaning up", contracts.CleanupComplete: "Cleanup complete", contracts.CleanupFailed: "Cleanup failed", contracts.PreparationFailed: "Preparation failed", contracts.Cancelled: "Cancellation observed", contracts.HTTPReady: "Application HTTP ready",
	}
	message, ok := messages[p.Kind]
	if !ok {
		return ""
	}
	if p.Kind == contracts.Selected && p.Total >= 0 && p.Total <= 100 {
		message = fmt.Sprintf("Selected %d scenarios", p.Total)
	}
	if p.Kind == contracts.Asserting && p.Wait > 0 && p.Wait <= 10*time.Minute {
		message += fmt.Sprintf(" (up to %s)", p.Wait)
	}
	prefix := ""
	if p.Index > 0 && p.Index <= 100 && p.Total >= p.Index && p.Total <= 100 {
		prefix = fmt.Sprintf("[%d/%d %s", p.Index, p.Total, safeID(p.Name))
		if p.Step != "" {
			prefix += "/" + safeID(p.Step)
		}
		prefix += "] "
	}
	line := prefix + message + "\n"
	if len(line) > 1024 {
		return ""
	}
	return line
}
func (o *consoleOutput) Observe(p contracts.Progress) {
	o.mu.Lock()
	stopped := o.closed || o.truncated
	o.mu.Unlock()
	if stopped {
		return
	}
	// Only bounded, copied rendered fields enter the queue; no original string retained.
	line := renderProgress(p)
	if line == "" {
		return
	}
	o.enqueue(line, true)
}
func (o *consoleOutput) enqueue(line string, progress bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.truncated {
		return
	}
	if progress && o.bytes+len(line) > progressQuota {
		o.truncated = true
		return
	}
	select {
	case o.queue <- strings.Clone(line):
		if progress {
			o.bytes += len(line)
		}
	default:
		o.truncated = true
	}
}
func (o *consoleOutput) Finish(ctx context.Context, render func(io.Writer)) {
	o.once.Do(func() {
		stop := context.AfterFunc(ctx, o.cancel)
		defer stop()
		if ctx.Err() != nil {
			o.cancel()
		}
		o.mu.Lock()
		o.closed = true
		close(o.queue)
		o.mu.Unlock()
		<-o.done // bounded sink contract guarantees joined writes, even on cancellation
		o.mu.Lock()
		truncated := o.truncated
		o.mu.Unlock()
		if truncated {
			o.write(ctx, []byte(truncationNotice))
		}
		render(finalWriter{o, ctx})
		o.cancel()
		o.release()
	})
}

type finalWriter struct {
	o   *consoleOutput
	ctx context.Context
}

func (w finalWriter) Write(b []byte) (int, error) { w.o.write(w.ctx, b); return len(b), nil }
