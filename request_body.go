package recorder

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
)

// bodyCapture observes one body stream (request or response) as a tee: it
// counts every byte that flows, hashes the full stream, and stores content up
// to the configured limit in a BodyStore. It never generates reads of its
// own and never buffers the whole body for a later rewrite. Body
// redaction uses bounded parser/output buffers. A failing store or redactor
// only stops content capture — counting and the HTTP flow itself continue.
//
// bodyCapture is safe for concurrent use: the transport may still be
// streaming the request body from a background goroutine while the exchange
// is being finalized.
type bodyCapture struct {
	mu sync.Mutex

	ctx             context.Context
	store           BodyStore
	meta            BodyMetadata
	contentEncoding string
	limit           int64 // <= 0 means unlimited
	captureContent  bool
	onInternal      func(error)
	red             *redactor
	decoder         ContentDecoder

	w              BodyWriter
	storeFailed    bool
	storedDecoded  bool
	storedRedacted bool
	h              hash.Hash
	hashName       string

	// expected is the announced Content-Length (-1 when unknown). When a
	// stream is closed after exactly expected bytes flowed, it is complete
	// even without an explicit EOF read: http.Transport reads exactly
	// Content-Length bytes from a request body and may never issue the
	// final Read that would return io.EOF.
	expected int64

	finished    bool
	complete    bool
	closedEarly bool
	truncated   bool
	captured    int64
	total       int64
	readErr     error
	closeErr    error
}

func newBodyCapture(ctx context.Context, store BodyStore, meta BodyMetadata,
	contentEncoding string, captureContent bool, limit int64, hashAlg string, hashBody bool, red *redactor, decoder ContentDecoder, onInternal func(error),
) *bodyCapture {
	c := &bodyCapture{
		ctx:             ctx,
		store:           store,
		meta:            meta,
		contentEncoding: contentEncoding,
		limit:           limit,
		captureContent:  captureContent,
		onInternal:      onInternal,
		red:             red,
		decoder:         decoder,
		expected:        -1,
	}
	if hashBody {
		c.h, c.hashName = newBodyHash(hashAlg)
	}

	return c
}

func newBodyHash(alg string) (hash.Hash, string) {
	switch strings.ToLower(alg) {
	case "sha1":
		return sha1.New(), "sha1"

	case "md5":
		return md5.New(), "md5"

	default:
		return sha256.New(), "sha256"
	}
}

// observe records bytes that just flowed through the stream.
func (c *bodyCapture) observe(p []byte) {
	if c == nil || len(p) == 0 {
		return
	}

	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}

	c.total += int64(len(p))
	if c.h != nil {
		c.h.Write(p)
	}

	if !c.captureContent {
		c.mu.Unlock()
		return
	}

	take := int64(len(p))

	if c.limit > 0 {
		room := c.limit - c.captured
		if room <= 0 {
			c.truncated = true
			c.mu.Unlock()

			return
		}

		if room < take {
			take = room
			c.truncated = true
		}
	}

	if c.storeFailed {
		c.mu.Unlock()
		return
	}

	var internalErr error

	if c.w == nil {
		enc := strings.ToLower(strings.TrimSpace(c.contentEncoding))

		needsRedaction := bodyStreamRedactionEnabled(c.meta.ContentType, c.red)
		if enc != "" && enc != "identity" && needsRedaction && c.decoder == nil {
			c.storeFailed = true
			internalErr = fmt.Errorf("recorder: no streaming decoder for redacted %s body", enc)
			c.mu.Unlock()
			c.internal(internalErr)

			return
		}

		w, err := c.store.NewWriter(c.ctx, c.meta)
		if err != nil {
			c.storeFailed = true
			internalErr = fmt.Errorf("recorder: open body store writer: %w", err)
			c.mu.Unlock()
			c.internal(internalErr)

			return
		}

		c.w = w
		if enc == "" || enc == "identity" {
			buf := bufio.NewWriterSize(w, 32<<10)
			if sr := newBodyStreamRedactor(buf, c.meta.ContentType, c.red); sr != nil {
				c.w = &redactingBodyWriter{BodyWriter: w, redactor: sr, buf: buf}
				c.storedRedacted = true
			}
		} else if needsRedaction {
			c.w = newDecodingRedactingBodyWriter(w, c.decoder, c.meta.ContentType, c.red, c.limit)
			c.storedDecoded = true
			c.storedRedacted = true
		}
	}

	n, err := c.w.Write(p[:take])

	c.captured += int64(n)
	if err != nil {
		c.storeFailed = true

		internalErr = fmt.Errorf("recorder: write body store: %w", err)
		if abortErr := c.abortWriterLocked(); abortErr != nil {
			internalErr = errors.Join(internalErr, abortErr)
		}
	}
	c.mu.Unlock()
	c.internal(internalErr)
}

type redactingBodyWriter struct {
	BodyWriter
	redactor  io.WriteCloser
	buf       *bufio.Writer
	mu        sync.Mutex
	finalized bool
}

func (w *redactingBodyWriter) Write(p []byte) (int, error) {
	return w.redactor.Write(p)
}

func (w *redactingBodyWriter) Bytes() ([]byte, error) {
	if err := w.buf.Flush(); err != nil {
		return nil, err
	}

	return w.BodyWriter.Bytes()
}

func (w *redactingBodyWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.finalized {
		return nil
	}

	w.finalized = true

	redactErr := w.redactor.Close()
	flushErr := w.buf.Flush()

	if redactErr != nil {
		_ = w.BodyWriter.Abort()

		return redactErr
	}

	if flushErr != nil {
		_ = w.BodyWriter.Abort()

		return flushErr
	}

	if err := w.BodyWriter.Commit(); err != nil {
		_ = w.BodyWriter.Abort()

		return err
	}

	return nil
}

