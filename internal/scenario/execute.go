package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"operator-dynamo/internal/contracts"
	"time"
)

type Control struct {
	Progress  contracts.ProgressObserver
	Select    func(int, int, string)
	Cancelled func() bool
	Publish   func(*Report) error
	Failure   func() string
	Cleanup   func(bool) (context.Context, context.CancelFunc)
}
type Outcome struct {
	Status, Reason string
	Code           int
}

func failure(reason string) Outcome { return Outcome{Status: "error", Reason: reason, Code: 3} }
func failed(reason string) Outcome  { return Outcome{Status: "failed", Reason: reason, Code: 1} }
func cancelled() Outcome            { return Outcome{Status: "cancelled", Reason: "cancelled", Code: 1} }
func health(ctx context.Context, s Session, control Control) Outcome {
	if s != nil {
		if r := s.Failure(); r != "" {
			return failure(r)
		}
	}
	if control.Failure != nil {
		if r := control.Failure(); r != "" {
			return failure(r)
		}
	}
	if ctx.Err() != nil {
		return cancelled()
	}
	return Outcome{}
}
func skipSteps(s *ScenarioResult, reason string) {
	for i := range s.Steps {
		if s.Steps[i].Outcome == "pending" || s.Steps[i].Outcome == "running" {
			s.Steps[i].Outcome = "skipped"
			s.Steps[i].ReasonCode = reason
			s.Steps[i].DurationNS = 0
		}
	}
}
func Run(ctx context.Context, plan *Plan, report *Report, factory Factory, control Control) Outcome {
	result := Outcome{Status: "passed"}
	for i, definition := range plan.Scenarios {
		current := &report.Scenarios[i]
		if control.Select != nil {
			control.Select(i+1, len(plan.Scenarios), definition.Name)
		}
		h := health(ctx, nil, control)
		if h.Code != 0 {
			result = h
			break
		}
		now := time.Now().UTC()
		started := time.Now()
		current.StartedAt = &now
		current.Status = "running"
		if e := control.Publish(report); e != nil {
			result = failure("report_failed")
			current.Status = "error"
			current.ReasonCode = result.Reason
			skipSteps(current, "run_aborted")
			break
		}
		session, e := factory.Start(ctx, report.RunID)
		if session != nil {
			current.SessionID = session.ID()
			current.ArtifactPath = "sessions/" + session.ID()
		}
		out := Outcome{Status: "passed"}
		if e != nil {
			out = health(ctx, session, control)
			if out.Code == 0 {
				out = failure("startup_failed")
			}
		}
		if e != nil {
			control.Progress.Emit(contracts.Progress{Kind: contracts.PreparationFailed})
		}
		if e == nil {
			control.Progress.Emit(contracts.Progress{Kind: contracts.ScenarioRunning})
			if e = session.Record("scenario.started", contracts.LifecyclePayload{RunID: report.RunID, ScenarioID: definition.SourcePath}); e != nil {
				out = failure("journal_failed")
			} else {
				out = execute(ctx, session, definition, current, report.RunID, control)
			}
		}
		if h = health(ctx, session, control); h.Code == 3 || h.Code != 0 && out.Code == 0 {
			out = h
		}
		if session != nil {
			if e = session.Record("scenario.completed", contracts.LifecyclePayload{RunID: report.RunID, ScenarioID: definition.SourcePath, State: out.Status, ReasonCode: out.Reason}); e != nil {
				out = failure("journal_failed")
			}
			terminal := i == len(plan.Scenarios)-1 || out.Code == 3 || out.Status == "cancelled"
			stop, finish := control.Cleanup(terminal)
			stopStart := time.Now()
			control.Progress.Emit(contracts.Progress{Kind: contracts.CleanupStarting})
			err := session.Stop(stop)
			current.Cleanup.DurationNS = time.Since(stopStart).Nanoseconds()
			current.Cleanup.Outcome = "passed"
			if err != nil || stop.Err() != nil {
				current.Cleanup.Outcome = "error"
				current.Cleanup.ReasonCode = "cleanup_failed"
			}
			finish()
			if h = health(context.Background(), session, control); h.Code == 3 {
				if out.Code == 0 {
					out = h
				}
				result = failure(h.Reason)
			}
			if current.Cleanup.Outcome == "passed" {
				control.Progress.Emit(contracts.Progress{Kind: contracts.CleanupComplete})
			} else {
				control.Progress.Emit(contracts.Progress{Kind: contracts.CleanupFailed})
			}
			if current.Cleanup.Outcome == "error" {
				result = failure("cleanup_failed")
			}
		}
		if control.Cancelled != nil && control.Cancelled() && out.Code < 3 {
			out = cancelled()
		}
		current.Status = out.Status
		current.ReasonCode = out.Reason
		now = time.Now().UTC()
		current.EndedAt = &now
		current.DurationNS = time.Since(started).Nanoseconds()
		reason := "previous_step_failed"
		if out.Code == 3 || out.Status == "cancelled" {
			reason = "run_aborted"
		}
		skipSteps(current, reason)
		if out.Code > result.Code || out.Status == "cancelled" && result.Code < 3 {
			result = out
		}
		report.Recount()
		if e = control.Publish(report); e != nil {
			result = failure("report_failed")
		}
		if result.Code == 3 || out.Status == "cancelled" {
			break
		}
	}
	for i := range report.Scenarios {
		v := &report.Scenarios[i]
		if v.Status == "pending" {
			v.Status = "skipped"
			v.ReasonCode = "run_aborted"
			if result.Status == "cancelled" {
				v.ReasonCode = "cancelled"
			}
			skipSteps(v, v.ReasonCode)
		} else if v.Status == "running" {
			v.Status = "error"
			v.ReasonCode = result.Reason
			skipSteps(v, "run_aborted")
			now := time.Now().UTC()
			v.EndedAt = &now
		}
	}
	report.Recount()
	return result
}
func execute(ctx context.Context, s Session, definition Scenario, result *ScenarioResult, runID string, control Control) Outcome {
	scenarioCtx, cancel := context.WithTimeout(ctx, definition.Timeout)
	defer cancel()
	observations := map[string]map[string]any{}
	observationBytes := 0
	for i, step := range definition.Steps {
		record := &result.Steps[i]
		start := time.Now()
		record.Outcome = "running"
		h := health(ctx, s, control)
		if h.Code != 0 {
			return h
		}
		payload := contracts.LifecyclePayload{RunID: runID, ScenarioID: definition.SourcePath, StepID: step.ID, Operation: step.Operation}
		if e := s.Record("step.started", payload); e != nil {
			return failure("journal_failed")
		}
		allowance := 5 * time.Second
		if step.Assert != nil {
			allowance = step.Assert.Timeout
		}
		kind := map[string]contracts.ProgressKind{"create": contracts.Creating, "update": contracts.Updating, "delete": contracts.Deleting}[step.Operation]
		if step.Assert != nil {
			kind = contracts.Asserting
		}
		control.Progress.Emit(contracts.Progress{Kind: kind, Step: step.ID, Wait: allowance})
		stepCtx, done := context.WithTimeout(scenarioCtx, allowance)
		out := runStep(ctx, scenarioCtx, stepCtx, s, step, record, observations, &observationBytes, control)
		done()
		record.Outcome = out.Status
		record.ReasonCode = out.Reason
		record.DurationNS = time.Since(start).Nanoseconds()
		payload.State = out.Status
		payload.ReasonCode = out.Reason
		payload.RequestIDs = record.RequestIDs
		payload.SampleCount = record.SampleCount
		if e := s.Record("step.completed", payload); e != nil {
			return failure("journal_failed")
		}
		kind = contracts.StepPassed
		if out.Code != 0 {
			kind = contracts.StepFailed
		}
		control.Progress.Emit(contracts.Progress{Kind: kind, Step: step.ID})
		if out.Code != 0 {
			return out
		}
	}
	if h := health(ctx, s, control); h.Code != 0 {
		return h
	}
	if scenarioCtx.Err() != nil {
		return failed("scenario_timeout")
	}
	return Outcome{Status: "passed"}
}
func deadlineOutcome(parent, scenarioCtx, stepCtx context.Context, s Session, control Control, assert bool) Outcome {
	if h := health(parent, s, control); h.Code != 0 {
		return h
	}
	expired := func(c context.Context) bool {
		d, ok := c.Deadline()
		return c.Err() != nil || ok && !time.Now().Before(d)
	}
	if expired(scenarioCtx) {
		return failed("scenario_timeout")
	}
	if expired(stepCtx) {
		if assert {
			return failed("assertion_timeout")
		}
		return failed("action_timeout")
	}
	return Outcome{}
}
func runStep(parent, scenarioCtx, stepCtx context.Context, s Session, step Step, record *StepResult, observations map[string]map[string]any, retained *int, control Control) Outcome {
	request := func(method string, target Target, body any) (Response, Outcome) {
		if h := deadlineOutcome(parent, scenarioCtx, stepCtx, s, control, step.Assert != nil); h.Code != 0 {
			return Response{}, h
		}
		requestCtx, cancel := context.WithTimeout(stepCtx, 5*time.Second)
		defer cancel()
		res, e := s.Gateway().Request(requestCtx, method, target, body)
		if res.RequestID != "" {
			if step.Assert == nil || len(record.RequestIDs) < 2 {
				record.RequestIDs = append(record.RequestIDs, res.RequestID)
			} else {
				record.RequestIDs[1] = res.RequestID
			}
		}
		if h := deadlineOutcome(parent, scenarioCtx, stepCtx, s, control, step.Assert != nil); h.Code != 0 {
			return res, h
		}
		if requestCtx.Err() != nil {
			return res, failed("action_timeout")
		}
		if e != nil {
			if errors.Is(e, ErrCapacity) {
				return res, failure("capacity_exceeded")
			}
			return res, failure("api_failed")
		}
		if res.Code == 429 || res.Code >= 500 {
			return res, failure("api_failed")
		}
		return res, Outcome{}
	}
	rejected := func(res Response) Outcome {
		if res.Code < 200 || res.Code >= 300 {
			return failed("api_rejected")
		}
		return Outcome{Status: "passed"}
	}
	if step.Assert == nil {
		switch step.Operation {
		case "create":
			res, out := request(http.MethodPost, step.Target, step.Fixture)
			if out.Code != 0 {
				return out
			}
			return rejected(res)
		case "update", "delete":
			res, out := request(http.MethodGet, step.Target, nil)
			if out.Code != 0 {
				return out
			}
			if out = rejected(res); out.Code != 0 {
				return out
			}
			meta := object(res.Object["metadata"])
			if str(meta["uid"]) == "" || str(meta["resourceVersion"]) == "" {
				return failure("api_failed")
			}
			var body any
			method := http.MethodDelete
			if step.Operation == "update" {
				method = http.MethodPut
				replacement := copyObject(step.Fixture)
				m := object(replacement["metadata"])
				for _, key := range []string{"uid", "resourceVersion", "creationTimestamp"} {
					m[key] = meta[key]
				}
				body = replacement
			} else {
				body = map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": meta["uid"], "resourceVersion": meta["resourceVersion"]}}
			}
			res, out = request(method, step.Target, body)
			if out.Code != 0 {
				return out
			}
			return rejected(res)
		}
		return failure("api_failed")
	}
	a := step.Assert
	expected := make([]any, len(a.Fields))
	for i, f := range a.Fields {
		expected[i] = f.Equals
		if f.EqualsFrom != nil {
			v, ok := pointer(observations[f.EqualsFrom.Observation], f.EqualsFrom.Pointer)
			if !ok {
				return failed("reference_missing")
			}
			expected[i] = v
		}
	}
	for {
		res, out := request(http.MethodGet, a.Target, nil)
		record.SampleCount++
		if out.Code != 0 {
			return out
		}
		var matches []map[string]any
		if a.Target.Name != "" {
			if res.Code == 200 {
				matches = append(matches, res.Object)
			} else if res.Code != 404 {
				return failure("api_failed")
			}
		} else {
			if res.Code != 200 {
				return failure("api_failed")
			}
			items, ok := res.Object["items"].([]any)
			if !ok {
				return failure("api_failed")
			}
			for _, raw := range items {
				v, ok := raw.(map[string]any)
				if !ok {
					return failure("api_failed")
				}
				labels := object(object(v["metadata"])["labels"])
				match := true
				for k, want := range a.Target.MatchLabels {
					got, exists := labels[k]
					if !exists || got != want {
						match = false
						break
					}
				}
				if match {
					matches = append(matches, v)
				}
			}
		}
		match := len(matches) == a.Count
		if match && a.Count == 1 {
			for i, f := range a.Fields {
				v, exists := pointer(matches[0], f.Pointer)
				if f.Exists != nil {
					if exists != *f.Exists {
						match = false
					}
				} else if !exists || !equalJSON(v, expected[i]) {
					match = false
				}
			}
		}
		if h := deadlineOutcome(parent, scenarioCtx, stepCtx, s, control, true); h.Code != 0 {
			return h
		}
		if match {
			if a.SaveAs != "" {
				b, e := json.Marshal(matches[0])
				if e != nil {
					return failure("api_failed")
				}
				if *retained+len(b) > 8<<20 {
					return failure("capacity_exceeded")
				}
				*retained += len(b)
				observations[a.SaveAs] = copyObject(matches[0])
			}
			return Outcome{Status: "passed"}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-stepCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
func copyObject(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = copyValue(v)
	}
	return out
}
func copyValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return copyObject(v)
	case []any:
		a := make([]any, len(v))
		for i, x := range v {
			a[i] = copyValue(x)
		}
		return a
	default:
		return v
	}
}
