package recorder_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	recorder "github.com/mgurevin/recorder"
)

var memoryTraceBase = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

func traceEntry(traceID string, startOffsetMS int) *recorder.Entry {
	return &recorder.Entry{
		StartedDateTime: memoryTraceBase.Add(time.Duration(startOffsetMS) * time.Millisecond).Format(time.RFC3339Nano),
		Recorder:        &recorder.RecorderEntryExtension{SchemaVersion: recorder.RecorderExtensionVersion, TraceID: traceID},
		Request: &recorder.Request{
			Method: "GET", URL: "http://x/", HTTPVersion: "HTTP/1.1",
			Cookies: []recorder.Cookie{}, Headers: []recorder.NameValuePair{}, QueryString: []recorder.NameValuePair{},
			HeadersSize: -1, BodySize: 0,
		},
		Response: &recorder.Response{
			Status: 200, StatusText: "OK", HTTPVersion: "HTTP/1.1",
			Cookies: []recorder.Cookie{}, Headers: []recorder.NameValuePair{},
			Content:     &recorder.Content{Size: 0, MimeType: "x-unknown"},
			HeadersSize: -1, BodySize: -1,
		},
		Cache:   &recorder.Cache{},
		Timings: &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, Send: -1, Wait: -1, Receive: -1, SSL: -1},
	}
}

func TestMemoryRecorderDefaultCapacityEvictsOldest(t *testing.T) {
	rec := recorder.NewMemoryRecorder()
	for i := 0; i < recorder.DefaultMemoryRecorderCapacity+3; i++ {
		_ = rec.Record(traceEntry(fmt.Sprintf("trace-%d", i), i))
	}

	entries, stats := rec.Snapshot()
	if len(entries) != recorder.DefaultMemoryRecorderCapacity {
		t.Fatalf("entries = %d, want %d", len(entries), recorder.DefaultMemoryRecorderCapacity)
	}

	if entries[0].Recorder.TraceID != "trace-3" || entries[len(entries)-1].Recorder.TraceID != "trace-1026" {
		t.Errorf("retained range = %q..%q", entries[0].Recorder.TraceID, entries[len(entries)-1].Recorder.TraceID)
	}

	wantStats := (recorder.MemoryRecorderStats{Capacity: recorder.DefaultMemoryRecorderCapacity, Retained: recorder.DefaultMemoryRecorderCapacity, Evicted: 3})
	if stats != wantStats {
		t.Errorf("stats = %+v, want %+v", stats, wantStats)
	}
}

func TestMemoryRecorderCustomCapacityWrapAndTraceOperations(t *testing.T) {
	rec, err := recorder.NewMemoryRecorderWithCapacity(4)
	if err != nil {
		t.Fatalf("recorder.NewMemoryRecorderWithCapacity: %v", err)
	}

	for i, traceID := range []string{"old", "a", "b", "a", "c", "b"} {
		_ = rec.Record(traceEntry(traceID, i))
	}

	if got := traceIDs(rec.Entries()); fmt.Sprint(got) != "[b a c b]" {
		t.Fatalf("entries after wrap = %v", got)
	}

	if got := traceIDs(rec.EntriesByTrace("b")); fmt.Sprint(got) != "[b b]" {
		t.Errorf("EntriesByTrace(b) = %v", got)
	}

	if got := traceIDs(rec.TakeTrace("a")); fmt.Sprint(got) != "[a]" {
		t.Errorf("TakeTrace(a) = %v", got)
	}

	_ = rec.Record(traceEntry("d", 6))
	_ = rec.Record(traceEntry("e", 7))

	if got := traceIDs(rec.Entries()); fmt.Sprint(got) != "[c b d e]" {
		t.Fatalf("entries after compaction and wrap = %v", got)
	}

	if removed := rec.RemoveTrace("b"); removed != 1 {
		t.Errorf("RemoveTrace(b) = %d, want 1", removed)
	}

	if got := traceIDs(rec.Entries()); fmt.Sprint(got) != "[c d e]" {
		t.Errorf("entries after RemoveTrace = %v", got)
	}
}

