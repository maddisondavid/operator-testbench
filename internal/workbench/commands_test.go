package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"operator-dynamo/internal/contracts"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type commandPorts struct {
	stubPorts
	calls atomic.Int32
	raw   string
}

func (p *commandPorts) ExecuteCommand(ctx context.Context, id string, c contracts.ResourceCommand) (contracts.CommandResult, error) {
	p.calls.Add(1)
	p.raw = string(c.Object)
	return contracts.CommandResult{SessionID: id, ActionID: c.ActionID, RequestID: "api-id", Status: 201, Disposition: "committed"}, nil
}
func commandHandler(t *testing.T) (*Handler, *commandPorts) {
	t.Helper()
	h := handler(t)
	p := &commandPorts{}
	h.catalog = p
	if e := h.EnableWrites("http://198.51.100.10:18080"); e != nil {
		t.Fatal(e)
	}
	return h, p
}

const commandJSON = `{"sessionId":"ABCDEFGHIJKLMNOPQRSTUVWXYZ","actionId":"one","operation":"create","apiVersion":"v1","kind":"ConfigMap","name":"test","namespace":"demo","object":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test","namespace":"demo"},"number":9007199254740993}}`

func TestCommandAdmissionAndExactRawJSON(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		body   string
		status int
	}{
		{name: "valid", status: 201},
		{name: "missing origin", change: func(r *http.Request) { r.Header.Del("Origin") }, status: 403},
		{name: "duplicate origin", change: func(r *http.Request) { r.Header.Add("Origin", r.Header.Get("Origin")) }, status: 403},
		{name: "duplicate marker", change: func(r *http.Request) { r.Header.Add("X-Dynamo-Write", "1") }, status: 403},
		{name: "wrong origin", change: func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, status: 403},
		{name: "missing marker", change: func(r *http.Request) { r.Header.Del("X-Dynamo-Write") }, status: 403},
		{name: "wrong media", change: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, status: 415},
		{name: "secret", body: strings.Replace(commandJSON, `"kind":"ConfigMap"`, `"kind":"Secret"`, -1), status: 403},
		{name: "duplicate field", body: strings.Replace(commandJSON, `"actionId":"one"`, `"actionId":"one","actionId":"two"`, 1), status: 400},
		{name: "unknown field", body: strings.Replace(commandJSON, `"actionId":"one"`, `"actionId":"one","extra":true`, 1), status: 400},
		{name: "wrong session", body: strings.Replace(commandJSON, testID, "ZYXWVUTSRQPONMLKJIHGFEDCBAZ", 1), status: 400},
		{name: "envelope limit", body: strings.Repeat(" ", commandLimit+1), status: 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := commandHandler(t)
			s := observedServer(h)
			defer s.Close()
			body := tc.body
			if body == "" {
				body = commandJSON
			}
			r, _ := http.NewRequest("POST", s.URL+"/ui/api/v1/sessions/"+testID+"/commands", strings.NewReader(body))
			r.Host = "198.51.100.10:18080"
			r.Header.Set("Origin", "http://198.51.100.10:18080")
			r.Header.Set("X-Dynamo-Write", "1")
			r.Header.Set("Content-Type", "application/json")
			if tc.change != nil {
				tc.change(r)
			}
			res, e := s.Client().Do(r)
			if e != nil {
				t.Fatal(e)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode != tc.status {
				t.Fatalf("%d %s", res.StatusCode, b)
			}
			if tc.status == 201 {
				var out map[string]any
				if json.Unmarshal(b, &out) != nil || !strings.Contains(p.raw, "9007199254740993") {
					t.Fatal(string(b), p.raw)
				}
			} else if p.calls.Load() != 0 {
				t.Fatal("rejected request reached API")
			}
		})
	}
}
func TestCommandStalledUploadDeadline(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint(chunked), func(t *testing.T) {
			h, p := commandHandler(t)
			s := observedServer(h)
			defer s.Close()
			c, e := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			framing := "Content-Length: 200"
			body := "{"
			if chunked {
				framing = "Transfer-Encoding: chunked"
				body = "c8\r\n{"
			}
			start := time.Now()
			fmt.Fprintf(c, "POST /ui/api/v1/sessions/%s/commands HTTP/1.1\r\nHost: 198.51.100.10:18080\r\nOrigin: http://198.51.100.10:18080\r\nX-Dynamo-Write: 1\r\nContent-Type: application/json\r\n%s\r\n\r\n%s", testID, framing, body)
			r, e := http.ReadResponse(bufio.NewReader(c), nil)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Body.Close()
			b, _ := io.ReadAll(r.Body)
			if r.StatusCode != 504 || time.Since(start) > 2200*time.Millisecond || p.calls.Load() != 0 {
				t.Fatal(r.StatusCode, string(b), time.Since(start), p.calls.Load())
			}
			if len(h.slots) != 0 {
				t.Fatal("slot retained")
			}
		})
	}
}

