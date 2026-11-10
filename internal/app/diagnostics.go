package app

import (
	"errors"
	"operator-dynamo/internal/contracts"
)

type diagnosticSource interface {
	Diagnostics() contracts.APIDiagnostic
}

// Collect after API workers stop and before the journal closes. Capture failures
// remain domain failures; console delivery remains best effort under its barrier.
func collectDiagnostics(components []Component, journal Journal, id string) ([]contracts.APIDiagnostic, error) {
	var summaries []contracts.APIDiagnostic
	var errs []error
	for _, c := range components {
		source, ok := c.(diagnosticSource)
		if !ok {
			continue
		}
		d := source.Diagnostics()
		if d.Records == 0 && d.Terminal == "" {
			continue
		}
		d.SessionID = id
		recorder, ok := journal.(eventRecorder)
		var err error
		if !ok {
			err = errors.New("API rejection summary capture failed")
		} else {
			err = recorder.Record(contracts.Observation{Kind: "api.rejections.summary", CorrelationID: id, Payload: contracts.LifecyclePayload{Notice: d.Notice, Records: d.Records, Truncated: d.Truncated}})
		}
		d.Capture = "recorded"
		if err != nil {
			d.Capture = "failed"
			errs = append(errs, err)
		}
		summaries = append(summaries, d)
	}
	return summaries, errors.Join(errs...)
}
func deliverDiagnostics(summaries []contracts.APIDiagnostic, cleanup error, emit func(contracts.APIDiagnostic)) {
	if emit == nil {
		return
	}
	for _, d := range summaries {
		d.Cleanup = "complete"
		if cleanup != nil {
			d.Cleanup = "failed"
		}
		emit(d)
	}
}
