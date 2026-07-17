package recorder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

// BodyMetadata describes the body stream a BodyWriter is created for.
type BodyMetadata struct {
	ExchangeID  string
	Direction   string // "request" or "response"
	ContentType string
	// SizeHint is the expected number of bytes this writer will receive
	// (derived from Content-Length, already clamped to the capture limit),
	// or 0 when unknown. Stores may use it to pre-allocate, but must treat
	// it as untrusted: the actual stream may be shorter or longer, and a
	// hostile peer can announce an absurd Content-Length.
	SizeHint int64
}

// maxPreallocBytes caps how much memory a SizeHint may pre-allocate in one
// step. A lying Content-Length can therefore waste at most this much; larger
// bodies simply grow the buffer as bytes actually arrive.
const maxPreallocBytes = 4 << 20

// BodyWriter receives captured body bytes for one stream. Implementations
// must be safe for concurrent use of Write with Bytes.
type BodyWriter interface {
	io.Writer
	Close() error
	// Bytes returns the bytes captured so far (used when embedding content
	// into the HAR document).
	Bytes() ([]byte, error)
	// Ref returns an external reference to the stored content (e.g. a file
	// path), or "" when the content lives inline in memory.
	Ref() string
}

// BodyStore creates BodyWriter instances. Implementations must be safe for
// concurrent use.
type BodyStore interface {
	NewWriter(ctx context.Context, metadata BodyMetadata) (BodyWriter, error)
}

// MemoryBodyStore keeps captured bodies in memory. It is the default store.
type MemoryBodyStore struct{}

// NewWriter implements BodyStore. A positive SizeHint pre-sizes the buffer
// (bounded by maxPreallocBytes) so growth re-copies are avoided for bodies
// with a truthful Content-Length; a wrong hint costs at most one bounded
// allocation and never breaks the capture.
func (MemoryBodyStore) NewWriter(_ context.Context, meta BodyMetadata) (BodyWriter, error) {
	w := &memoryBodyWriter{}
	if hint := meta.SizeHint; hint > 0 {
		if hint > maxPreallocBytes {
			hint = maxPreallocBytes
		}
		w.buf.Grow(int(hint))
	}
	return w, nil
}

type memoryBodyWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *memoryBodyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *memoryBodyWriter) Close() error { return nil }

func (w *memoryBodyWriter) Bytes() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...), nil
}

func (w *memoryBodyWriter) Ref() string { return "" }

// FileBodyStore spools captured bodies to temporary files so large bodies do
// not have to live in memory. Files are NOT deleted automatically; their
// paths are exposed via BodyInfo.Store and cleanup is the caller's
// responsibility.
type FileBodyStore struct {
	// Dir is the directory temp files are created in; empty means the system
	// temp directory.
	Dir string
}

// NewWriter implements BodyStore.
func (s FileBodyStore) NewWriter(_ context.Context, meta BodyMetadata) (BodyWriter, error) {
	f, err := os.CreateTemp(s.Dir, "recorder-"+meta.Direction+"-*")
	if err != nil {
		return nil, fmt.Errorf("recorder: create body file: %w", err)
	}
	return &fileBodyWriter{f: f}, nil
}

type fileBodyWriter struct {
	mu     sync.Mutex
	f      *os.File
	closed bool
}

func (w *fileBodyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	return w.f.Write(p)
}

func (w *fileBodyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.f.Close()
}

func (w *fileBodyWriter) Bytes() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		if err := w.f.Sync(); err != nil {
			return nil, fmt.Errorf("recorder: sync body file: %w", err)
		}
	}
	b, err := os.ReadFile(w.f.Name())
	if err != nil {
		return nil, fmt.Errorf("recorder: read body file: %w", err)
	}
	return b, nil
}

func (w *fileBodyWriter) Ref() string { return w.f.Name() }
