package recorder

import (
	"errors"
	"fmt"
)

// NewMultiRecorder creates a synchronous fan-out Recorder. Each call is sent
// to every recorder in argument order, even when an earlier recorder fails.
// Multiple failures are returned as one errors.Join-compatible error.
//
// Multi-recorder fan-out does not add synchronization, clone entries, recover
// panics, or own downstream lifecycle. Every supplied recorder must satisfy
// the normal concurrent-use and immutable-entry Recorder contract. Wrap the
// result in AsyncRecorder when fan-out must not run on response-finalization
// goroutines. The first recorder is mandatory; a nil recorder is a wiring
// error and causes NewMultiRecorder to panic.
func NewMultiRecorder(first Recorder, others ...Recorder) Recorder {
	if first == nil {
		panic("recorder: multi recorder 0 is nil")
	}

	for i, recorder := range others {
		if recorder == nil {
			panic(fmt.Sprintf("recorder: multi recorder %d is nil", i+1))
		}
	}

	if len(others) == 0 {
		return first
	}

	recorders := make([]Recorder, 1, len(others)+1)
	recorders[0] = first
	recorders = append(recorders, others...)

	return &multiRecorder{recorders: recorders}
}

type multiRecorder struct {
	recorders []Recorder
}

// Record delivers entry to every downstream recorder without short-circuiting.
func (r *multiRecorder) Record(entry *Entry) error {
	var failures []error

	for i, recorder := range r.recorders {
		if err := recorder.Record(entry); err != nil {
			failures = append(failures, multiRecorderError(i, recorder, err))
		}
	}

	return errors.Join(failures...)
}

// RecordBatch preserves AsyncRecorder batching for capable downstream sinks
// and falls back to ordered Record calls for ordinary recorders.
func (r *multiRecorder) RecordBatch(entries []*Entry) error {
	if len(entries) == 0 {
		return nil
	}

	var failures []error

	for i, recorder := range r.recorders {
		if batch, ok := recorder.(batchRecorder); ok {
			if err := batch.RecordBatch(entries); err != nil {
				failures = append(failures, multiRecorderError(i, recorder, err))
			}

			continue
		}

		for _, entry := range entries {
			if err := recorder.Record(entry); err != nil {
				failures = append(failures, multiRecorderError(i, recorder, err))
			}
		}
	}

	return errors.Join(failures...)
}

func multiRecorderError(index int, recorder Recorder, err error) error {
	return fmt.Errorf("recorder: multi recorder %d (%T): %w", index, recorder, err)
}
