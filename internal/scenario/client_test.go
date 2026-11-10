package scenario

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPProtocolAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, id, content, body string
		wantError               bool
	}{{"valid", "id", "application/json", `{"apiVersion":"example.test/v1","kind":"Widget","spec":{"number":9007199254740995}}`, false}, {"missing-id", "", "application/json", `{}`, true}, {"bad-media", "id", "text/plain", `{}`, true}, {"bad-object", "id", "application/json", `{}`, true}, {"trailing", "id", "application/json", `{} {}`, true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Dynamo-Request-ID", tc.id)
				w.Header().Set("Content-Type", tc.content)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			g := NewHTTPGateway(strings.TrimPrefix(server.URL, "http://"))
			defer g.Close()
			res, e := g.Request(context.Background(), "GET", target(), nil)
			if (e != nil) != tc.wantError {
				t.Fatal(res, e)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	g := NewHTTPGateway(strings.TrimPrefix(server.URL, "http://"))
	defer g.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, e := g.Request(ctx, "GET", target(), nil)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}
func TestHTTPDoesNotFollowRedirectOrProxy(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	t.Setenv("HTTP_PROXY", other.URL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	g := NewHTTPGateway(strings.TrimPrefix(server.URL, "http://"))
	defer g.Close()
	if _, e := g.Request(context.Background(), "POST", target(), map[string]any{}); e == nil {
		t.Fatal("redirect accepted")
	}
	if hit {
		t.Fatal("redirect or environment proxy received fixture")
	}
}
