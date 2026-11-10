package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"operator-dynamo/internal/cli"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpserver"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"testing"
	"time"
)

type memoryWorkspace struct {
	metadata                   []contracts.Metadata
	closed                     bool
	newErr, writeErr, closeErr error
}

func (w *memoryWorkspace) NewSession(string) error { return w.newErr }
func (w *memoryWorkspace) WriteMetadata(m contracts.Metadata) error {
	w.metadata = append(w.metadata, m)
	return w.writeErr
}
func (w *memoryWorkspace) CreateJournal() (*os.File, error) { return nil, errors.New("unused") }
func (w *memoryWorkspace) Close() error                     { w.closed = true; return w.closeErr }

type memoryJournal struct {
	states              []string
	failState           string
	appendErr, closeErr error
	closed              bool
}

func (j *memoryJournal) Append(state string) error {
	j.states = append(j.states, state)
	if state == j.failState {
		return j.appendErr
	}
	return nil
}
func (j *memoryJournal) Close() error { j.closed = true; return j.closeErr }

type stub struct {
	name              string
	calls             *[]string
	session           contracts.Session
	startErr, stopErr error
	start             func(context.Context) error
	stop              func(context.Context) error
	ready             func(bool)
	acquired          bool
}

func (s *stub) Start(ctx context.Context, session contracts.Session) error {
	s.session = session
	s.acquired = true
	*s.calls = append(*s.calls, "start "+s.name)
	if s.start != nil {
		return s.start(ctx)
	}
	return s.startErr
}
func (s *stub) Stop(ctx context.Context) error {
	if !s.acquired {
		return nil
	}
	s.acquired = false
	*s.calls = append(*s.calls, "stop "+s.name)
	if s.stop != nil {
		return s.stop(ctx)
	}
	return s.stopErr
}
func (s *stub) SetReady(b bool) {
	if s.ready != nil {
		s.ready(b)
	}
}
func fixture(cs ...Component) (*Application, *memoryWorkspace, *memoryJournal) {
	w := &memoryWorkspace{}
	j := &memoryJournal{}
	return &Application{OpenWorkspace: func(string) (Workspace, error) { return w, nil }, OpenJournal: func(Workspace, string) (Journal, error) { return j, nil }, Components: func(contracts.Options) []Component { return cs }, CleanupTimeout: 100 * time.Millisecond}, w, j
}
func TestLifecycleOrderIdentityAndIsolation(t *testing.T) {
	var prior string
	for run := 0; run < 2; run++ {
		var calls []string
		ctx, cancel := context.WithCancel(context.Background())
		a := &stub{name: "a", calls: &calls}
		b := &stub{name: "b", calls: &calls, ready: func(ready bool) {
			if ready {
				cancel()
			}
		}}
		application, w, j := fixture(a, b)
		if err := application.Run(ctx, contracts.Options{}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(calls, []string{"start a", "start b", "stop b", "stop a"}) {
			t.Fatal(calls)
		}
		if a.session.ID == "" || a.session.ID != b.session.ID || a.session.ID == prior {
			t.Fatal("session identity", a.session, b.session)
		}
		prior = a.session.ID
		if !reflect.DeepEqual(j.states, []string{"starting", "ready", "stopping", "stopped"}) || !w.closed || !j.closed {
			t.Fatal(j, w)
		}
		if w.metadata[len(w.metadata)-1].State != "stopped" || w.metadata[len(w.metadata)-1].EndedAt == nil {
			t.Fatal(w.metadata)
		}
		n := len(calls)
		if err := a.Stop(context.Background()); err != nil || len(calls) != n {
			t.Fatal("stop not idempotent")
		}
	}
}
func TestPartialStartupEveryPositionAndStopFailure(t *testing.T) {
	startErr := errors.New("start failed")
	stopErr := errors.New("stop failed")
	for failed := 0; failed < 3; failed++ {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var calls []string
			var components []Component
			var stubs []*stub
			for i := 0; i < 3; i++ {
				s := &stub{name: fmt.Sprint(i), calls: &calls}
				if i == failed {
					s.startErr = startErr
					s.stopErr = stopErr
				}
				stubs = append(stubs, s)
				components = append(components, s)
			}
			a, w, j := fixture(components...)
			err := a.Run(context.Background(), contracts.Options{})
			if !errors.Is(err, startErr) || !errors.Is(err, stopErr) {
				t.Fatal(err)
			}
			var want []string
			for i := 0; i <= failed; i++ {
				want = append(want, fmt.Sprint("start ", i))
			}
			for i := failed; i >= 0; i-- {
				want = append(want, fmt.Sprint("stop ", i))
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls, want)
			}
			for _, s := range stubs {
				if s.acquired {
					t.Fatal("resource leak")
				}
			}
			if !w.closed || !j.closed || w.metadata[len(w.metadata)-1].State != "failed" {
				t.Fatal(w, j)
			}
		})
	}
}
func TestJournalFailuresAndMetadataFailure(t *testing.T) {
	injected := errors.New("journal failure")
	for _, phase := range []string{"starting", "ready", "stopping", "stopped", "flush", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			var calls []string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &stub{name: "one", calls: &calls, ready: func(ready bool) {
				if ready {
					cancel()
				}
			}}
			a, w, j := fixture(s)
			if phase == "flush" {
				j.closeErr = injected
			} else if phase == "metadata" {
				w.writeErr = injected
			} else {
				j.failState = phase
				j.appendErr = injected
			}
			err := a.Run(ctx, contracts.Options{})
			if !errors.Is(err, injected) {
				t.Fatal(err)
			}
			if !w.closed || s.acquired {
				t.Fatal("cleanup incomplete")
			}
			if phase != "metadata" && (!j.closed || w.metadata[len(w.metadata)-1].State != "failed") {
				t.Fatal(w, j)
			}
		})
	}
}
func TestCleanupDeadlineAndStartupCancellation(t *testing.T) {
	var calls []string
	ctx, cancel := context.WithCancel(context.Background())
	s := &stub{name: "one", calls: &calls, start: func(ctx context.Context) error { cancel(); <-ctx.Done(); return ctx.Err() }, stop: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
	a, w, _ := fixture(s)
	start := time.Now()
	err := a.Run(ctx, contracts.Options{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal(err, time.Since(start))
	}
	if !w.closed || s.acquired {
		t.Fatal("cleanup incomplete")
	}
}
func TestBlockedStartupReadiness(t *testing.T) {
	server := httpserver.New("127.0.0.1:0")
	entered := make(chan struct{})
	release := make(chan struct{})
	ready := make(chan struct{})
	var once sync.Once
	var calls []string
	blocker := &stub{name: "blocked", calls: &calls, start: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, ready: func(b bool) {
		if b {
			once.Do(func() { close(ready) })
		}
	}}
	a, _, _ := fixture(server, blocker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, contracts.Options{}) }()
	<-entered
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	check := func(want int) {
		t.Helper()
		response, err := client.Get("http://" + server.Address() + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("readiness %d want %d", response.StatusCode, want)
		}
	}
	check(503)
	close(release)
	<-ready
	check(200)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("run leaked")
	}
	listener, err := net.Listen("tcp", server.Address())
	if err != nil {
		t.Fatal("listener leaked", err)
	}
	listener.Close()
}
func TestBindFailureReleasesWorkspace(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a, w, j := fixture(httpserver.New(listener.Addr().String()))
	if err = a.Run(context.Background(), contracts.Options{}); err == nil {
		t.Fatal("expected bind failure")
	}
	if !w.closed || !j.closed {
		t.Fatal("cleanup missing")
	}
}

