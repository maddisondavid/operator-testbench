// Package operatorruntime owns the local Docker lifecycle, not API semantics.
package operatorruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"operator-dynamo/internal/contracts"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Engine interface {
	Call(context.Context, string, string, io.Reader, any) error
}
type Docker struct{ client *http.Client }
type HTTPError struct {
	Method string
	Status int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Docker %s failed (HTTP %d)", e.Method, e.Status)
}
func IsNotFound(err error) bool {
	var e *HTTPError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func LocalDocker() *Docker {
	return &Docker{client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
	}}}}
}

// Engine errors contain only operation/status, never daemon messages that may echo secrets.
func (d *Docker) Call(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.47"+path, body)
	if err != nil {
		return errors.New("invalid Docker request")
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.Contains(path, "/archive?") {
		req.Header.Set("Content-Type", "application/x-tar")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return errors.New("local Docker daemon unavailable or operation cancelled")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Method: method, Status: resp.StatusCode}
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return errors.New("invalid Docker response")
		}
	}
	return nil
}
func jsonBody(v any) io.Reader { b, _ := json.Marshal(v); return bytes.NewReader(b) }

type Runtime struct {
	Progress               contracts.ProgressObserver
	Prepared               *Prepared
	setupStarted, launched time.Time
	Config                 Config
	Endpoint               string
	Timeout                time.Duration
	Connected              <-chan struct{}
	Engine                 Engine
	Record                 func(contracts.Observation) error
	id, image, session     string
	helper, volume         string
	Inputs                 Inputs
	inputFiles             []contracts.InputFile
	buildInfo              *buildEvidence
	owner                  string
	containers, volumes    []string
	capture                *capture
	stopping               atomic.Bool
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	failures               chan error
}

