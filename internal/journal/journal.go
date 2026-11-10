// Package journal records the bounded lifecycle history of a single M0 session.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"operator-dynamo/internal/contracts"
)

type Output interface {
	io.WriteCloser
	Sync() error
}
type Journal struct {
	mu         sync.Mutex
	output     Output
	session    string
	sequence   uint64
	failure    error
	closed     bool
	projection *Windows
}

func New(output Output, session string) *Journal { return &Journal{output: output, session: session} }
func (j *Journal) Append(state string) error {
	return j.Record(contracts.Observation{Kind: "session." + state, CorrelationID: j.session, Payload: contracts.LifecyclePayload{State: state}})
}
func (j *Journal) Record(observation contracts.Observation) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return errors.New("journal closed")
	}
	if j.failure != nil {
		return j.failure
	}
	next := j.sequence + 1
	event := contracts.Event{SchemaVersion: 1, SessionID: j.session, EventID: fmt.Sprintf("%s:%d", j.session, next), Sequence: next, Timestamp: time.Now().UTC(), Kind: observation.Kind, Actor: "unknown", CorrelationID: observation.CorrelationID, Payload: observation.Payload}
	if len(observation.Kind) >= 8 && observation.Kind[:8] == "session." {
		event.Actor = "system"
	}
	if observation.Kind == "scenario.started" || observation.Kind == "scenario.completed" || observation.Kind == "step.started" || observation.Kind == "step.completed" {
		event.Actor = "test"
	}
	data, err := json.Marshal(event)
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = j.output.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		j.failure = err
		return err
	}
	j.sequence = next
	if j.projection != nil {
		j.projection.append(j.session, next, data)
	}
	return nil
}
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return j.failure
	}
	j.closed = true
	j.failure = errors.Join(j.failure, j.output.Sync(), j.output.Close())
	return j.failure
}

// SetProjection is called by composition before the first Append.
func (j *Journal) SetProjection(w *Windows) { j.mu.Lock(); defer j.mu.Unlock(); j.projection = w }