func TestCommandDisabledAndPressureDoNotReachAPI(t *testing.T) {
	for _, mode := range []string{"disabled", "capacity", "rate"} {
		t.Run(mode, func(t *testing.T) {
			h, p := commandHandler(t)
			expected := 429
			switch mode {
			case "disabled":
				h.writeAuthority = ""
				expected = 403
			case "capacity":
				for i := 0; i < 8; i++ {
					h.slots <- struct{}{}
				}
			case "rate":
				h.tokens = 0
				h.last = time.Now().Add(time.Hour)
			}
			s := observedServer(h)
			defer s.Close()
			r, _ := http.NewRequest("POST", s.URL+"/ui/api/v1/sessions/"+testID+"/commands", strings.NewReader(commandJSON))
			r.Host = "198.51.100.10:18080"
			r.Header.Set("Origin", "http://198.51.100.10:18080")
			r.Header.Set("X-Dynamo-Write", "1")
			r.Header.Set("Content-Type", "application/json")
			res, err := s.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			io.Copy(io.Discard, res.Body)
			if res.StatusCode != expected || p.calls.Load() != 0 {
				t.Fatal(res.StatusCode, p.calls.Load())
			}
		})
	}
}

func TestEarlyAdmissionDoesNotDrainStalledUploads(t *testing.T) {
	for _, tc := range []struct {
		name, host, origin, path, method string
		chunked                          bool
		code                             int
	}{
		{"bad-host", "evil.test", "http://198.51.100.10:18080", "commands", "POST", false, 403},
		{"cross-origin", "198.51.100.10:18080", "http://evil.test", "commands", "POST", false, 403},
		{"null-origin-chunked", "198.51.100.10:18080", "null", "commands", "POST", true, 403},
		{"bad-path", "198.51.100.10:18080", "http://198.51.100.10:18080", "%2e%2e/commands", "POST", true, 400},
		{"large-headers", "198.51.100.10:18080", "http://198.51.100.10:18080", "commands", "POST", false, 400},
		{"get-body", "198.51.100.10:18080", "http://198.51.100.10:18080", "objects", "GET", false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := commandHandler(t)
			s := observedServer(h)
			defer s.Close()
			c, e := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			framing, body := "Content-Length: 200", "{"
			if tc.chunked {
				framing, body = "Transfer-Encoding: chunked", "c8\r\n{"
			}
			if tc.name == "large-headers" {
				framing += "\r\nX-Noise: " + strings.Repeat("v", 33000)
			}
			start := time.Now()
			fmt.Fprintf(c, "%s /ui/api/v1/sessions/%s/%s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/json\r\nX-Dynamo-Write: 1\r\n%s\r\n\r\n%s", tc.method, testID, tc.path, tc.host, tc.origin, framing, body)
			r, e := http.ReadResponse(bufio.NewReader(c), nil)
			if e != nil {
				t.Fatal(e)
			}
			b, e := io.ReadAll(r.Body)
			r.Body.Close()
			if e != nil || r.StatusCode != tc.code || time.Since(start) > time.Second || p.calls.Load() != 0 {
				t.Fatal(r.StatusCode, string(b), e, time.Since(start), p.calls.Load())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			if e := s.Config.Shutdown(ctx); e != nil {
				t.Fatal("unread upload blocked shutdown", e)
			}
		})
	}
}
