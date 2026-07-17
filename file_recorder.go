package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// HARFileRecorder buffers finalized entries and writes a complete HAR
// document to a file on Flush (atomically, via a temp file + rename). It is
// safe for concurrent use.
type HARFileRecorder struct {
	mu      sync.Mutex
	path    string
	entries []*Entry
}

// NewHARFileRecorder creates a recorder that will write to path on Flush.
func NewHARFileRecorder(path string) *HARFileRecorder {
	return &HARFileRecorder{path: path}
}

// Record implements Recorder.
func (r *HARFileRecorder) Record(e *Entry) {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	r.mu.Unlock()
}

// Flush writes the full HAR document collected so far. It can be called any
// number of times; each call rewrites the file atomically.
func (r *HARFileRecorder) Flush() error {
	r.mu.Lock()
	entries := append([]*Entry(nil), r.entries...)
	r.mu.Unlock()

	har := NewHAR(entries)
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".recorder-*.har")
	if err != nil {
		return fmt.Errorf("recorder: create HAR temp file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := har.Write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("recorder: close HAR temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("recorder: rename HAR file: %w", err)
	}
	return nil
}

// Close flushes the document; it satisfies io.Closer for defer-friendly use.
func (r *HARFileRecorder) Close() error { return r.Flush() }

// EntriesByTrace implements TraceStore: entries are buffered until Flush, so
// they remain queryable by trace ID.
func (r *HARFileRecorder) EntriesByTrace(traceID string) []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return filterTrace(r.entries, traceID)
}

// RemoveTrace implements TraceStore. Removed entries are excluded from every
// subsequent Flush.
func (r *HARFileRecorder) RemoveTrace(traceID string) int {
	removed, _ := r.remove(traceID, false)
	return removed
}

// TakeTrace implements TraceStore: it atomically removes and returns the
// trace's entries. The taken entries are excluded from every subsequent
// Flush — use this to split one shared recording into per-call HAR files
// (recorder.NewHAR(taken).Write(...)).
func (r *HARFileRecorder) TakeTrace(traceID string) []*Entry {
	_, taken := r.remove(traceID, true)
	return taken
}

func (r *HARFileRecorder) remove(traceID string, collect bool) (int, []*Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept, taken, removed := splitTrace(r.entries, traceID, collect)
	r.entries = kept
	return removed, taken
}

// JSONStreamRecorder writes every finalized entry immediately as one JSON
// document per line (NDJSON) to the underlying writer. This is the streaming
// export path: it deliberately does not emit the enclosing HAR wrapper, so
// the top-level HAR JSON structure is never left half-written; consumers can
// wrap the lines into a log object themselves. Safe for concurrent use.
//
// JSONStreamRecorder intentionally does not implement TraceStore: entries
// leave the process the moment they are recorded, so there is nothing left
// to query or remove. Group downstream by each line's "_traceId" field, or
// use a retaining recorder (MemoryRecorder, HARFileRecorder) when per-trace
// access is needed.
type JSONStreamRecorder struct {
	mu  sync.Mutex
	enc *json.Encoder
	err error
}

// NewJSONStreamRecorder creates a streaming recorder writing to w.
func NewJSONStreamRecorder(w io.Writer) *JSONStreamRecorder {
	return &JSONStreamRecorder{enc: json.NewEncoder(w)}
}

// Record implements Recorder. Encoding errors are retained and observable via
// Err; recording never panics.
func (r *JSONStreamRecorder) Record(e *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.enc.Encode(e); err != nil && r.err == nil {
		r.err = fmt.Errorf("recorder: encode entry: %w", err)
	}
}

// Err returns the first encoding error encountered, if any.
func (r *JSONStreamRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
