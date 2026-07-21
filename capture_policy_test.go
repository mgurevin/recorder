package recorder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBodyCapturePolicyControlsEachDirection(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []BodyCaptureMeta
	)

	policy := BodyCapturePolicy(func(_ context.Context, meta BodyCaptureMeta, defaults BodyCaptureDecision) (BodyCaptureDecision, error) {
		mu.Lock()

		seen = append(seen, meta)
		mu.Unlock()

		if meta.Direction == RequestBody {
			return BodyCaptureDecision{}, nil
		}

		defaults.Capture = true
		defaults.Embed = false
		defaults.Hash = true
		defaults.MaxBodyBytes = 4

		return defaults, nil
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testCopy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		testWriteString(w, "response-body")
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts, withBodyCapturePolicy(policy))

	resp, err := client.Post(ts.URL+"/payments", "text/plain", bytes.NewBufferString("request-body"))
	if err != nil {
		t.Fatal(err)
	}

	callerBody := mustReadAll(t, resp.Body)

	e := singleEntry(t, rec)
	if e.Recorder.RequestBody.CapturedBytes != 0 || e.Recorder.RequestBody.Hash != "" || e.Request.PostData != nil {
		t.Fatalf("request policy not applied: body=%+v postData=%+v", e.Recorder.RequestBody, e.Request.PostData)
	}

	if e.Recorder.ResponseBody.CapturedBytes != 4 || !e.Recorder.ResponseBody.Truncated || e.Recorder.ResponseBody.Hash != sha256Hex(callerBody) {
		t.Fatalf("response policy not applied: %+v", e.Recorder.ResponseBody)
	}

	if e.Response.Content.Text != "" {
		t.Fatalf("response was embedded: %q", e.Response.Content.Text)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != 2 || seen[0].Direction != RequestBody || seen[1].Direction != ResponseBody {
		t.Fatalf("policy calls = %+v", seen)
	}

	if seen[0].Path != "/payments" || seen[1].StatusCode != http.StatusCreated {
		t.Fatalf("policy metadata = %+v", seen)
	}
}

func TestBodyCapturePolicyCanOverrideBodyRedactor(t *testing.T) {
	custom := &markerBodyRedactor{marker: "policy-redacted"}
	policy := BodyCapturePolicy(func(_ context.Context, meta BodyCaptureMeta, defaults BodyCaptureDecision) (BodyCaptureDecision, error) {
		if meta.Direction == ResponseBody && meta.StatusCode == http.StatusBadRequest {
			defaults.RedactorOverride = custom
		}

		return defaults, nil
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		testWriteString(w, `{"password":"wire-secret"}`)
	}))
	defer ts.Close()

	client, rec := newRecordedClient(ts, withBodyCapturePolicy(policy), withRedaction(RedactionConfig{Common: RedactionRules{JSONFields: []string{"password"}}}))

	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(mustReadAll(t, resp.Body)); got != `{"password":"wire-secret"}` {
		t.Fatalf("caller body changed: %q", got)
	}

	e := singleEntry(t, rec)
	if e.Response.Content.Text != custom.marker || custom.opens.Load() != 1 || custom.closes.Load() != 1 {
		t.Fatalf("policy redactor result=%q opens=%d closes=%d", e.Response.Content.Text, custom.opens.Load(), custom.closes.Load())
	}

	if e.Recorder.Redaction == nil || e.Recorder.Redaction.Response == nil || e.Recorder.Redaction.Response.Body == nil ||
		e.Recorder.Redaction.Response.Body.Kind != "custom" || e.Recorder.Redaction.Response.Body.Outcome != "unchanged" ||
		e.Recorder.Redaction.Response.Body.Replacements == nil || *e.Recorder.Redaction.Response.Body.Replacements != 0 {
		t.Fatalf("redaction audit = %+v", e.Recorder.Redaction)
	}
}

func TestBodyCapturePolicyRunsForEveryRedirectHop(t *testing.T) {
	var mu sync.Mutex

	seen := map[BodyDirection]map[int]int{RequestBody: {}, ResponseBody: {}}
	policy := BodyCapturePolicy(func(_ context.Context, meta BodyCaptureMeta, defaults BodyCaptureDecision) (BodyCaptureDecision, error) {
		mu.Lock()
		seen[meta.Direction][meta.RedirectIndex]++
		mu.Unlock()

		return defaults, nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) { testWriteString(w, "done") })

	ts := httptest.NewServer(mux)
	defer ts.Close()

	client, _ := newRecordedClient(ts, withBodyCapturePolicy(policy))
	req, _ := http.NewRequestWithContext(WithTraceID(context.Background(), "policy-chain"), http.MethodGet, ts.URL+"/a", nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	mustReadAll(t, resp.Body)
	mu.Lock()
	defer mu.Unlock()

	for _, direction := range []BodyDirection{RequestBody, ResponseBody} {
		if seen[direction][0] != 1 || seen[direction][1] != 1 {
			t.Fatalf("%s policy calls = %+v", direction, seen[direction])
		}
	}
}

func TestBodyCapturePolicyFailureIsFailClosedAndDoesNotAffectHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy BodyCapturePolicy
	}{
		{"error", BodyCapturePolicy(func(context.Context, BodyCaptureMeta, BodyCaptureDecision) (BodyCaptureDecision, error) {
			return BodyCaptureDecision{}, errors.New("policy failed")
		})},
		{"panic", BodyCapturePolicy(func(context.Context, BodyCaptureMeta, BodyCaptureDecision) (BodyCaptureDecision, error) {
			panic("policy panic")
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var internal atomic.Int64

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				testWriteString(w, "caller-visible")
			}))
			defer ts.Close()

			client, rec := newRecordedClient(ts, withBodyCapturePolicy(tc.policy), withOnInternalError(func(error) { internal.Add(1) }))

			resp, err := client.Get(ts.URL)
			if err != nil {
				t.Fatalf("HTTP changed: %v", err)
			}

			if got := string(mustReadAll(t, resp.Body)); got != "caller-visible" {
				t.Fatalf("caller body = %q", got)
			}

			e := singleEntry(t, rec)
			if e.Recorder.ResponseBody.CapturedBytes != 0 || e.Recorder.ResponseBody.Hash != "" || e.Response.Content.Text != "" {
				t.Fatalf("policy did not fail closed: body=%+v content=%+v", e.Recorder.ResponseBody, e.Response.Content)
			}

			if internal.Load() < 2 {
				t.Fatalf("internal errors = %d, want request and response failures", internal.Load())
			}
		})
	}
}

func TestBodyCapturePolicyIsConcurrentSafe(t *testing.T) {
	var calls atomic.Int64

	policy := BodyCapturePolicy(func(_ context.Context, _ BodyCaptureMeta, defaults BodyCaptureDecision) (BodyCaptureDecision, error) {
		calls.Add(1)
		return defaults, nil
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { testWriteString(w, "ok") }))
	defer ts.Close()

	client, rec := newRecordedClient(ts, withBodyCapturePolicy(policy))

	const requests = 32

	var wg sync.WaitGroup

	errs := make(chan error, requests)
	for range requests {
		wg.Add(1)

		go func() {
			defer wg.Done()

			resp, err := client.Get(ts.URL)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)

				closeErr := resp.Body.Close()
				if err == nil {
					err = closeErr
				}
			}

			errs <- err
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	if calls.Load() != requests*2 {
		t.Fatalf("policy calls = %d, want %d", calls.Load(), requests*2)
	}

	if len(rec.Entries()) != requests {
		t.Fatalf("entries = %d, want %d", len(rec.Entries()), requests)
	}
}
