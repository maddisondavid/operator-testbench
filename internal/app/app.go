// Package app owns session orchestration. Resource, runtime and scenario logic
// intentionally do not exist in the architectural spine.
package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/uiaddress"
	"operator-dynamo/internal/workbench"
)

var ErrUnsupported = errors.New("unsupported capability")

type Workspace interface {
	NewSession(string) error
	WriteMetadata(contracts.Metadata) error
	CreateJournal() (*os.File, error)
	Close() error
}
type Journal interface {
	Append(string) error
	Close() error
}
type Component interface {
	Start(context.Context, contracts.Session) error
	Stop(context.Context) error
}
type Readiness interface{ SetReady(bool) }
type FailureSource interface{ Failures() <-chan error }

type UserHTTP interface {
	Component
	Readiness
	FailureSource
}

type Application struct {
	UserHTTP       func(string) UserHTTP
	OpenWorkspace  func(string) (Workspace, error)
	OpenJournal    func(Workspace, string) (Journal, error)
	Components     func(contracts.Options) []Component
	Prepare        func(contracts.Options, Workspace, Journal) ([]Component, error)
	CleanupTimeout time.Duration
}

func (a *Application) Run(parent context.Context, options contracts.Options) (result error) {
	if options.UIWrite {
		if !options.UI || options.Tests != "" {
			return contracts.ErrInvalid
		}
		if _, err := uiaddress.Origin(options.UIOrigin); err != nil {
			return contracts.ErrInvalid
		}
	}
	if options.Tests != "" {
		return a.runTests(parent, options)
	}
	var finalContext context.Context
	var cleanupErr error
	var apiDiagnostics []contracts.APIDiagnostic
	reachedReady := false
	preparationReported := false
	finalCancel := func() {}
	emit := func(kind contracts.ProgressKind) {
		if options.Console != nil {
			options.Console.Observe.Emit(contracts.Progress{Kind: kind})
		}
	}
	emit(contracts.Validating)
	// Registered first: this barrier runs after all component and Workspace defers.
	defer func() {
		defer finalCancel()
		if finalContext != nil && finalContext.Err() != nil {
			cleanupErr = errors.Join(cleanupErr, finalContext.Err())
			result = errors.Join(result, finalContext.Err())
		}
		deliverDiagnostics(apiDiagnostics, cleanupErr, options.Diagnose)
		if options.Console == nil {
			return
		}
		if parent.Err() != nil {
			emit(contracts.Cancelled)
		}
		if finalContext == nil {
			finalContext, finalCancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer finalCancel()

		} else if cleanupErr != nil {
			emit(contracts.CleanupFailed)
		} else {
			emit(contracts.CleanupComplete)
		}
		if !reachedReady && !preparationReported && result != nil && finalContext != nil {
			emit(contracts.PreparationFailed)
		}
		options.Console.Complete(finalContext, result)
	}()
	if parent.Err() != nil {
		return nil
	}
	work, err := a.OpenWorkspace(options.Workdir)
	if err != nil {
		return fmt.Errorf("open workspace: %w", err)
	}
	var ui *workbenchState
	if options.UI {
		ui, err = newWorkbench(work)
		if err != nil {
			work.Close()
			return err
		}
	}
	var meta contracts.Metadata
	defer func() {
		if closeErr := work.Close(); closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, closeErr)
			result = errors.Join(result, closeErr)
			if meta.ID != "" {
				meta.State = "failed"
				now := time.Now().UTC()
				meta.EndedAt = &now
				// Workspace retains its artifact handle on early close failure so the
				// failed outcome can still be recorded. Any recording failure is surfaced.
				result = errors.Join(result, work.WriteMetadata(meta))
			}
			result = errors.Join(result, work.Close())
		}
	}()
	session := contracts.Session{ID: rand.Text()}
	if err = work.NewSession(session.ID); err != nil {
		return fmt.Errorf("allocate session: %w", err)
	}
	if ui != nil {
		ui.register(session.ID, "", 0, false)
	}
	meta = contracts.Metadata{SchemaVersion: 1, ID: session.ID, State: "created", StartedAt: time.Now().UTC()}
	if err = work.WriteMetadata(meta); err != nil {
		return fmt.Errorf("write initial metadata: %w", err)
	}
	journal, err := a.OpenJournal(work, session.ID)
	if err != nil {
		meta.State = "failed"
		now := time.Now().UTC()
		meta.EndedAt = &now
		return errors.Join(err, work.WriteMetadata(meta))
	}
	if ui != nil {
		ui.project(journal)
	}
	ctx, cancel := context.WithCancel(parent)
	var started []Component
	var monitors sync.WaitGroup
	failures := make(chan error, 1)
	transition := func(state string) error {
		meta.State = state
		return errors.Join(journal.Append(state), work.WriteMetadata(meta))
	}
	defer func() {
		if ui != nil {
			ui.catalog.closing(session.ID)
		}
		for _, c := range started {
			if ready, ok := c.(Readiness); ok {
				ready.SetReady(false)
			}
		}
		cancel()
		timeout := a.CleanupTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		finalContext, finalCancel = context.WithTimeout(context.Background(), timeout)
		cleanup := finalContext
		emit(contracts.CleanupStarting)
		cleanupErr = errors.Join(cleanupErr, transition("stopping"))
		for i := len(started) - 1; i >= 0; i-- {
			cleanupErr = errors.Join(cleanupErr, started[i].Stop(cleanup))
		}
		monitors.Wait()
		summaries, diagnosticErr := collectDiagnostics(started, journal, session.ID)
		apiDiagnostics = summaries
		cleanupErr = errors.Join(cleanupErr, diagnosticErr)
		select {
		case e := <-failures:
			result = errors.Join(result, e)
		default:
		}
		if cleanup.Err() != nil {
			cleanupErr = errors.Join(cleanupErr, cleanup.Err())
		}
		result = errors.Join(result, cleanupErr)
		state := "stopped"
		if result != nil {
			state = "failed"
		}
		now := time.Now().UTC()
		meta.EndedAt = &now
		terminalErr := transition(state)
		journalErr := journal.Close()
		if terminalErr != nil || journalErr != nil {
			for i := range apiDiagnostics {
				apiDiagnostics[i].Capture = "failed"
			}
		}
		cleanupErr = errors.Join(cleanupErr, terminalErr, journalErr)
		result = errors.Join(result, cleanupErr)
		// A flush failure can occur after a terminal event was written. Final metadata
		// is authoritative for completion and must reflect that failure.
		if result != nil {
			meta.State = "failed"
			cleanupErr = errors.Join(cleanupErr, work.WriteMetadata(meta))
			result = errors.Join(result, cleanupErr)
		}
		if ui != nil {
			ui.catalog.ended(session.ID, result != nil)
			ui.events.Complete(session.ID)
		}
	}()
	if err = transition("starting"); err != nil {
		return err
	}
	var components []Component
	if a.Prepare != nil {
		components, err = a.Prepare(options, work, journal)
		if err != nil {
			emit(contracts.PreparationFailed)
			preparationReported = true
			return err
		}
	} else {
		components = a.Components(options)
	}
	if ui != nil {
		for _, c := range components {
			if _, ok := c.(workbench.Reader); ok {
				ui.catalog.mu.Lock()
				ui.catalog.entries[session.ID].descriptor.OperatorState = "starting"
				ui.catalog.mu.Unlock()
			}
		}
	}
	for _, component := range components {
		if ui != nil {
			ui.configure(component, options)
		}
		if ctx.Err() != nil {
			return nil
		}
		// Stop is required even for a partially acquired Start failure.
		started = append(started, component)
		if err = component.Start(ctx, session); err != nil {
			if parent.Err() != nil && errors.Is(err, context.Canceled) {
				return nil
			}
			emit(contracts.PreparationFailed)
			preparationReported = true
			return fmt.Errorf("component start: %w", err)
		}
		if ui != nil {
			if reader, ok := component.(workbench.Reader); ok {
				ui.catalog.available(session.ID, reader)
			}
		}
		if source, ok := component.(FailureSource); ok {
			monitors.Add(1)
			go func() {
				defer monitors.Done()
				select {
				case e, open := <-source.Failures():
					if open && e != nil {
						select {
						case failures <- e:
						default:
						}
						cancel() // Interrupt dependent startup as well as steady-state work.
					}
				case <-ctx.Done():
				}
			}()
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err = transition("ready"); err != nil {
		return err
	}
	for _, c := range started {
		if ready, ok := c.(Readiness); ok {
			ready.SetReady(true)
		}
	}
	if ui != nil {
		ui.catalog.ready(session.ID)
	}
	reachedReady = true
	emit(contracts.HTTPReady)
	select {
	case <-ctx.Done():
		return nil
	case err = <-failures:
		return fmt.Errorf("component failed: %w", err)
	}
}
