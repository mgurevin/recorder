package recorder

import (
	"io"
	"sync"
)

// MemoryRecorder collects finalized entries in memory. It is safe for
// concurrent use. Entries handed to it are immutable snapshots, so the copies
// returned by Entries can be shared freely.
type MemoryRecorder struct {
	mu      sync.Mutex
	entries []*Entry
}

// NewMemoryRecorder returns an empty in-memory recorder.
func NewMemoryRecorder() *MemoryRecorder { return &MemoryRecorder{} }

// Record implements Recorder.
func (r *MemoryRecorder) Record(e *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = append(r.entries, e)
}

// Entries returns a copy of the recorded entries in recording order.
func (r *MemoryRecorder) Entries() []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]*Entry(nil), r.entries...)
}

// Len returns the number of recorded entries.
func (r *MemoryRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.entries)
}

// Reset discards all recorded entries.
func (r *MemoryRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = nil
}

// EntriesByTrace returns a copy of the entries belonging to the given trace
// ID (see WithTraceID), in recording order. The recorder keeps the entries.
func (r *MemoryRecorder) EntriesByTrace(traceID string) []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	return filterTrace(r.entries, traceID)
}

// RemoveTrace deletes every entry belonging to the given trace ID and
// returns how many were removed.
func (r *MemoryRecorder) RemoveTrace(traceID string) int {
	removed, _ := r.remove(traceID, false)
	return removed
}

// TakeTrace atomically removes and returns the entries belonging to the
// given trace ID, in recording order. Atomicity matters on a shared client:
// an entry finalized concurrently can never be lost between a query and a
// separate delete.
func (r *MemoryRecorder) TakeTrace(traceID string) []*Entry {
	_, taken := r.remove(traceID, true)
	return taken
}

func (r *MemoryRecorder) remove(traceID string, collect bool) (int, []*Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept, taken, removed := splitTrace(r.entries, traceID, collect)
	r.entries = kept

	return removed, taken
}

// filterTrace returns the entries matching traceID, preserving order.
func filterTrace(entries []*Entry, traceID string) []*Entry {
	var matched []*Entry

	for _, e := range entries {
		if e.TraceID == traceID {
			matched = append(matched, e)
		}
	}

	return matched
}

// splitTrace compacts non-matching entries to the front of the slice in
// place, optionally collecting the matching ones, and nils out the vacated
// tail so removed entries are released to the GC.
func splitTrace(entries []*Entry, traceID string, collect bool) (kept, taken []*Entry, removed int) {
	kept = entries[:0]
	for _, e := range entries {
		if e.TraceID == traceID {
			if collect {
				taken = append(taken, e)
			}

			continue
		}

		kept = append(kept, e)
	}

	removed = len(entries) - len(kept)
	for i := len(kept); i < len(entries); i++ {
		entries[i] = nil
	}

	return kept, taken, removed
}

// HAR builds a HAR 1.2 document from the entries recorded so far, ordered by
// request start time. In-flight exchanges are absent by definition: entries
// only reach the recorder once finalized.
func (r *MemoryRecorder) HAR() *HAR { return NewHAR(r.Entries()) }

// HARForTrace builds a HAR 1.2 document containing only the entries of the
// given trace ID (the recorder keeps them; combine with TakeTrace +
// NewHAR to export-and-drop in one step).
func (r *MemoryRecorder) HARForTrace(traceID string) *HAR {
	return NewHAR(r.EntriesByTrace(traceID))
}

// WriteHAR serializes the current HAR document to w as indented JSON.
func (r *MemoryRecorder) WriteHAR(w io.Writer) error { return r.HAR().Write(w) }
