package recorder

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type byteWriterProbe struct {
	bytes.Buffer
	writes     int
	byteWrites int
}

func (w *byteWriterProbe) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func (w *byteWriterProbe) WriteByte(b byte) error {
	w.byteWrites++
	return w.Buffer.WriteByte(b)
}

type writerOnlyProbe struct{ bytes.Buffer }

type discardByteWriter struct{}

func (discardByteWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardByteWriter) WriteByte(byte) error        { return nil }

type writerOnlyDiscard struct{}

func (writerOnlyDiscard) Write(p []byte) (int, error) { return len(p), nil }

type shortByteWriter struct{}

func (shortByteWriter) Write([]byte) (int, error) { return 0, nil }

type failedByteWriter struct{ err error }

func (w failedByteWriter) Write([]byte) (int, error) { return 0, w.err }

func TestByteSinkUsesByteWriterFastPath(t *testing.T) {
	dst := &byteWriterProbe{}
	sink := newByteSink(dst)
	if err := sink.WriteByte('x'); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "x" {
		t.Fatalf("output = %q", got)
	}
	if dst.byteWrites != 1 || dst.writes != 0 {
		t.Fatalf("byte writes = %d, slice writes = %d", dst.byteWrites, dst.writes)
	}
}

func TestByteSinkWriterFallback(t *testing.T) {
	dst := &writerOnlyProbe{}
	sink := newByteSink(dst)
	if err := sink.WriteByte('x'); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteByte('y'); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "xy" {
		t.Fatalf("output = %q", got)
	}
}

func TestByteSinkReportsWriterFailures(t *testing.T) {
	shortSink := newByteSink(shortByteWriter{})
	if err := shortSink.WriteByte('x'); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v", err)
	}
	want := errors.New("write failed")
	failedSink := newByteSink(failedByteWriter{err: want})
	if err := failedSink.WriteByte('x'); !errors.Is(err, want) {
		t.Fatalf("write error = %v", err)
	}
}

func TestByteSinkWritesWithoutPerByteAllocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		dst  io.Writer
	}{
		{"byte writer", discardByteWriter{}},
		{"writer fallback", writerOnlyDiscard{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newByteSink(tc.dst)
			if allocs := testing.AllocsPerRun(1000, func() {
				if err := sink.WriteByte('x'); err != nil {
					panic(err)
				}
			}); allocs != 0 {
				t.Fatalf("allocations per byte = %v, want 0", allocs)
			}
		})
	}
}
