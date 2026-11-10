package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/httpserver"
	"operator-dynamo/internal/kubeapi"
	"operator-dynamo/internal/operatorruntime"
	"operator-dynamo/internal/scenario"
	"operator-dynamo/internal/workspace"
	"os"
	"sort"
	"sync"
	"time"
)

type testWorkspace interface {
	Workspace
	scenario.Source
	operatorruntime.Inputs
	RelativeSelection(string) (string, error)
	ReleaseSession() error
	NewRun(string) error
	WriteRun(context.Context, any) error
	TakeRunOutput() (workspace.RunWriter, error)
}
type frozenReader struct {
	ctx      context.Context
	source   scenario.Source
	manifest map[string]string
	files    map[string][]byte
}

func (f *frozenReader) ReadInput(name string) ([]byte, error) {
	if b, ok := f.files[name]; ok {
		return append([]byte(nil), b...), nil
	}
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	if f.manifest == nil {
		f.manifest = map[string]string{}
	}
	v, e := f.source.ReadSource(f.ctx, name, 4<<20)
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256(v.Data)
	f.manifest[name] = hex.EncodeToString(h[:])
	f.files[name] = append([]byte(nil), v.Data...)
	return v.Data, nil
}

type testRun struct {
	diagnose func(contracts.APIDiagnostic)
	ui       *workbenchState
	scope    contracts.Progress
	progress contracts.ProgressObserver
	app      *Application
	work     testWorkspace
	cfg      connection
	regs     []kubeapi.Registration
	timeout  time.Duration
	prepared *operatorruntime.Prepared
}
type testSession struct {
	diagnose func(contracts.APIDiagnostic)
	ui       *workbenchState
	id       string
	work     testWorkspace
	journal  Journal
	recorder eventRecorder
	meta     contracts.Metadata
	api      *kubeapi.Server
	runtime  *operatorruntime.Runtime
	gateway  *scenario.HTTPGateway
	started  []Component
	ctx      context.Context
	cancel   context.CancelFunc
	monitors sync.WaitGroup
	mu       sync.Mutex
	failure  string
}

