package workbench

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRawHeaderBoundariesAndPipelinedBodies(t *testing.T) {
	h := handler(t)
	s := observedServer(h)
	defer s.Close()
	for _, fragment := range []bool{false, true} {
		for _, newline := range []string{"\r\n", "\n"} {
			c, e := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
			if e != nil {
				t.Fatal(e)
			}
			c.SetDeadline(time.Now().Add(8 * time.Second))
			r := bufio.NewReader(c)
			var wire strings.Builder
			wants := []int{}
			for _, size := range []int{32768, 32769, 36029, 32768} {
				prefix := "Host: 198.51.100.10:18080" + newline
				// Repeated names and whitespace count even though parsing normalizes them.
				fields := strings.Repeat("X: a"+newline, 1000)
				padding := size - len(prefix) - len(fields) - len("Z:") - 2*len(newline)
				section := prefix + fields + "Z:" + strings.Repeat(" ", padding) + newline + newline
				if len(section) != size {
					t.Fatal(len(section), size)
				}
				wire.WriteString("GET /ui/app.js HTTP/1.1" + newline + section)
				code := 200
				if size > 32768 {
					code = 400
				}
				wants = append(wants, code)
				// A health body is ignored by the handler but drained by net/http. It
				// deliberately resembles a header/request; ingress must not parse it.
				body := "GET /ui/ HTTP/1.1\r\nHost: evil\r\n\r\n"
				wire.WriteString(fmt.Sprintf("GET /healthz HTTP/1.1\r\nHost: ignored\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
				wants = append(wants, 404) // this server contains only Workbench
			}
			done := make(chan error, 1)
			go func() {
				b := []byte(wire.String())
				step := len(b)
				if fragment {
					step = 7
				}
				for len(b) > 0 {
					n := step
					if n > len(b) {
						n = len(b)
					}
					_, e := c.Write(b[:n])
					if e != nil {
						done <- e
						return
					}
					b = b[n:]
				}
				done <- nil
			}()
			for i, want := range wants {
				response, e := http.ReadResponse(r, nil)
				if e != nil {
					t.Fatal(fragment, newline, i, e)
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				// Workbench alone rejects the unrelated Host first. The composed-health
				// counterpart is exercised in httpserver's transport test.
				if want == 404 {
					want = 403
				}
				if response.StatusCode != want {
					t.Fatal(fragment, newline, i, response.StatusCode, want)
				}
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			c.Close()
		}
	}
}
