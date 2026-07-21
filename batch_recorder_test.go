package recorder

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestBuiltInbatchRecordersPreserveOrder(t *testing.T) {
	entries := []*Entry{
		traceEntry("a", 0),
		traceEntry("b", 1),
		traceEntry("a", 2),
	}

	memory, err := NewMemoryRecorderWithCapacity(2)
	if err != nil {
		t.Fatalf("NewMemoryRecorderWithCapacity: %v", err)
	}

	_ = memory.RecordBatch(entries)

	if got := traceIDs(memory.Entries()); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("memory entries = %v", got)
	}

	file := NewHARFileRecorder("unused")
	_ = file.RecordBatch(entries)

	if got := traceIDs(file.EntriesByTrace("a")); len(got) != 2 || got[0] != "a" || got[1] != "a" {
		t.Errorf("HAR file entries = %v", got)
	}
}

func TestJSONStreamRecorderReturnsWriteError(t *testing.T) {
	want := errors.New("write failed")
	recorder := NewJSONStreamRecorder(errorWriter{err: want})

	if err := recorder.Record(&Entry{}); !errors.Is(err, want) {
		t.Fatalf("Record error = %v, want %v", err, want)
	}
}

func TestJSONStreamRecorderBatchUsesOneWrite(t *testing.T) {
	writer := &countingWriter{}
	recorder := NewJSONStreamRecorder(writer)
	entries := []*Entry{traceEntry("a", 0), traceEntry("b", 1)}

	if err := recorder.RecordBatch(entries); err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}

	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}

	decoder := json.NewDecoder(bytes.NewReader(writer.Bytes()))

	for i, want := range []string{"a", "b"} {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode entry %d: %v", i, err)
		}

		if entry.Recorder.TraceID != want {
			t.Errorf("entry %d trace = %q, want %q", i, entry.Recorder.TraceID, want)
		}
	}
}

type countingWriter struct {
	bytes.Buffer
	writes int
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++

	return w.Buffer.Write(p)
}