func (s *testSession) ID() string                { return s.id }
func (s *testSession) Gateway() scenario.Gateway { return s.gateway }
func (s *testSession) Failure() string           { s.mu.Lock(); defer s.mu.Unlock(); return s.failure }
func (s *testSession) fail(reason string) {
	s.mu.Lock()
	if s.failure == "" {
		s.failure = reason
	}
	s.mu.Unlock()
	s.cancel()
}
func (s *testSession) Record(kind string, p contracts.LifecyclePayload) error {
	if s.recorder == nil {
		return errors.New("journal unavailable")
	}
	e := s.recorder.Record(contracts.Observation{Kind: kind, CorrelationID: p.RunID + ":" + p.ScenarioID + ":" + p.StepID, Payload: p})
	if e != nil {
		s.fail("journal_failed")
	}
	return e
}
func (f *testRun) Start(parent context.Context, runID string) (scenario.Session, error) {
	s := &testSession{id: rand.Text(), work: f.work, ui: f.ui, diagnose: f.diagnose}
	if s.ui != nil {
		s.ui.register(s.id, f.scope.Name, f.scope.Index, true)
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	if e := f.work.NewSession(s.id); e != nil {
		if s.ui != nil {
			s.ui.catalog.closing(s.id)
			s.ui.catalog.ended(s.id, true)
			s.ui.events.Complete(s.id)
		}
		s.cancel()
		return nil, e
	}
	s.meta = contracts.Metadata{SchemaVersion: 1, ID: s.id, State: "starting", StartedAt: time.Now().UTC()}
	if e := f.work.WriteMetadata(s.meta); e != nil {
		return s, e
	}
	j, e := f.app.OpenJournal(f.work, s.id)
	if e != nil {
		return s, e
	}
	s.journal = j
	if s.ui != nil {
		s.ui.project(j)
	}
	s.recorder, _ = j.(eventRecorder)
	if s.recorder == nil {
		return s, errors.New("journal unavailable")
	}
	if e = j.Append("starting"); e != nil {
		return s, e
	}
	s.api = f.cfg.newAPI(f.regs, s.recorder.Record)
	s.runtime = operatorruntime.New(f.cfg.Operator, f.cfg.API.AdvertiseURL, f.timeout, s.api.Connected, s.recorder.Record)
	s.runtime.Inputs = f.work
	s.runtime.Prepared = f.prepared
	s.runtime.Progress = f.progress
	for _, c := range []Component{s.api, s.runtime} {
		s.started = append(s.started, c)
		if e = c.Start(s.ctx, contracts.Session{ID: s.id}); e != nil {
			return s, e
		}
		if s.ui != nil && c == s.api {
			s.ui.catalog.available(s.id, s.api)
		}
		if source, ok := c.(FailureSource); ok {
			reason := "runtime_failed"
			if c == s.api {
				reason = "api_failed"
			}
			s.monitors.Add(1)
			go func(source FailureSource, reason string) {
				defer s.monitors.Done()
				select {
				case e := <-source.Failures():
					if e != nil {
						s.fail(reason)
					}
				case <-s.ctx.Done():
				}
			}(source, reason)
		}
	}
	s.gateway = scenario.NewHTTPGateway(s.api.Address())
	s.meta.State = "ready"
	if e = errors.Join(j.Append("ready"), f.work.WriteMetadata(s.meta)); e != nil {
		s.fail("journal_failed")
		return s, e
	}
	if s.ui != nil {
		s.ui.catalog.ready(s.id)
	}
	return s, nil
}
func (s *testSession) Stop(ctx context.Context) error {
	if s.ui != nil {
		s.ui.catalog.closing(s.id)
	}
	s.cancel()
	if s.gateway != nil {
		s.gateway.Close()
	}
	var errs []error
	if s.journal != nil {
		errs = append(errs, s.journal.Append("stopping"))
	}
	for i := len(s.started) - 1; i >= 0; i-- {
		errs = append(errs, s.started[i].Stop(ctx))
	}
	s.monitors.Wait()
	summaries, diagnosticErr := collectDiagnostics(s.started, s.journal, s.id)
	errs = append(errs, diagnosticErr)
	// Failure notifications can race cancellation; drain after producers have joined.
	for _, c := range s.started {
		if f, ok := c.(FailureSource); ok {
			select {
			case e := <-f.Failures():
				if e != nil {
					s.fail("runtime_failed")
				}
			default:
			}
		}
	}
	if s.journal != nil {
		if e := s.journal.Close(); e != nil {
			for i := range summaries {
				summaries[i].Capture = "failed"
			}
			s.fail("journal_failed")
			errs = append(errs, e)
		}
	}
	errs = append(errs, ctx.Err())
	state := "stopped"
	if errors.Join(errs...) != nil || s.Failure() != "" {
		state = "failed"
	}
	s.meta.State = state
	now := time.Now().UTC()
	s.meta.EndedAt = &now
	errs = append(errs, s.work.WriteMetadata(s.meta))
	errs = append(errs, s.work.ReleaseSession())
	result := errors.Join(errs...)
	deliverDiagnostics(summaries, result, s.diagnose)
	if s.ui != nil {
		s.ui.catalog.ended(s.id, result != nil || s.Failure() != "")
		s.ui.events.Complete(s.id)
	}
	return result
}
func (a *Application) runTests(parent context.Context, options contracts.Options) error {
	result := &contracts.TestResult{Code: 3, Status: "error", Reason: "input_failed"}
	var progress contracts.ProgressObserver
	if options.Console != nil {
		progress = options.Console.Observe
	}
	progress.Emit(contracts.Progress{Kind: contracts.ValidatingTests})
	raw, e := a.OpenWorkspace(options.Workdir)
	if e != nil {
		return result
	}
	work, ok := raw.(testWorkspace)
	if !ok {
		raw.Close()
		return result
	}
	// Report finalization explicitly handles Close so a release/flush error cannot pass.
	closed := false
	defer func() {
		if !closed {
			_ = raw.Close()
		}
	}()
	ctx, preflightDone := context.WithTimeout(parent, 30*time.Second)
	preflightError := func(err error) error {
		preflightDone()
		switch {
		case parent.Err() != nil:
			result.Code = 1
			result.Status = "cancelled"
			result.Reason = "cancelled"
		case errors.Is(err, contracts.ErrInvalid) || errors.Is(err, os.ErrNotExist):
			result.Code = 2
			result.Reason = "invalid_input"
		default:
			result.Reason = "input_failed"
		}
		if raw.Close() != nil {
			result.Code = 3
			result.Status = "error"
			result.Reason = "cleanup_failed"
		}
		closed = true
		return result
	}
	reader := &frozenReader{ctx: ctx, source: work, manifest: map[string]string{}}
	config, e := reader.ReadInput("config/operator.yaml")
	if e != nil {
		return preflightError(e)
	}
	cfg, regs, timeout, e := readConnection(config, reader)
	if e != nil {
		return preflightError(e)
	}
	selection, e := work.RelativeSelection(options.Tests)
	if e != nil {
		return preflightError(e)
	}
	resources := []scenario.Resource{}
	for _, r := range regs {
		resources = append(resources, scenario.Resource{APIVersion: r.APIVersion(), Kind: r.Kind, ListKind: r.ListKind, Plural: r.Plural, Scope: r.Scope})
	}
	plan, e := scenario.BuildPlan(ctx, work, selection, resources)
	if e != nil {
		return preflightError(e)
	}
	progress.Emit(contracts.Progress{Kind: contracts.Selected, Total: len(plan.Scenarios)})
	prepared, e := operatorruntime.Prepare(ctx, cfg.Operator, work)
	if e != nil {
		return preflightError(e)
	}
	manifest := reader.manifest
	for _, v := range append(plan.Inputs, prepared.Manifest()...) {
		if old, ok := manifest[v.Path]; ok && old != v.SHA256 {
			return preflightError(contracts.ErrInvalid)
		}
		manifest[v.Path] = v.SHA256
	}
	plan.Inputs = nil
	for name, hash := range manifest {
		plan.Inputs = append(plan.Inputs, contracts.InputDigest{Path: name, SHA256: hash})
	}
	sort.Slice(plan.Inputs, func(i, j int) bool { return plan.Inputs[i].Path < plan.Inputs[j].Path })
	if e = scenario.ValidateReportBound(plan); e != nil {
		return preflightError(e)
	}
	if e = ctx.Err(); e != nil {
		return preflightError(e)
	}
	preflightDone()
	runID := rand.Text()
	if e = work.NewRun(runID); e != nil {
		return preflightError(e)
	}
	result.ReportPath = "runs/" + runID + "/report.json"
	report := scenario.NewReport(plan, runID)
	started := time.Now()
	if e = work.WriteRun(parent, report); e != nil {
		return preflightError(e)
	}
	appCtx, appCancel := context.WithCancel(parent)
	defer appCancel()
	var health UserHTTP = httpserver.New(options.Listen)
	if a.UserHTTP != nil {
		health = a.UserHTTP(options.Listen)
	}
	var ui *workbenchState
	if options.UI {
		ui, e = newWorkbench(work)
		if e != nil {
			return preflightError(e)
		}
		ui.catalog.runID = runID
		ui.configure(health, options)
	}
	health.SetReady(true) // Not externally visible until Start; terminal cancellation only withdraws it.
	var mu sync.Mutex
	appFailure := ""
	var monitor sync.WaitGroup
	failure := func() string { mu.Lock(); defer mu.Unlock(); return appFailure }
	var stopMu sync.Mutex
	stopCtx := context.Context(nil)
	stopCancel := func() {}
	var stopOnce sync.Once
	beginStop := func() context.Context {
		stopOnce.Do(func() {
			health.SetReady(false)
			c, d := context.WithTimeout(context.Background(), 5*time.Second)
			stopMu.Lock()
			stopCtx = c
			stopCancel = d
			stopMu.Unlock()
			appCancel()
		})
		stopMu.Lock()
		defer stopMu.Unlock()
		return stopCtx
	}
	currentStop := func() context.Context { stopMu.Lock(); defer stopMu.Unlock(); return stopCtx }
	var cancellationMonitor sync.WaitGroup
	cancellationMonitor.Add(1)
	go func() { defer cancellationMonitor.Done(); <-appCtx.Done(); beginStop() }()
	control := scenario.Control{
		Failure: failure, Cancelled: func() bool { return parent.Err() != nil },
		Cleanup: func(terminal bool) (context.Context, context.CancelFunc) {
			if terminal || appCtx.Err() != nil {
				return beginStop(), func() {}
			}
			return context.WithTimeout(context.Background(), 5*time.Second)
		},
		Publish: func(r *scenario.Report) error {
			r.Recount()
			c := currentStop()
			if c == nil {
				if appCtx.Err() != nil {
					c = beginStop()
				} else {
					c = appCtx
				}
			}
			return work.WriteRun(c, r)
		},
	}
	out := scenario.Outcome{Status: "passed"}
	if e = health.Start(appCtx, contracts.Session{}); e != nil {
		out = scenario.Outcome{Status: "error", Reason: "http_failed", Code: 3}
		if parent.Err() != nil {
			out = scenario.Outcome{Status: "cancelled", Reason: "cancelled", Code: 1}
		}
	} else {
		monitor.Add(1)
		go func() {
			defer monitor.Done()
			select {
			case e := <-health.Failures():
				if e != nil {
					mu.Lock()
					appFailure = "http_failed"
					mu.Unlock()
					health.SetReady(false)
					appCancel()
				}
			case <-appCtx.Done():
			}
		}()
		if cfg.Operator.Build == nil {
			pinCtx, pinDone := context.WithTimeout(appCtx, 5*time.Second)
			cfg.Operator.Image, e = operatorruntime.PinImage(pinCtx, operatorruntime.LocalDocker(), cfg.Operator.Image)
			pinDone()
			if e != nil {
				out = scenario.Outcome{Status: "error", Reason: "startup_failed", Code: 3}
				if parent.Err() != nil {
					out = scenario.Outcome{Status: "cancelled", Reason: "cancelled", Code: 1}
				}
			}
		}
		if out.Code == 0 {
			factory := &testRun{diagnose: options.Diagnose, ui: ui, app: a, work: work, cfg: cfg, regs: regs, timeout: timeout, prepared: prepared}
			var scope contracts.Progress
			scoped := contracts.ProgressObserver(func(p contracts.Progress) {
				p.Index, p.Total, p.Name = scope.Index, scope.Total, scope.Name
				progress.Emit(p)
			})
			control.Progress = scoped
			control.Select = func(index, total int, name string) {
				scope = contracts.Progress{Index: index, Total: total, Name: name}
				factory.progress = scoped
				factory.scope = scope
			}
			out = scenario.Run(appCtx, plan, report, factory, control)
		}
	}
	cleanup := beginStop()
	if e = health.Stop(cleanup); e != nil {
		out = scenario.Outcome{Status: "error", Reason: "cleanup_failed", Code: 3}
	}
	monitor.Wait()
	select {
	case err := <-health.Failures():
		if err != nil {
			mu.Lock()
			appFailure = "http_failed"
			mu.Unlock()
		}
	default:
	}
	if failure() != "" {
		out = scenario.Outcome{Status: "error", Reason: failure(), Code: 3}
	}
	for i := range report.Scenarios {
		v := &report.Scenarios[i]
		if v.Status == "pending" {
			v.Status = "skipped"
			v.ReasonCode = "run_aborted"
			for j := range v.Steps {
				v.Steps[j].Outcome = "skipped"
				v.Steps[j].ReasonCode = "run_aborted"
			}
		}
	}
	if parent.Err() != nil && out.Code < 3 {
		out = scenario.Outcome{Status: "cancelled", Reason: "cancelled", Code: 1}
	}
	if cleanup.Err() != nil {
		out = scenario.Outcome{Status: "error", Reason: "cleanup_failed", Code: 3}
	}
	out = finalizeTestRun(cleanup, work, report, out, started)
	closed = true
	cancellationMonitor.Wait()
	defer stopCancel()
	result.Code = out.Code
	result.Status = out.Status
	result.Reason = out.Reason
	for _, v := range report.Scenarios {
		summary := contracts.TestSummary{Name: v.Name, Status: v.Status, Reason: v.ReasonCode}
		for _, s := range v.Steps {
			if s.Outcome == "failed" || s.Outcome == "error" {
				summary.Step = s.ID
				summary.Reason = s.ReasonCode
				break
			}
		}
		result.Scenarios = append(result.Scenarios, summary)
	}
	result.Counts = report.Counts
	if parent.Err() != nil {
		progress.Emit(contracts.Progress{Kind: contracts.Cancelled})
	}
	if options.Console != nil {
		options.Console.Complete(cleanup, result)
	}
	return result
}

var _ testWorkspace = (*workspace.Workspace)(nil)

func finalizeTestRun(cleanup context.Context, work testWorkspace, report *scenario.Report, out scenario.Outcome, started time.Time) scenario.Outcome {
	output, outputErr := work.TakeRunOutput()
	if outputErr != nil {
		out = scenario.Outcome{Status: "error", Reason: "report_failed", Code: 3}
	}
	// No terminal pass is written until input/session handle closure succeeds.
	closeErr := work.Close()
	if closeErr != nil {
		out = scenario.Outcome{Status: "error", Reason: "cleanup_failed", Code: 3}
	}
	report.Finish(out.Status, out.Reason, out.Code, started)
	if output != nil {
		if e := output.Write(cleanup, report); e != nil {
			out = scenario.Outcome{Status: "error", Reason: "report_failed", Code: 3}
		}
		if closeErr != nil {
			_ = work.Close()
		}
		if e := output.Close(); e != nil {
			out = scenario.Outcome{Status: "error", Reason: "report_failed", Code: 3}
		}
	}

	return out
}
