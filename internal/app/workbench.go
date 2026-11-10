package app

import (
	"context"
	"errors"
	"net/http"
	"operator-dynamo/internal/contracts"
	"operator-dynamo/internal/journal"
	"operator-dynamo/internal/workbench"
)

type workbenchState struct {
	catalog *sessionCatalog
	events  *journal.Windows
	reports workbench.Reports
}

func newWorkbench(work Workspace) (*workbenchState, error) {
	reports, ok := work.(workbench.Reports)
	if !ok {
		return nil, errors.New("report inspection unavailable")
	}
	return &workbenchState{catalog: newCatalog(), events: journal.NewWindows(), reports: reports}, nil
}
func (u *workbenchState) register(id, name string, index int, operator bool) {
	u.catalog.register(id, name, index, operator)
	u.events.Register(id)
}
func (u *workbenchState) project(j Journal) {
	if v, ok := j.(interface{ SetProjection(*journal.Windows) }); ok {
		v.SetProjection(u.events)
	}
}
func (u *workbenchState) configure(component Component, options contracts.Options) {
	u.catalog.mu.Lock()
	u.catalog.writeEnabled = options.UIWrite && options.Tests == ""
	u.catalog.mu.Unlock()
	if server, ok := component.(interface {
		Workbench(func(context.Context, string) (http.Handler, error))
	}); ok {
		server.Workbench(func(ctx context.Context, address string) (http.Handler, error) {
			h, origin, e := workbench.New(ctx, address, options.UIOrigin, u.catalog, u.events, u.reports)
			if e == nil && options.UIWrite && options.Tests == "" {
				e = h.EnableWrites(options.UIOrigin)
			}
			if e == nil && options.Console != nil {
				options.Console.Observe.Emit(contracts.Progress{Kind: contracts.UIAvailable, Name: origin, Step: options.Listen})
			}
			return h, e
		})
	}
}
