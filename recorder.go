package recorder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"
)

// Recorder receives finalized, immutable entries. Implementations must be
// safe for concurrent use: entries arrive from whichever goroutine finishes
// reading a response body.
type Recorder interface {
	Record(entry *Entry)
}

// TraceStore is an optional capability for recorders that retain entries and
// can query or remove them by trace ID (see WithTraceID). The Recorder
// interface itself stays minimal — a custom recorder is still just one
// method — and callers discover the capability by type assertion:
//
//	if ts, ok := rec.(TraceStore); ok {
//		entries := ts.TakeTrace(id)
//	}
//
// MemoryRecorder and HARFileRecorder implement TraceStore.
// JSONStreamRecorder cannot: its entries leave the process the moment they
// are recorded, so there is nothing left to query or remove.
type TraceStore interface {
	// EntriesByTrace returns the entries belonging to the trace ID, in
	// recording order, without removing them.
	EntriesByTrace(traceID string) []*Entry
	// RemoveTrace deletes the trace's entries and returns how many were
	// removed.
	RemoveTrace(traceID string) int
	// TakeTrace atomically removes and returns the trace's entries, in
	// recording order.
	TakeTrace(traceID string) []*Entry
}

// Compile-time capability checks.
var (
	_ TraceStore = (*MemoryRecorder)(nil)
	_ TraceStore = (*HARFileRecorder)(nil)
)

// RecorderFunc adapts a function into a Recorder (the "callback recorder").
// The function must be safe for concurrent use.
type RecorderFunc func(*Entry)

// Record implements Recorder.
func (f RecorderFunc) Record(e *Entry) { f(e) }

// OnEntryCompleted is invoked with the request context and the finished HAR
// entry every time an exchange is finalized.
type OnEntryCompleted func(context.Context, *Entry)

type traceCtxKey struct{}

// traceState correlates the exchanges of one logical operation. The atomic
// counter yields the redirect index: http.Client carries the original request
// context across redirect hops, so every hop sees the same state.
type traceState struct {
	id  string
	seq atomic.Int64
}

// WithTraceID returns a context carrying the given correlation ID. Every
// exchange started under this context records the ID as "_traceId" and an
// incrementing "_redirectIndex", which links redirect chains followed by
// http.Client into one logical trace.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, &traceState{id: id})
}

// TraceContext is a convenience wrapper around WithTraceID that generates a
// fresh random trace ID and returns it alongside the derived context.
func TraceContext(ctx context.Context) (context.Context, string) {
	id := newID()
	return WithTraceID(ctx, id), id
}

// TraceIDFromContext returns the correlation ID installed by WithTraceID.
func TraceIDFromContext(ctx context.Context) (string, bool) {
	if ts, ok := ctx.Value(traceCtxKey{}).(*traceState); ok {
		return ts.id, true
	}

	return "", false
}

func traceStateFromContext(ctx context.Context) *traceState {
	ts, _ := ctx.Value(traceCtxKey{}).(*traceState)
	return ts
}

// newID returns a 128-bit random hex identifier.
func newID() string {
	var b [16]byte

	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail on supported platforms

	return hex.EncodeToString(b[:])
}
