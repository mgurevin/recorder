package recorder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func traceEntry(traceID string, startOffsetMS int) *Entry {
	return &Entry{
		StartedDateTime: traceBase.Add(time.Duration(startOffsetMS) * time.Millisecond).UTC().Format(harTimeFormat),
		TraceID:         traceID,
		Request: &Request{
			Method: "GET", URL: "http://x/", HTTPVersion: "HTTP/1.1",
			Cookies: []Cookie{}, Headers: []NameValuePair{}, QueryString: []NameValuePair{},
			HeadersSize: -1, BodySize: 0,
		},
		Response: &Response{
			Status: 200, StatusText: "OK", HTTPVersion: "HTTP/1.1",
			Cookies: []Cookie{}, Headers: []NameValuePair{},
			Content:     &Content{Size: 0, MimeType: "x-unknown"},
			HeadersSize: -1, BodySize: -1,
		},
		Cache:   &Cache{},
		Timings: &Timings{Blocked: -1, DNS: -1, Connect: -1, Send: -1, Wait: -1, Receive: -1, SSL: -1},
	}
}

func TestMemoryRecorderTraceQueries(t *testing.T) {
	rec := NewMemoryRecorder()
	// Interleave two traces to verify order preservation and selective removal.
	rec.Record(traceEntry("a", 0))
	rec.Record(traceEntry("b", 1))
	rec.Record(traceEntry("a", 2))
	rec.Record(traceEntry("b", 3))
	rec.Record(traceEntry("b", 4))

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
	rec := NewMemoryRecorder()

	const traces, perTrace = 8, 25

	var wg sync.WaitGroup
	for i := 0; i < traces; i++ {
		wg.Add(1)

		go func(id string) {
			defer wg.Done()

			for j := 0; j < perTrace; j++ {
				rec.Record(traceEntry(id, j))
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

// TestHARFileRecorderTraceStore verifies the file recorder's TraceStore
// implementation: taken traces are excluded from subsequent flushes.
func TestHARFileRecorderTraceStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.har")
	rec := NewHARFileRecorder(path)
	rec.Record(traceEntry("call-1", 0))
	rec.Record(traceEntry("call-2", 1))
	rec.Record(traceEntry("call-1", 2))

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

	doc := validateHAR(t, data)

	entries := doc["log"].(map[string]any)["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("flushed entries = %d, want only call-2", len(entries))
	}

	if id := entries[0].(map[string]any)["_traceId"]; id != "call-2" {
		t.Errorf("remaining trace = %v", id)
	}
}

// TestTraceStoreCapabilityDiscovery shows the intended usage pattern: a
// caller holding only the Recorder interface upgrades via type assertion.
func TestTraceStoreCapabilityDiscovery(t *testing.T) {
	for _, rec := range []Recorder{NewMemoryRecorder(), NewHARFileRecorder("unused")} {
		if _, ok := rec.(TraceStore); !ok {
			t.Errorf("%T must implement TraceStore", rec)
		}
	}

	var stream Recorder = NewJSONStreamRecorder(io.Discard)
	if _, ok := stream.(TraceStore); ok {
		t.Errorf("JSONStreamRecorder must not claim TraceStore")
	}

	var cb Recorder = RecorderFunc(func(*Entry) {})
	if _, ok := cb.(TraceStore); ok {
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
		w.Write([]byte("done"))
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

	ctx, traceID := TraceContext(context.Background())
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

	har := NewHAR(taken)
	if len(har.Log.Entries) != 2 {
		t.Errorf("HAR entries = %d", len(har.Log.Entries))
	}
	// The unrelated exchange must survive untouched.
	if rec.Len() != 1 {
		t.Fatalf("remaining entries = %d, want 1", rec.Len())
	}

	if rec.Entries()[0].TraceID == traceID {
		t.Errorf("wrong entry removed")
	}
}
