package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"operator-dynamo/internal/contracts"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dynamo-process-tests-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "dynamo")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "build: %s %v", out, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
func command(args ...string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode(), string(out)
	}
	return -1, err.Error()
}
func TestProcessArgumentFailuresNoSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "must-not-exist")
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{"--help", "--workdir", root}, 0}, {nil, 2}, {[]string{"--unknown"}, 2}, {[]string{"--workdir", root, "--ui-origin", "http://127.0.0.1:8080"}, 2}} {
		code, out := command(tc.args...)
		if code != tc.code {
			t.Fatal(code, out)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("workspace created", err)
		}
	}
}
func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

type child struct {
	cmd    *exec.Cmd
	done   chan error
	output bytes.Buffer
}

func start(t *testing.T, root, addr string, extra ...string) *child {
	t.Helper()
	c := &child{cmd: exec.Command(binary, "--workdir", root, "--listen", addr), done: make(chan error, 1)}
	c.cmd.Args = append(c.cmd.Args, extra...)
	c.cmd.Stdout = &c.output
	c.cmd.Stderr = &c.output
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	t.Cleanup(func() {
		if c.cmd.ProcessState == nil {
			c.cmd.Process.Kill()
			select {
			case <-c.done:
			case <-time.After(time.Second):
			}
		}
	})
	client := &http.Client{Timeout: 100 * time.Millisecond}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.done:
			t.Fatalf("early process exit: %v %s", err, c.output.String())
		default:
		}
		r, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("readiness deadline")
	return nil
}
func stop(t *testing.T, c *child, signal os.Signal) {
	t.Helper()
	begin := time.Now()
	if err := c.cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.done:
		if signal != os.Kill && err != nil {
			t.Fatalf("exit: %v %s", err, c.output.String())
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown exceeded budget")
	}
	if time.Since(begin) > 6*time.Second {
		t.Fatal("slow shutdown")
	}
}
func metadata(t *testing.T, root string) []contracts.Metadata {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "sessions", "*", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result []contracts.Metadata
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m contracts.Metadata
		if err = json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		result = append(result, m)
	}
	return result
}
func TestSignalsOwnershipAndSessionArtifacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	var firstID string
	for _, signal := range []os.Signal{syscall.SIGINT, syscall.SIGTERM} {
		addr := freeAddress(t)
		c := start(t, root, addr)
		begin := time.Now()
		code, out := command("--workdir", root, "--listen", "127.0.0.1:0")
		if code != 3 || !strings.Contains(out, "infrastructure failed") || time.Since(begin) > 2*time.Second {
			t.Fatal("ownership", code, out)
		}
		stop(t, c, signal)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal("listener leaked", err)
		}
		listener.Close()
		ms := metadata(t, root)
		for _, m := range ms {
			if m.State != "stopped" || m.EndedAt == nil {
				t.Fatal(m)
			}
			if firstID == "" {
				firstID = m.ID
			}
			b, err := os.ReadFile(filepath.Join(root, "sessions", m.ID, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) != 4 {
				t.Fatalf("events %s", b)
			}
			for i, line := range lines {
				var e contracts.Event
				if err = json.Unmarshal([]byte(line), &e); err != nil || e.Sequence != uint64(i+1) || e.SessionID != m.ID {
					t.Fatal(line, err)
				}
			}
		}
	}
	if len(metadata(t, root)) != 2 {
		t.Fatal("session ID reused")
	}
}
func TestCrashReleasesOwnershipPreservesIncompleteSession(t *testing.T) {
	root := t.TempDir()
	c := start(t, root, freeAddress(t))
	before := metadata(t, root)
	if len(before) != 1 {
		t.Fatal(before)
	}
	oldPath := filepath.Join(root, "sessions", before[0].ID, "session.json")
	old, _ := os.ReadFile(oldPath)
	stop(t, c, os.Kill)
	c = start(t, root, freeAddress(t))
	stop(t, c, syscall.SIGTERM)
	after, _ := os.ReadFile(oldPath)
	if !bytes.Equal(old, after) {
		t.Fatal("prior session changed")
	}
	ms := metadata(t, root)
	if len(ms) != 2 {
		t.Fatal(ms)
	}
	for _, m := range ms {
		if m.ID == before[0].ID && (m.State != "ready" || m.EndedAt != nil) {
			t.Fatal("crash presented as completion", m)
		}
	}
}
func TestBindFailureAndPermissionFailure(t *testing.T) {
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	code, out := command("--workdir", root, "--listen", listener.Addr().String())
	listener.Close()
	if code != 3 {
		t.Fatal(code, out)
	}
	ms := metadata(t, root)
	if len(ms) != 1 || ms[0].State != "failed" {
		t.Fatal(ms)
	}
	c := start(t, root, freeAddress(t))
	stop(t, c, syscall.SIGTERM)
	if os.Geteuid() == 0 {
		t.Fatal("process permission test requires non-root")
	}
	restricted := t.TempDir()
	os.Chmod(restricted, 0500)
	defer os.Chmod(restricted, 0700)
	code, out = command("--workdir", restricted)
	if code != 3 {
		t.Fatal(code, out)
	}
}

func TestProcessTestPreflight(t *testing.T) {
	root := t.TempDir()
	code, out := command("--workdir", root, "--tests", filepath.Join(root, "missing"))
	if code != 2 {
		t.Fatal(code, out)
	}
	for _, name := range []string{"sessions", "runs"} {
		entries, e := os.ReadDir(filepath.Join(root, name))
		if e != nil || len(entries) != 0 {
			t.Fatal(name, entries, e)
		}
	}
}

func TestVerboseHealthOnly(t *testing.T) {
	c := start(t, t.TempDir(), freeAddress(t), "--verbose")
	stop(t, c, syscall.SIGTERM)
	text := c.output.String()
	if !strings.Contains(text, "Application HTTP ready") || !strings.Contains(text, "Cleanup complete") || strings.Contains(text, "operator container") || strings.Contains(text, "Operator connected") {
		t.Fatal(text)
	}
}

func TestProcessReportOnlyWorkbench(t *testing.T) {
	root := t.TempDir()
	address := freeAddress(t)
	c := start(t, root, address, "--ui")
	client := &http.Client{Timeout: time.Second}
	for _, path := range []string{"/ui/", "/ui/app.js", "/ui/style.css", "/ui/api/v1/sessions", "/ui/api/v1/runs"} {
		res, e := client.Get("http://" + address + path)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(path, res.StatusCode)
		}
	}
	client.CloseIdleConnections()
	stop(t, c, syscall.SIGTERM)
	if !strings.Contains(c.output.String(), "Workbench available: http://"+address+"/ui/") {
		t.Fatal(c.output.String())
	}
	if strings.Contains(c.output.String(), "Validating configuration") {
		t.Fatal("UI-only emitted ordinary progress")
	}
}
