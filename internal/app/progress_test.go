package app

import (
	"context"
	"errors"
	"operator-dynamo/internal/contracts"
	"testing"
	"time"
)

func TestFinalConsoleAfterWorkspaceCloseSameBudget(t *testing.T) {
	for _, closeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "close-failure"}[closeFailure], func(t *testing.T) {
			work := &memoryWorkspace{}
			if closeFailure {
				work.closeErr = errors.New("private close detail")
			}
			journal := &memoryJournal{}
			calls := []string{}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var originalDeadline time.Time
			c := &stub{name: "http", calls: &calls, start: func(context.Context) error { cancel(); return nil }, stop: func(ctx context.Context) error { originalDeadline, _ = ctx.Deadline(); return nil }}
			app := &Application{OpenWorkspace: func(string) (Workspace, error) { return work, nil }, OpenJournal: func(Workspace, string) (Journal, error) { return journal, nil }, Components: func(contracts.Options) []Component { return []Component{c} }, CleanupTimeout: 100 * time.Millisecond}
			count := 0
			var kinds []contracts.ProgressKind
			o := contracts.Options{Console: &contracts.Console{Observe: func(p contracts.Progress) { kinds = append(kinds, p.Kind) }, Finish: func(ctx context.Context, e error) {
				count++
				if !work.closed || !journal.closed {
					t.Error("rendered before closure")
				}
				d, _ := ctx.Deadline()
				if !d.Equal(originalDeadline) {
					t.Error("deadline replaced")
				}
				if (e != nil) != closeFailure {
					t.Errorf("close result %v", e)
				}
				// Delivery-only expiry does not change the already finalized domain result.
				<-ctx.Done()
			}}}
			result := app.Run(parent, o)
			if count != 1 || (result != nil) != closeFailure {
				t.Fatal(count, result, kinds)
			}
			for _, k := range kinds {
				if closeFailure && k == contracts.CleanupComplete {
					t.Fatal("false cleanup complete")
				}
			}
		})
	}
}

func TestObserverOnlyDoesNotRequireFinalRenderer(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Application{}
	if e := a.Run(parent, contracts.Options{Console: &contracts.Console{Observe: func(contracts.Progress) {}}}); e != nil {
		t.Fatal(e)
	}
}
