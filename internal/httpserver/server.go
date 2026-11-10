// Package httpserver supplies infrastructure health endpoints, not a Kubernetes API.
package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpdrain"
)

type Server struct {
	workbench   func(context.Context, string) (http.Handler, error)
	cancelUI    context.CancelFunc
	connections httpdrain.Connections
	address     string
	ready       atomic.Bool
	mu          sync.Mutex
	server      *http.Server
	listener    net.Listener
	done        chan struct{}
	failures    chan error
}

func New(address string) *Server                                                  { return &Server{address: address, failures: make(chan error, 1)} }
func (s *Server) Workbench(f func(context.Context, string) (http.Handler, error)) { s.workbench = f }
func (s *Server) Start(ctx context.Context, _ contracts.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.server != nil {
		return errors.New("HTTP server already started")
	}
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	s.listener = listener
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeStatus(w, 200, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.ready.Load() {
			writeStatus(w, 200, "ready")
		} else {
			writeStatus(w, 503, "not-ready")
		}
	})
	health := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			mux.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	var handler http.Handler = health
	if s.workbench != nil {
		life, cancel := context.WithCancel(context.Background())
		s.cancelUI = cancel
		ui, e := s.workbench(life, listener.Addr().String())
		if e != nil {
			cancel()
			listener.Close()
			return e
		}
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ui" || strings.HasPrefix(r.URL.Path, "/ui/") {
				ui.ServeHTTP(w, r)
			} else {
				health.ServeHTTP(w, r)
			}
		})
	}
	s.server = &http.Server{ConnState: s.connections.State, Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	if s.workbench != nil {
		listener = ObserveHeaders(s.server, listener)
	}
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.failures <- err
		}
	}()
	return nil
}
func writeStatus(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("{\"status\":\"" + value + "\"}\n"))
}
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }
func (s *Server) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}
func (s *Server) Failures() <-chan error { return s.failures }
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SetReady(false)
	if s.server == nil {
		return nil
	}
	if s.cancelUI != nil {
		s.cancelUI()
	}
	s.connections.ClosePending()
	err := s.server.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, s.server.Close())
	}
	// Serve returns after listener close; joining it prevents a leaked serving goroutine.
	<-s.done
	return err
}
