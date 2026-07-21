package recorder

import (
	"errors"
	"io"
	"sync"
)

// DefaultMemoryRecorderCapacity is the maximum number of finalized entries
// retained by NewMemoryRecorder.
const DefaultMemoryRecorderCapacity = 1024

// MemoryRecorderStats is an atomic point-in-time snapshot of retention state.
// Evicted is a lifetime counter and is not cleared by Reset.
type MemoryRecorderStats struct {
	Capacity int
	Retained int
	Evicted  uint64
}

// MemoryRecorder collects finalized entries in memory. It is safe for
// concurrent use. Entries handed to it are immutable snapshots, so the copies
// returned by Entries can be shared freely.
type MemoryRecorder struct {
	mu      sync.Mutex
	entries []*Entry
	head    int
	size    int
	evicted uint64
}

// NewMemoryRecorder returns an empty in-memory recorder retaining the newest
// DefaultMemoryRecorderCapacity finalized entries. Once full, recording a new
// entry evicts the oldest retained entry.
func NewMemoryRecorder() *MemoryRecorder {
	return newMemoryRecorder(DefaultMemoryRecorderCapacity)
}

// NewMemoryRecorderWithCapacity returns an empty in-memory recorder retaining
// the newest capacity finalized entries. Capacity must be positive.
func NewMemoryRecorderWithCapacity(capacity int) (*MemoryRecorder, error) {
	if capacity <= 0 {
		return nil, errors.New("recorder: memory recorder capacity must be positive")
	}

	return newMemoryRecorder(capacity), nil
}

func newMemoryRecorder(capacity int) *MemoryRecorder {
	return &MemoryRecorder{entries: make([]*Entry, capacity)}
}

// Record implements Recorder.
func (r *MemoryRecorder) Record(e *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.recordLocked(e)
}

// RecordBatch implements batchRecorder with one lock acquisition.
func (r *MemoryRecorder) RecordBatch(entries []*Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, entry := range entries {
		r.recordLocked(entry)
	}
}

func (r *MemoryRecorder) recordLocked(e *Entry) {
	if r.size == len(r.entries) {
		r.entries[r.head] = nil
		r.head = (r.head + 1) % len(r.entries)
		r.size--
		r.evicted++
	}

	index := (r.head + r.size) % len(r.entries)
	r.entries[index] = e
	r.size++
}

// Entries returns a copy of the recorded entries in recording order.
func (r *MemoryRecorder) Entries() []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.entriesLocked()
}

// Snapshot atomically returns the retained entries in recording order and the
// corresponding retention statistics.
func (r *MemoryRecorder) Snapshot() ([]*Entry, MemoryRecorderStats) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.entriesLocked(), r.statsLocked()
}

// Stats returns an atomic point-in-time snapshot of retention state.
func (r *MemoryRecorder) Stats() MemoryRecorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.statsLocked()
}

// Len returns the number of recorded entries.
func (r *MemoryRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.size
}

// Reset discards all recorded entries.
func (r *MemoryRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.entries {
		r.entries[i] = nil
	}

	r.head = 0
	r.size = 0
}

// EntriesByTrace returns a copy of the entries belonging to the given trace
// ID (see WithTraceID), in recording order. The recorder keeps the entries.
func (r *MemoryRecorder) EntriesByTrace(traceID string) []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	return filterTrace(r.entriesLocked(), traceID)
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

	kept, taken, removed := splitTrace(r.entriesLocked(), traceID, collect)
	for i := range r.entries {
		r.entries[i] = nil
	}

	copy(r.entries, kept)
	r.head = 0
	r.size = len(kept)

	return removed, taken
}

func (r *MemoryRecorder) entriesLocked() []*Entry {
	entries := make([]*Entry, r.size)
	for i := range r.size {
		entries[i] = r.entries[(r.head+i)%len(r.entries)]
	}

	return entries
}

func (r *MemoryRecorder) statsLocked() MemoryRecorderStats {
	return MemoryRecorderStats{
		Capacity: len(r.entries),
		Retained: r.size,
		Evicted:  r.evicted,
	}
}

// filterTrace returns the entries matching traceID, preserving order.
func filterTrace(entries []*Entry, traceID string) []*Entry {
	var matched []*Entry

	for _, e := range entries {
		if e != nil && e.Recorder != nil && e.Recorder.TraceID == traceID {
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
		if e != nil && e.Recorder != nil && e.Recorder.TraceID == traceID {
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