func TestMemoryRecorderCapacityValidationAndReset(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		if _, err := recorder.NewMemoryRecorderWithCapacity(capacity); err == nil {
			t.Errorf("capacity %d: expected error", capacity)
		}
	}

	rec, err := recorder.NewMemoryRecorderWithCapacity(2)
	if err != nil {
		t.Fatalf("recorder.NewMemoryRecorderWithCapacity: %v", err)
	}

	_ = rec.Record(traceEntry("a", 0))
	_ = rec.Record(traceEntry("b", 1))
	_ = rec.Record(traceEntry("c", 2))
	rec.Reset()

	entries, stats := rec.Snapshot()
	if len(entries) != 0 || stats.Retained != 0 || stats.Evicted != 1 || stats.Capacity != 2 {
		t.Errorf("after Reset: entries=%d stats=%+v", len(entries), stats)
	}

	_ = rec.Record(traceEntry("d", 3))

	if got := traceIDs(rec.Entries()); fmt.Sprint(got) != "[d]" {
		t.Errorf("entries after Reset and Record = %v", got)
	}
}

func traceIDs(entries []*recorder.Entry) []string {
	ids := make([]string, len(entries))
	for i, entry := range entries {
		ids[i] = entry.Recorder.TraceID
	}

	return ids
}

func TestMemoryRecorderTraceQueries(t *testing.T) {
	rec := recorder.NewMemoryRecorder()
	// Interleave two traces to verify order preservation and selective removal.
	_ = rec.Record(traceEntry("a", 0))
	_ = rec.Record(traceEntry("b", 1))
	_ = rec.Record(traceEntry("a", 2))
	_ = rec.Record(traceEntry("b", 3))
	_ = rec.Record(traceEntry("b", 4))

	got := rec.EntriesByTrace("a")
	if len(got) != 2 {
		t.Fatalf("EntriesByTrace(a) = %d entries", len(got))
	}

	if rec.Len() != 5 {
		t.Fatalf("EntriesByTrace must not remove; len = %d", rec.Len())
	}

	har := rec.HARForTrace("b")
	if len(har.Log.Entries) != 3 {
		t.Fatalf("HARForTrace(b) = %d entries", len(har.Log.Entries))
	}

	for i := 1; i < len(har.Log.Entries); i++ {
		if har.Log.Entries[i].StartTime().Before(har.Log.Entries[i-1].StartTime()) {
			t.Errorf("HARForTrace entries not sorted by start time")
		}
	}

	if n := rec.RemoveTrace("a"); n != 2 {
		t.Fatalf("RemoveTrace(a) = %d, want 2", n)
	}

	if rec.Len() != 3 {
		t.Fatalf("len after remove = %d", rec.Len())
	}

	if n := rec.RemoveTrace("a"); n != 0 {
		t.Errorf("second RemoveTrace(a) = %d, want 0", n)
	}

	taken := rec.TakeTrace("b")
	if len(taken) != 3 {
		t.Fatalf("TakeTrace(b) = %d entries", len(taken))
	}

	if rec.Len() != 0 {
		t.Fatalf("len after take = %d", rec.Len())
	}

	if again := rec.TakeTrace("b"); len(again) != 0 {
		t.Errorf("second TakeTrace(b) = %d entries", len(again))
	}

	if missing := rec.TakeTrace("nope"); len(missing) != 0 {
		t.Errorf("TakeTrace(nope) = %d entries", len(missing))
	}
}

func TestMemoryRecorderTakeTraceConcurrent(t *testing.T) {
	rec := recorder.NewMemoryRecorder()

	const traces, perTrace = 8, 25

	var wg sync.WaitGroup
	for i := 0; i < traces; i++ {
		wg.Add(1)

		go func(id string) {
			defer wg.Done()

			for j := 0; j < perTrace; j++ {
				_ = rec.Record(traceEntry(id, j))
			}
		}(fmt.Sprintf("trace-%d", i))
	}

	// Concurrently drain traces while recording is still in progress.
	results := make(chan int, traces)
	for i := 0; i < traces; i++ {
		go func(id string) {
			total := 0
			for {
				total += len(rec.TakeTrace(id))
				if total == perTrace {
					results <- total
					return
				}
			}
		}(fmt.Sprintf("trace-%d", i))
	}

	wg.Wait()

	for i := 0; i < traces; i++ {
		if got := <-results; got != perTrace {
			t.Fatalf("trace drained %d entries, want %d", got, perTrace)
		}
	}

	if rec.Len() != 0 {
		t.Fatalf("leftover entries = %d", rec.Len())
	}
}