func New(c Config, endpoint string, timeout time.Duration, connected <-chan struct{}, record func(contracts.Observation) error) *Runtime {
	return &Runtime{Config: c, owner: rand.Text(), Endpoint: endpoint, Timeout: timeout, Connected: connected, Engine: LocalDocker(), Record: record, failures: make(chan error, 1)}
}
func (r *Runtime) Failures() <-chan error { return r.failures }
func (r *Runtime) emit(state string) error {
	kinds := map[string]contracts.ProgressKind{"build.started": contracts.BuildStarting, "build.completed": contracts.BuildComplete, "started": contracts.ContainerStarted, "connected": contracts.OperatorConnected}
	if kind, ok := kinds[state]; ok {
		r.Progress.Emit(contracts.Progress{Kind: kind})
	}
	if r.Record == nil {
		return nil
	}
	payload := contracts.LifecyclePayload{State: state, ContainerID: r.id, ImageID: r.image}
	if state == "started" && !r.setupStarted.IsZero() {
		payload.DurationNS = time.Since(r.setupStarted).Nanoseconds()
	}
	if state == "connected" && !r.launched.IsZero() {
		payload.DurationNS = time.Since(r.launched).Nanoseconds()
	}
	if strings.HasPrefix(state, "build.") && r.buildInfo != nil {
		payload.ContextDigest = r.buildInfo.ContextDigest
		payload.BuildTag = r.buildInfo.Tag
		payload.ImageID = r.buildInfo.Image
		payload.Artifact = "operator-build.json"
	}
	if strings.HasPrefix(state, "capture.") {
		payload.Artifact = "operator-output.jsonl"
		if state != "capture.started" && r.capture != nil {
			payload.Bytes = r.capture.bytes
			payload.Records = r.capture.records
			payload.Truncated = r.capture.truncated
		}
	}
	if err := r.Record(contracts.Observation{Kind: "operator." + state, CorrelationID: r.session, Payload: payload}); err != nil {
		return errors.New("operator journal capture failed")
	}
	return nil
}
func (r *Runtime) Start(ctx context.Context, session contracts.Session) error {
	r.Progress.Emit(contracts.Progress{Kind: contracts.Preparing})
	r.session = session.ID
	if e := r.Config.Validate(); e != nil {
		return e
	}
	preparation := ctx
	finish := func() {}
	if r.Config.Build != nil {
		preparation, finish = context.WithTimeout(ctx, buildTimeout(r.Config))
	} else {
		preparation, finish = context.WithTimeout(ctx, r.Timeout)
	}
	prepareStart := time.Now()
	archive, e := r.preparedInputs(preparation)
	if r.buildInfo != nil {
		r.buildInfo.PreparationNS = time.Since(prepareStart).Nanoseconds()
	}
	if e != nil {
		finish()
		return e
	}
	reference := r.Config.Image
	if r.Config.Build != nil {
		reference, e = r.build(preparation, archive)
		if e != nil {
			finish()
			return e
		}
	}
	finish()
	r.setupStarted = time.Now()
	deadline, done := context.WithTimeout(ctx, r.Timeout)
	defer done()
	var image struct {
		ID     string `json:"Id"`
		Config struct{ Volumes map[string]any }
	}
	if e := r.Engine.Call(deadline, "GET", "/images/"+url.PathEscape(reference)+"/json", nil, &image); e != nil {
		return fmt.Errorf("resolve existing local image (no pull): %w", e)
	}
	if image.ID == "" {
		return errors.New("Docker image has no immutable ID")
	}
	if len(image.Config.Volumes) > 0 {
		return errors.New("image-declared volumes are unsupported")
	}
	r.image = image.ID
	body, e := r.provision(deadline)
	if e != nil {
		return e
	}
	r.id, e = r.createContainer(deadline, "dynamo-operator-"+strings.ToLower(session.ID), body)
	if e != nil {
		return e
	}
	if e = r.startCapture(deadline); e != nil {
		return e
	}
	r.Progress.Emit(contracts.Progress{Kind: contracts.ContainerStarting})
	if e = r.Engine.Call(deadline, "POST", "/containers/"+r.id+"/start", nil, nil); e != nil {
		return e
	}
	r.launched = time.Now()
	if err := r.emit("started"); err != nil {
		return err
	}
	r.Progress.Emit(contracts.Progress{Kind: contracts.ConnectionWaiting})
	life, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		var exit struct{ StatusCode int }
		err := r.Engine.Call(life, "POST", "/containers/"+r.id+"/wait?condition=not-running", nil, &exit)
		if life.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("operator %s exited unexpectedly (code %d)", r.id, exit.StatusCode)
		}
		select {
		case r.failures <- err:
		default:
		}
	}()
	if r.capture != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			select {
			case <-r.capture.done:
			case <-life.Done():
				return
			}
			if r.stopping.Load() {
				return
			}
			check, stop := context.WithTimeout(life, time.Second)
			defer stop()
			// EOF can precede the ordinary wait notification; confirm Docker state.
			var captureErr error
			for {
				var state struct{ State struct{ Running bool } }
				e := r.Engine.Call(check, "GET", "/containers/"+r.id+"/json", nil, &state)
				if e == nil && !state.State.Running {
					return
				}
				if check.Err() != nil {
					captureErr = errors.New("operator capture ended without confirmed exit")
					break
				}
				select {
				case <-check.Done():
				case <-time.After(20 * time.Millisecond):
				}
			}
			r.capture.mu.Lock()
			if r.capture.err != nil {
				captureErr = r.capture.err
			}
			r.capture.mu.Unlock()
			select {
			case r.failures <- captureErr:
			default:
			}
		}()
	}
	select {
	case <-deadline.Done():
		return errors.New("operator connection startup cancelled or timed out")
	case err := <-r.failures:
		return err
	case <-r.Connected:
		// Confirm the container is still running when publishing connection readiness.
		var state struct{ State struct{ Running bool } }
		if err := r.Engine.Call(deadline, "GET", "/containers/"+r.id+"/json", nil, &state); err != nil {
			return err
		}
		if !state.State.Running {
			return errors.New("operator exited before connection readiness")
		}
		return r.emit("connected")
	}
}
func (r *Runtime) Stop(ctx context.Context) error {
	r.stopping.Store(true)
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	var errs []error
	if r.id != "" {
		if own, e := r.owns(ctx, "containers", r.id); e != nil {
			errs = append(errs, e)
		} else if own {
			_ = r.Engine.Call(ctx, "POST", "/containers/"+r.id+"/stop?t=1", nil, nil)
		}
	}
	for i := len(r.containers) - 1; i >= 0; i-- {
		id := r.containers[i]
		if e := r.removeContainer(ctx, id); e != nil {
			errs = append(errs, fmt.Errorf("cleanup incomplete for owned container %s: %w", id, e))
		}
	}
	if r.capture != nil {
		// Reserve a small part of the same cleanup deadline to cancel and join
		// an incomplete capture; never allocate another shutdown budget.
		drain := ctx
		finishDrain := func() {}
		if deadline, ok := ctx.Deadline(); ok {
			drain, finishDrain = context.WithDeadline(ctx, deadline.Add(-100*time.Millisecond))
		}
		select {
		case <-r.capture.done:
		case <-drain.Done():
			r.capture.cancel()
			r.capture.source.Close()
			errs = append(errs, errors.New("capture drain deadline exceeded"))
			select {
			case <-r.capture.done:
			case <-ctx.Done():
				errs = append(errs, errors.New("capture worker cleanup incomplete"))
			}
		}
		finishDrain()
		select {
		case <-r.capture.done:
			r.capture.mu.Lock()
			errs = append(errs, r.capture.err)
			truncated := r.capture.truncated
			r.capture.mu.Unlock()
			state := "capture.finished"
			if r.capture.err != nil {
				state = "capture.failed"
			}
			if truncated && r.capture.err == nil {
				state = "capture.truncated"
			}
			errs = append(errs, r.emit(state))
		default:
		}
		r.capture.cancel()
	}
	for i := len(r.volumes) - 1; i >= 0; i-- {
		id := r.volumes[i]
		own, e := r.owns(ctx, "volumes", id)
		if e == nil && own {
			e = r.Engine.Call(ctx, "DELETE", "/volumes/"+url.PathEscape(id), nil, nil)
		}
		if e != nil && !IsNotFound(e) {
			errs = append(errs, fmt.Errorf("cleanup incomplete for owned volume %s: %w", id, e))
		}
	}
	if r.buildInfo != nil && r.Inputs != nil {
		data, _ := json.MarshalIndent(r.buildInfo, "", "  ")
		errs = append(errs, r.Inputs.WriteOutput(ctx, "operator-build.json", data))
	}
	if r.id != "" && errors.Join(errs...) == nil {
		errs = append(errs, r.emit("removed"))
	}
	return errors.Join(errs...)
}
