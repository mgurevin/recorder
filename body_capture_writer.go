package recorder

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
)

var errDecodedBodyTooLarge = errors.New("recorder: decoded body exceeds capture limit")

// newCaptureBodyWriter builds the write side of one body capture. The
// decorators are applied once in stream order: optional embedding observes
// the processed store writes, while redaction and decoding sit in front of
// that destination.
func newCaptureBodyWriter(
	dst BodyWriter,
	embedded *bytes.Buffer,
	embed bool,
	contentEncoding string,
	needsRedaction bool,
	decoder ContentDecoder,
	contentType string,
	red *redactor,
	limit int64,
) (BodyWriter, bool, bool) {
	if embed {
		dst = &embeddingBodyWriter{BodyWriter: dst, embedded: embedded}
	}

	if contentEncoding == "" || contentEncoding == "identity" {
		buffered := bufio.NewWriterSize(dst, 32<<10)
		if streamRedactor := newBodyStreamRedactor(buffered, contentType, red); streamRedactor != nil {
			return &redactingBodyWriter{
				BodyWriter: dst,
				redactor:   streamRedactor,
				buf:        buffered,
			}, false, true
		}

		return dst, false, false
	}

	if needsRedaction {
		return newDecodingRedactingBodyWriter(dst, decoder, contentType, red, limit), true, true
	}

	return dst, false, false
}

type embeddingBodyWriter struct {
	BodyWriter
	embedded *bytes.Buffer
}

func (w *embeddingBodyWriter) Write(p []byte) (int, error) {
	n, err := w.BodyWriter.Write(p)
	if n > 0 {
		_, _ = w.embedded.Write(p[:n])
	}

	return n, err
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

func (w *redactingBodyWriter) flushForSnapshot() error {
	return w.buf.Flush()
}

// decodingRedactingBodyWriter streams encoded input through a ContentDecoder
// and then through the structured redactor before it reaches the BodyStore.
// The pipe intentionally applies backpressure: memory remains bounded by the
// decoder, parser state, and io.Pipe's synchronous handoff. As with entry
// finalization, the caller must read or close the HTTP body; otherwise the
// capture pipeline, including this worker, remains live with that body.
type decodingRedactingBodyWriter struct {
	BodyWriter
	pw        *io.PipeWriter
	done      chan error
	mu        sync.Mutex
	finalized bool
}

func newDecodingRedactingBodyWriter(
	dst BodyWriter,
	decoder ContentDecoder,
	mimeType string,
	red *redactor,
	limit int64,
) BodyWriter {
	pr, pw := io.Pipe()
	w := &decodingRedactingBodyWriter{BodyWriter: dst, pw: pw, done: make(chan error, 1)}

	go func() {
		decoded, err := decoder(pr)
		if err != nil {
			_ = pr.CloseWithError(err)
			w.done <- fmt.Errorf("recorder: open streaming body decoder: %w", err)

			return
		}

		buffered := bufio.NewWriterSize(dst, 32<<10)

		streamRedactor := newBodyStreamRedactor(buffered, mimeType, red)
		if streamRedactor == nil {
			err := errors.New("recorder: selected body redactor is unavailable")
			_ = decoded.Close()

			_ = pr.CloseWithError(err)
			w.done <- err

			return
		}

		var copied int64

		buf := make([]byte, 32<<10)

		for err == nil {
			var n int

			n, err = decoded.Read(buf)
			if n > 0 {
				if limit > 0 && copied+int64(n) > limit {
					err = errDecodedBodyTooLarge
					break
				}

				copied += int64(n)
				if _, writeErr := streamRedactor.Write(buf[:n]); writeErr != nil {
					err = writeErr
					break
				}
			}
		}

		if errors.Is(err, io.EOF) {
			err = nil
		}

		if closeErr := decoded.Close(); err == nil {
			err = closeErr
		}

		if closeErr := streamRedactor.Close(); err == nil {
			err = closeErr
		}

		if flushErr := buffered.Flush(); err == nil {
			err = flushErr
		}

		_ = pr.CloseWithError(err)
		w.done <- err
	}()

	return w
}

func (w *decodingRedactingBodyWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

func (w *decodingRedactingBodyWriter) Commit() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.finalized {
		return nil
	}

	w.finalized = true

	pipeErr := w.pw.Close()

	decodeErr := <-w.done
	if decodeErr != nil {
		_ = w.BodyWriter.Abort()

		return decodeErr
	}

	if pipeErr != nil {
		_ = w.BodyWriter.Abort()

		return pipeErr
	}

	if err := w.BodyWriter.Commit(); err != nil {
		_ = w.BodyWriter.Abort()

		return err
	}

	return nil
}

func (w *decodingRedactingBodyWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.finalized {
		return nil
	}

	w.finalized = true

	_ = w.pw.CloseWithError(errors.New("recorder: body capture aborted"))
	<-w.done

	return w.BodyWriter.Abort()
}