func (w *redactingBodyWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.finalized {
		return nil
	}

	w.finalized = true

	_ = w.redactor.Close()

	return w.BodyWriter.Abort()
}

func (c *bodyCapture) internal(err error) {
	if err != nil && c.onInternal != nil {
		c.onInternal(err)
	}
}

// finishComplete marks the stream as fully consumed (EOF observed).
func (c *bodyCapture) finishComplete() {
	if c == nil {
		return
	}

	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}

	c.finished = true
	c.complete = true
	err := c.commitWriterLocked()
	c.mu.Unlock()
	c.internal(err)
}

// fail records a read error terminating the stream.
func (c *bodyCapture) fail(err error) {
	if c == nil {
		return
	}

	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}

	c.finished = true
	c.readErr = err
	closeErr := c.commitWriterLocked()
	c.mu.Unlock()
	c.internal(closeErr)
}

// closed records the stream being closed; when it had not already finished,
// that is an early close. closeErr is the error returned by the underlying
// Close call, recorded regardless of stream state.
func (c *bodyCapture) closed(closeErr error) {
	if c == nil {
		return
	}

	c.mu.Lock()
	if closeErr != nil {
		c.closeErr = closeErr
	}

	if c.finished {
		c.mu.Unlock()
		return
	}

	c.finished = true
	if c.expected >= 0 && c.total == c.expected {
		c.complete = true
	} else {
		c.closedEarly = true
	}

	err := c.commitWriterLocked()
	c.mu.Unlock()
	c.internal(err)
}

// setExpected records the announced Content-Length. Values <= 0 mean unknown
// (for client requests a 0 Content-Length with a non-nil body means unknown).
func (c *bodyCapture) setExpected(n int64) {
	if c == nil || n <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.expected = n
}

func (c *bodyCapture) commitWriterLocked() error {
	if c.w == nil {
		return nil
	}

	if c.storeFailed {
		return c.abortWriterLocked()
	}

	if err := c.w.Commit(); err != nil {
		c.storeFailed = true
		_ = c.w.Abort()

		return fmt.Errorf("recorder: commit body store writer: %w", err)
	}

	return nil
}

func (c *bodyCapture) abortWriterLocked() error {
	if c.w == nil {
		return nil
	}

	if err := c.w.Abort(); err != nil {
		return fmt.Errorf("recorder: abort body store writer: %w", err)
	}

	return nil
}

// reset restarts the capture. Used when the transport replays the request
// body via GetBody (internal retry): only the bytes of the final attempt are
// kept, matching what was actually sent on the successful exchange.
func (c *bodyCapture) reset() {
	if c == nil {
		return
	}

	c.mu.Lock()
	closeErr := c.abortWriterLocked()
	c.w = nil
	c.storeFailed = false
	c.storedDecoded = false
	c.storedRedacted = false
	c.finished, c.complete, c.closedEarly, c.truncated = false, false, false, false
	c.captured, c.total = 0, 0

	c.readErr, c.closeErr = nil, nil
	if c.h != nil {
		c.h.Reset()
	}
	c.mu.Unlock()
	c.internal(closeErr)
}

// bytes returns the captured content, nil when nothing was stored.
func (c *bodyCapture) bytes() []byte {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	if c.w == nil || c.storeFailed {
		c.mu.Unlock()
		return nil
	}

	b, err := c.w.Bytes()
	c.mu.Unlock()

	if err != nil {
		c.internal(fmt.Errorf("recorder: read body store: %w", err))
		return nil
	}

	return b
}

func (c *bodyCapture) totalBytes() int64 {
	if c == nil {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.total
}

func (c *bodyCapture) isComplete() bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.complete
}

func (c *bodyCapture) isTruncated() bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.truncated
}

func (c *bodyCapture) isStoredDecoded() bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.storedDecoded
}

func (c *bodyCapture) isStoredRedacted() bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.storedRedacted
}

func (c *bodyCapture) readError() error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.readErr
}

// info builds the "_requestBody"/"_responseBody" extension snapshot.
func (c *bodyCapture) info(red *redactor) *BodyInfo {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	bi := &BodyInfo{
		Present:       true,
		Complete:      c.complete,
		ClosedEarly:   c.closedEarly,
		Truncated:     c.truncated,
		CapturedBytes: c.captured,
		TotalBytes:    c.total,
	}
	if c.h != nil && c.complete {
		bi.Hash = hex.EncodeToString(c.h.Sum(nil))
		bi.HashAlgorithm = c.hashName
	}

	if c.readErr != nil {
		bi.ReadError = red.redactError(c.readErr.Error())
	}

	if c.closeErr != nil {
		bi.CloseError = red.redactError(c.closeErr.Error())
	}

	if c.w != nil {
		bi.Store = c.w.Ref()
	}

	return bi
}

// requestBodyRecorder tees the caller-supplied request body while the
// transport streams it. Read and Close semantics of the wrapped body are
// passed through unchanged; only the bytes the transport actually read are
// recorded.
type requestBodyRecorder struct {
	rc io.ReadCloser
	bc *bodyCapture
	ex *exchange
}

func (r *requestBodyRecorder) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.bc.observe(p[:n])
		r.ex.setState(StateRequestBodyStreaming)
	}

	switch {
	case err == io.EOF:
		r.bc.finishComplete()

	case err != nil:
		r.bc.fail(err)
	}

	return n, err
}

func (r *requestBodyRecorder) Close() error {
	err := r.rc.Close()
	r.bc.closed(err)

	return err
}