// TestHARFileRecorderTraceStore verifies the file recorder's recorder.TraceStore
// implementation: taken traces are excluded from subsequent flushes.
func TestHARFileRecorderTraceStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.har")
	rec := recorder.NewHARFileRecorder(path)
	_ = rec.Record(traceEntry("call-1", 0))
	_ = rec.Record(traceEntry("call-2", 1))
	_ = rec.Record(traceEntry("call-1", 2))

	if got := rec.EntriesByTrace("call-1"); len(got) != 2 {
		t.Fatalf("EntriesByTrace = %d entries", len(got))
	}

	taken := rec.TakeTrace("call-1")
	if len(taken) != 2 {
		t.Fatalf("TakeTrace = %d entries", len(taken))
	}

	if n := rec.RemoveTrace("call-1"); n != 0 {
		t.Errorf("RemoveTrace after take = %d", n)
	}

	if err := rec.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	doc := decodeHAR(t, data)

	entries := doc.Log.Entries
	if len(entries) != 1 {
		t.Fatalf("flushed entries = %d, want only call-2", len(entries))
	}

	if id := entries[0].Recorder.TraceID; id != "call-2" {
		t.Errorf("remaining trace = %v", id)
	}
}

func decodeHAR(t *testing.T, data []byte) *recorder.HAR {
	t.Helper()

	var document recorder.HAR
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode HAR: %v", err)
	}

	return &document
}

// TestTraceStoreCapabilityDiscovery shows the intended usage pattern: a
// caller holding only the Recorder interface upgrades via type assertion.
func TestTraceStoreCapabilityDiscovery(t *testing.T) {
	for _, rec := range []recorder.Recorder{recorder.NewMemoryRecorder(), recorder.NewHARFileRecorder("unused")} {
		if _, ok := rec.(recorder.TraceStore); !ok {
			t.Errorf("%T must implement TraceStore", rec)
		}
	}

	var stream recorder.Recorder = recorder.NewJSONStreamRecorder(io.Discard)
	if _, ok := stream.(recorder.TraceStore); ok {
		t.Errorf("JSONStreamRecorder must not claim TraceStore")
	}

	var cb recorder.Recorder = recorder.RecorderFunc(func(*recorder.Entry) error { return nil })
	if _, ok := cb.(recorder.TraceStore); ok {
		t.Errorf("RecorderFunc must not claim TraceStore")
	}
}

// TestTakeTraceEndToEnd exercises the intended workflow: one logical call
// (with redirects) on a shared client, exported and dropped by trace ID.
func TestTakeTraceEndToEnd(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		testWrite(w, []byte("done"))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client, rec := newRecordedClient(ts)

	// Unrelated background traffic on the same client.
	resp, err := client.Get(ts.URL + "/final")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	mustReadAll(t, resp.Body)

	ctx, traceID := recorder.TraceContext(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/start", nil)

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}

	mustReadAll(t, resp.Body)

	taken := rec.TakeTrace(traceID)
	if len(taken) != 2 {
		t.Fatalf("TakeTrace = %d entries, want redirect + final", len(taken))
	}

	if taken[0].Response.Status != 302 || taken[1].Response.Status != 200 {
		t.Errorf("statuses = %d, %d", taken[0].Response.Status, taken[1].Response.Status)
	}

	har := recorder.NewHAR(taken)
	if len(har.Log.Entries) != 2 {
		t.Errorf("HAR entries = %d", len(har.Log.Entries))
	}
	// The unrelated exchange must survive untouched.
	if rec.Len() != 1 {
		t.Fatalf("remaining entries = %d, want 1", rec.Len())
	}

	if rec.Entries()[0].Recorder.TraceID == traceID {
		t.Errorf("wrong entry removed")
	}
}
