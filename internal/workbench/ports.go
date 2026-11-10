// Package workbench is a bounded inspection and opt-in command HTTP adapter. Its ports expose no filesystem,
// Docker, Kubernetes HTTP client, or mutable resource store.
package workbench

import (
	"context"
	"operator-dynamo/internal/contracts"
)

type Reader interface {
	InspectionTypes(context.Context) ([]contracts.ResourceType, error)
	Inspect(context.Context, string, string, string, string) (contracts.ResourceSnapshot, error)
}
type Lease interface {
	Context() context.Context
	Reader() Reader
	Commit() error
	Release()
}
type Catalog interface {
	Catalog(context.Context) (contracts.Invocation, error)
	Acquire(context.Context, string) (Lease, error)
}
type Events interface {
	Page(context.Context, string, uint64, int) (contracts.EventPage, error)
}
type Reports interface {
	ListReports(context.Context, string, int) (contracts.RunPage, error)
	ReadReport(context.Context, string) ([]byte, map[string]any, error)
}

// CommandTarget is API-owned; CloseCommands shares its mutation barrier.
type CommandTarget interface {
	Command(context.Context, contracts.ResourceCommand) (contracts.CommandResult, error)
	CloseCommands()
}
type CommandCatalog interface {
	ExecuteCommand(context.Context, string, contracts.ResourceCommand) (contracts.CommandResult, error)
}