type failureStub struct {
	*stub
	failures chan error
}

func (s *failureStub) Failures() <-chan error { return s.failures }
func TestRuntimeFailurePropagatesAndCleansUp(t *testing.T) {
	var calls []string
	injected := errors.New("serve failed")
	s := &failureStub{stub: &stub{name: "server", calls: &calls}, failures: make(chan error, 1)}
	s.ready = func(b bool) {
		if b {
			s.failures <- injected
		}
	}
	a, w, j := fixture(s)
	if err := a.Run(context.Background(), contracts.Options{}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if !w.closed || !j.closed || s.acquired {
		t.Fatal("leaked")
	}
}

func TestWorkspaceCloseFailureCorrectsTerminalState(t *testing.T) {
	var calls []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &stub{name: "one", calls: &calls, ready: func(b bool) {
		if b {
			cancel()
		}
	}}
	a, w, _ := fixture(s)
	injected := errors.New("workspace close failed")
	w.closeErr = injected
	if err := a.Run(ctx, contracts.Options{}); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if got := w.metadata[len(w.metadata)-1].State; got != "failed" {
		t.Fatalf("terminal state %s", got)
	}
}
func TestFailureCancelsBlockedDependentStartup(t *testing.T) {
	var calls []string
	entered := make(chan struct{})
	injected := errors.New("runtime startup failure")
	first := &failureStub{stub: &stub{name: "first", calls: &calls}, failures: make(chan error, 1)}
	second := &stub{name: "second", calls: &calls, start: func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }}
	a, w, _ := fixture(first, second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, contracts.Options{}) }()
	<-entered
	first.failures <- injected
	select {
	case err := <-done:
		if !errors.Is(err, injected) {
			t.Fatal(err)
		}
		if !w.closed || second.acquired || first.acquired {
			t.Fatal("cleanup incomplete")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dependent startup not cancelled")
	}
}
func TestSubprocessCleanupFailureExit(t *testing.T) {
	if os.Getenv("DYNAMO_TEST_CLEANUP_HELPER") == "1" {
		var calls []string
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := &stub{name: "one", calls: &calls, ready: func(b bool) {
			if b {
				cancel()
			}
		}, stopErr: errors.New("injected cleanup failure")}
		a, _, _ := fixture(s)
		os.Exit(cli.Execute(ctx, []string{"--workdir", "unused"}, os.Stderr, a))
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "-test.run=^TestSubprocessCleanupFailureExit$")
	command.Env = append(os.Environ(), "DYNAMO_TEST_CLEANUP_HELPER=1")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit=%v output=%s", err, output)
	}
}
