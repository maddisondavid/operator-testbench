package httpserver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"operator-dynamo/internal/contracts"
	"strings"
	"testing"
	"time"
)

func TestObservedHealthBodyDrainAndEarlyReturn(t *testing.T) {
	s := New("127.0.0.1:0")
	s.Workbench(func(context.Context, string) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v, ok := ObservedHeaders(r.Context())
			if !ok || v.Bytes != len("Host: local\r\n\r\n") {
				t.Errorf("bad evidence %+v %v", v, ok)
			}
			fmt.Fprint(w, "ui")
		}), nil
	})
	if e := s.Start(context.Background(), contracts.Session{}); e != nil {
		t.Fatal(e)
	}
	defer s.Stop(context.Background())
	c, e := net.Dial("tcp", s.Address())
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	body := "GET /ui/ HTTP/1.1\r\nHost: body\r\n\r\n"
	wire := fmt.Sprintf("GET /healthz HTTP/1.1\r\nHost: local\r\nContent-Length: %d\r\n\r\n%s", len(body), body) + "GET /ui/ HTTP/1.1\r\nHost: local\r\n\r\n" +
		fmt.Sprintf("GET /healthz HTTP/1.1\r\nHost: local\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(body), body) + "GET /ui/ HTTP/1.1\r\nHost: local\r\n\r\n"
	if _, e := io.WriteString(c, wire); e != nil {
		t.Fatal(e)
	}
	r := bufio.NewReader(c)
	for i := 0; i < 4; i++ {
		res, e := http.ReadResponse(r, nil)
		if e != nil {
			t.Fatal(i, e)
		}
		b, e := io.ReadAll(res.Body)
		res.Body.Close()
		if e != nil || res.StatusCode != 200 {
			t.Fatal(i, string(b), e, res.StatusCode)
		}
	}
}
func TestIngressHealthCost(t *testing.T) {
	// Measurement, not a performance claim. Both variants must retain the same
	// near-default header/body acceptance with the existing five-second timeout.
	for _, wrapped := range []bool{false, true} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); fmt.Fprint(w, "ok") })
		s := httptest.NewUnstartedServer(h)
		s.Config.ReadHeaderTimeout = 5 * time.Second
		if wrapped {
			s.Listener = ObserveHeaders(s.Config, s.Listener)
		}
		s.Start()
		for _, size := range []int{16, 900 << 10} {
			start := time.Now()
			for i := 0; i < 3; i++ {
				req, _ := http.NewRequest("GET", s.URL+"/healthz", strings.NewReader(strings.Repeat("b", size)))
				req.Header.Set("X", strings.Repeat("a", size))
				res, e := s.Client().Do(req)
				if e != nil {
					t.Fatal(wrapped, size, e)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != 200 {
					t.Fatal(res.StatusCode)
				}
			}
			t.Logf("wrapped=%v header/body=%d repetitions=3 elapsed=%s", wrapped, size, time.Since(start))
		}
		s.Close()
	}
}

func TestPostHealthPreamblePreservesHeaderEvidence(t *testing.T) {
	for _, prefix := range []string{"\r", "\n", "\r\n", "\r\n\r\n"} {
		for _, chunked := range []bool{false, true} {
			s := New("127.0.0.1:0")
			s.Workbench(func(context.Context, string) (http.Handler, error) {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					v, ok := ObservedHeaders(r.Context())
					if !ok {
						t.Error("missing ingress")
					}
					fmt.Fprint(w, v.Bytes)
				}), nil
			})
			if e := s.Start(context.Background(), contracts.Session{}); e != nil {
				t.Fatal(e)
			}
			c, e := net.Dial("tcp", s.Address())
			if e != nil {
				t.Fatal(e)
			}
			c.SetDeadline(time.Now().Add(5 * time.Second))
			body := "Content-Length: 4\r\n\r\nbody"
			if chunked {
				body = "Transfer-Encoding: chunked\r\n\r\n4\r\nbody\r\n0\r\n\r\n"
			}
			section := "Host: local\r\nX:" + strings.Repeat(" ", 32769-len("Host: local\r\nX:\r\n\r\n")) + "\r\n\r\n"
			io.WriteString(c, "POST /healthz HTTP/1.1\r\nHost: local\r\n"+body+prefix+"GET /ui/ HTTP/1.1\r\n"+section)
			br := bufio.NewReader(c)
			res, e := http.ReadResponse(br, nil)
			if e != nil {
				t.Fatal(prefix, chunked, e)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 405 {
				t.Fatal(res.StatusCode)
			}
			res, e = http.ReadResponse(br, nil)
			if e != nil {
				t.Fatal(prefix, chunked, e)
			}
			b, e := io.ReadAll(res.Body)
			res.Body.Close()
			if e != nil || string(b) != "32769" {
				t.Fatal(prefix, chunked, string(b), e)
			}
			c.Close()
			s.Stop(context.Background())
		}
	}
}
