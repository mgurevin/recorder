package recorder

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type commitFailBodyWriter struct {
	aborted bool
}

func (w *commitFailBodyWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *commitFailBodyWriter) Commit() error               { return errors.New("commit failed") }
func (w *commitFailBodyWriter) Abort() error {
	w.aborted = true

	return nil
}

type opaqueBodyStore struct {
	writer   *opaqueBodyWriter
	refCalls int
}

func (s *opaqueBodyStore) NewWriter(context.Context, BodyMetadata) (BodyWriter, error) {
	s.writer = &opaqueBodyWriter{}

	return s.writer, nil
}

func (s *opaqueBodyStore) Reference(writer BodyWriter) string {
	s.refCalls++

	w, ok := writer.(*opaqueBodyWriter)
	if !ok || w != s.writer || !w.committed {
		return ""
	}

	return "opaque:body"
}

type opaqueBodyWriter struct {
	bytes.Buffer
	committed bool
	aborted   bool
}

func (w *opaqueBodyWriter) Commit() error {
	w.committed = true

	return nil
}

func (w *opaqueBodyWriter) Abort() error {
	w.aborted = true

	return nil
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func TestRedactingBodyWriterAbortsUnderlyingWriterWhenCommitFails(t *testing.T) {
	underlying := &commitFailBodyWriter{}
	buffered := bufio.NewWriter(underlying)
	w := &redactingBodyWriter{
		BodyWriter: underlying,
		redactor:   nopWriteCloser{Writer: buffered},
		buf:        buffered,
	}

	if err := w.Commit(); err == nil {
		t.Fatal("Commit succeeded")
	}

	if !underlying.aborted {
		t.Fatal("underlying writer was not aborted after failed commit")
	}
}

func TestBodyCaptureResetAbortsPreviousFile(t *testing.T) {
	store := mustFileBodyStore(t, t.TempDir())
	capture := newBodyCapture(context.Background(), store, BodyMetadata{}, "", true, true, 1024, "", false, nil, nil, nil)
	capture.observe([]byte("first attempt"))

	if stats := store.Stats(); stats.PartialFiles != 1 {
		t.Fatalf("before reset stats = %+v", stats)
	}

	capture.reset()

	if stats := store.Stats(); stats.PartialFiles != 0 || stats.AbortedTotal != 1 {
		t.Fatalf("after reset stats = %+v", stats)
	}

	capture.observe([]byte("final attempt"))
	capture.finishComplete()

	if stats := store.Stats(); stats.CommittedFiles != 1 || stats.PartialFiles != 0 {
		t.Fatalf("after final attempt stats = %+v", stats)
	}
}

func TestBodyCaptureOwnsBoundedEmbeddedRepresentation(t *testing.T) {
	store := &opaqueBodyStore{}
	red := newRedactor(&Config{})
	capture := newBodyCapture(
		context.Background(),
		store,
		BodyMetadata{SizeHint: 1 << 50},
		"",
		true,
		true,
		4,
		"",
		false,
		nil,
		nil,
		nil,
	)

	if capacity := cap(capture.embedded.Bytes()); capacity > 1024 {
		t.Fatalf("embedded buffer pre-allocated %d bytes", capacity)
	}

	capture.observe([]byte("abcdef"))

	if info := capture.info(red); info.Store != "" || store.refCalls != 0 {
		t.Fatalf("reference published before commit: info=%+v calls=%d", info, store.refCalls)
	}

	capture.finishComplete()

	if got := string(capture.bytes()); got != "abcd" {
		t.Fatalf("embedded body = %q", got)
	}

	if got := store.writer.String(); got != "abcd" {
		t.Fatalf("stored body = %q", got)
	}

	if info := capture.info(red); info.Store != "opaque:body" || store.refCalls != 1 {
		t.Fatalf("committed reference: info=%+v calls=%d", info, store.refCalls)
	}

	if !capture.isTruncated() || !store.writer.committed || store.writer.aborted {
		t.Fatalf(
			"truncated=%v committed=%v aborted=%v",
			capture.isTruncated(),
			store.writer.committed,
			store.writer.aborted,
		)
	}
}

func TestBodyCaptureEmbedsProcessedStreamOnce(t *testing.T) {
	store := &opaqueBodyStore{}
	red := newRedactor(&Config{Redaction: RedactionConfig{Common: RedactionRules{
		JSONFields: []string{"password"},
	}}})
	capture := newBodyCapture(
		context.Background(),
		store,
		BodyMetadata{ContentType: "application/json"},
		"",
		true,
		true,
		1024,
		"",
		false,
		red,
		nil,
		nil,
	)

	capture.observe([]byte(`{"password":"secret",`))
	capture.observe([]byte(`"keep":"evidence"}`))
	capture.finishComplete()

	embedded := capture.bytes()
	stored := store.writer.Bytes()

	if !bytes.Equal(embedded, stored) {
		t.Fatalf("embedded = %q, stored = %q", embedded, stored)
	}

	if bytes.Contains(embedded, []byte("secret")) ||
		!bytes.Contains(embedded, []byte(redactedValue)) ||
		!bytes.Contains(embedded, []byte("evidence")) {
		t.Fatalf("processed body = %q", embedded)
	}
}

func TestBodyCaptureSkipsEmbeddingBufferWhenDisabled(t *testing.T) {
	store := &opaqueBodyStore{}
	capture := newBodyCapture(
		context.Background(),
		store,
		BodyMetadata{},
		"",
		true,
		false,
		1024,
		"",
		false,
		nil,
		nil,
		nil,
	)

	capture.observe([]byte("external only"))
	capture.finishComplete()

	if body := capture.bytes(); body != nil {
		t.Fatalf("embedded body = %q", body)
	}

	if capture.embedded.Len() != 0 || store.writer.String() != "external only" {
		t.Fatalf("embedded=%d stored=%q", capture.embedded.Len(), store.writer.String())
	}
}
