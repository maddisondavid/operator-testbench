package app

import (
	"operator-dynamo/internal/journal"
	"operator-dynamo/internal/workspace"
	"time"
)

// New is the only production composition root. Peer components know no concrete
// application implementation and future adapters may use the same Run contract.
func New() *Application {
	return &Application{
		OpenWorkspace: func(path string) (Workspace, error) { return workspace.Open(path) },
		OpenJournal: func(w Workspace, id string) (Journal, error) {
			f, err := w.CreateJournal()
			if err != nil {
				return nil, err
			}
			return journal.New(f, id), nil
		},
		Prepare:        prepare,
		CleanupTimeout: 5 * time.Second,
	}
}
