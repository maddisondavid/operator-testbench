package contracts

import (
	"context"
	"time"
)

// Progress is optional diagnostic vocabulary, never authoritative state or errors.
type ProgressKind uint8

const (
	Validating ProgressKind = iota + 1
	Selected
	Preparing
	BuildStarting
	BuildComplete
	ContainerStarting
	ContainerStarted
	ConnectionWaiting
	OperatorConnected
	ScenarioRunning
	Creating
	Updating
	Deleting
	Asserting
	StepPassed
	StepFailed
	CleanupStarting
	CleanupComplete
	CleanupFailed
	PreparationFailed
	Cancelled
	HTTPReady
	ValidatingTests
	UIAvailable
)

type Progress struct {
	Kind         ProgressKind
	Index, Total int
	Name, Step   string
	Wait         time.Duration
}
type ProgressObserver func(Progress)

func (f ProgressObserver) Emit(p Progress) {
	if f != nil {
		f(p)
	}
}

// Console is an adapter-owned observer and final barrier, composed by the Spine.
// Finish cannot affect the domain outcome; it must honor the supplied deadline.
type Console struct {
	Observe ProgressObserver
	Finish  func(context.Context, error)
}

func (c *Console) Complete(ctx context.Context, result error) {
	if c != nil && c.Finish != nil {
		c.Finish(ctx, result)
	}
}
