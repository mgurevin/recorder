package recorder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type samplingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f samplingRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type samplingReferenceStore struct{}

func (samplingReferenceStore) NewWriter(context.Context, BodyMetadata) (BodyWriter, error) {
	return &samplingReferenceWriter{}, nil
}

type samplingReferenceWriter struct {
	strings.Builder
	committed bool
}

func (w *samplingReferenceWriter) Commit() error {
	w.committed = true

	return nil
}

func (w *samplingReferenceWriter) Abort() error { return nil }

func (w *samplingReferenceWriter) Bytes() ([]byte, error) {
	return []byte(w.String()), nil
}

func (w *samplingReferenceWriter) Ref() string {
	if w.committed {
		return "custom:asset"
	}

	return ""
}

func TestHeadSampleDropUsesUninstrumentedFastPath(t *testing.T) {
	t.Parallel()

	request, err := http.NewRequest(http.MethodPost, "https://example.test/drop", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json; charset=utf-8")

	var baseRequest *http.Request

	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		baseRequest = req

		return &http.Response{
			StatusCode:    http.StatusNoContent,
			Status:        "204 No Content",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}, nil
	})

	rec := NewMemoryRecorder()
	transport := NewTransport(base, rec, configWith(withHeadSamplingPolicy(HeadSamplingPolicy(
		func(_ context.Context, meta HeadSamplingMeta) HeadSamplingDecision {
			if meta.MIMEType != "application/json" {
				t.Errorf("MIMEType = %q", meta.MIMEType)
			}

			return HeadSampleDrop
		}))))

	if transport.red != nil || transport.effectiveBase != nil {
		t.Fatal("recording pipeline initialized eagerly")
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if response.Body != http.NoBody || baseRequest != request {
		t.Fatal("drop path wrapped the response body or cloned the request")
	}

	if transport.red != nil || transport.effectiveBase != nil {
		t.Fatal("drop path initialized recording instrumentation")
	}

	if len(rec.Entries()) != 0 {
		t.Fatal("drop path recorded an entry")
	}

	if stats := transport.SamplingStats(); stats.HeadDropped != 1 || stats.Retained != 0 {
		t.Fatalf("sampling stats = %+v", stats)
	}
}

func TestHeadSampleMetadataOnlyCannotBeRelaxedByBodyPolicy(t *testing.T) {
	t.Parallel()

	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				return nil, err
			}
		}

		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"session=secret"}},
			Body:          io.NopCloser(strings.NewReader(`{"password":"secret"}`)),
			ContentLength: int64(len(`{"password":"secret"}`)),
			Request:       req,
		}, nil
	})

	rec := NewMemoryRecorder()
	transport := NewTransport(base, rec, configWith(withCaptureRequestBody(true),
		withCaptureResponseBody(true),
		withEmbedBodies(true),
		withHashBodies(true, "sha256"),
		withCaptureRawTrace(true),
		withHeadSamplingPolicy(HeadSamplingPolicy(func(context.Context, HeadSamplingMeta) HeadSamplingDecision {
			return HeadSampleMetadataOnly
		})),
		withBodyCapturePolicy(BodyCapturePolicy(func(_ context.Context, _ BodyCaptureMeta, decision BodyCaptureDecision) (BodyCaptureDecision, error) {
			decision.Capture = true
			decision.Embed = true
			decision.Hash = true

			return decision, nil
		}))),
	)

	request, err := http.NewRequest(http.MethodPost, "https://example.test/items?token=secret", strings.NewReader("request-body"))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Cookie", "session=secret")

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}

	entries := rec.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}

	entry := entries[0]
	if entry.Request.BodySize != int64(len("request-body")) || entry.Response.BodySize != int64(len(`{"password":"secret"}`)) {
		t.Fatalf("body sizes request=%d response=%d", entry.Request.BodySize, entry.Response.BodySize)
	}

	if entry.Request.PostData != nil || entry.Response.Content.Text != "" ||
		len(entry.Request.Headers) != 0 || len(entry.Response.Headers) != 0 ||
		len(entry.Request.Cookies) != 0 || len(entry.Response.Cookies) != 0 ||
		len(entry.Request.QueryString) != 0 || len(entry.Recorder.RawTrace) != 0 {
		t.Fatalf("metadata-only entry retained optional content: %+v", entry)
	}

	if entry.Recorder.RequestBody == nil || entry.Recorder.ResponseBody == nil ||
		entry.Recorder.RequestBody.TotalBytes != int64(len("request-body")) ||
		entry.Recorder.ResponseBody.TotalBytes != int64(len(`{"password":"secret"}`)) {
		t.Fatalf("body accounting missing: request=%+v response=%+v", entry.Recorder.RequestBody, entry.Recorder.ResponseBody)
	}
}

func TestRetentionDiscardReleasesAssetsAfterCompletionCallback(t *testing.T) {
	t.Parallel()

	store := mustFileBodyStore(t, t.TempDir())
	rec := NewMemoryRecorder()
	callbackRead := atomic.Bool{}

	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Content-Type": {"text/plain"}},
			Body:          io.NopCloser(strings.NewReader("retained-until-callback")),
			ContentLength: int64(len("retained-until-callback")),
			Request:       req,
		}, nil
	})

	transport := NewTransport(base, rec, configWith(withCaptureResponseBody(true),
		withEmbedBodies(false),
		withBodyStore(store),
		withRetentionPolicy(RetentionPolicy(func(context.Context, *Entry) RetentionDecision {
			return DiscardEntry
		})),
		withOnEntryCompleted(func(_ context.Context, entry *Entry) {
			if got := string(readBodyAsset(t, store, entry.Recorder.ResponseBody.Store)); got != "retained-until-callback" {
				t.Errorf("callback body = %q", got)
			}

			callbackRead.Store(true)
		})),
	)

	request, err := http.NewRequest(http.MethodGet, "https://example.test/retention", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}

	if !callbackRead.Load() || len(rec.Entries()) != 0 {
		t.Fatalf("callback=%v entries=%d", callbackRead.Load(), len(rec.Entries()))
	}

	if stats := store.Stats(); stats.CommittedFiles != 0 || stats.ReleasedTotal != 1 {
		t.Fatalf("store stats = %+v", stats)
	}

	if stats := transport.SamplingStats(); stats.Discarded != 1 || stats.AssetReleaseFailures != 0 {
		t.Fatalf("sampling stats = %+v", stats)
	}
}

func TestRetentionFailsOpenWhenStoreCannotReleaseReferencedAssets(t *testing.T) {
	t.Parallel()

	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("asset")),
			ContentLength: 5,
			Request:       req,
		}, nil
	})

	callbackFinished := atomic.Bool{}
	recorderCalledAfterCallback := atomic.Bool{}
	rec := RecorderFunc(func(*Entry) error {
		recorderCalledAfterCallback.Store(callbackFinished.Load())

		return nil
	})

	transport := NewTransport(base, rec, configWith(withCaptureResponseBody(true),
		withEmbedBodies(false),
		withBodyStore(samplingReferenceStore{}),
		withOnEntryCompleted(func(context.Context, *Entry) { callbackFinished.Store(true) }),
		withRetentionPolicy(RetentionPolicy(func(context.Context, *Entry) RetentionDecision {
			return DiscardEntry
		}))),
	)

	request, err := http.NewRequest(http.MethodGet, "https://example.test/fail-open", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}

	if !recorderCalledAfterCallback.Load() {
		t.Fatal("entry was not retained after callback when asset release was unsupported")
	}

	stats := transport.SamplingStats()
	if stats.Retained != 1 || stats.Discarded != 0 || stats.AssetReleaseFailures != 1 {
		t.Fatalf("sampling stats = %+v", stats)
	}
}

func TestSamplingPoliciesFailOpen(t *testing.T) {
	t.Parallel()

	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusNoContent,
			Status:        "204 No Content",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}, nil
	})

	rec := NewMemoryRecorder()
	transport := NewTransport(base, rec, configWith(withHeadSamplingPolicy(HeadSamplingPolicy(func(context.Context, HeadSamplingMeta) HeadSamplingDecision {
		panic("head boom")
	})),
		withRetentionPolicy(RetentionPolicy(func(context.Context, *Entry) RetentionDecision {
			panic("tail boom")
		}))),
	)

	request, err := http.NewRequest(http.MethodGet, "https://example.test/panic", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if len(rec.Entries()) != 1 {
		t.Fatal("panic paths did not retain the entry")
	}

	stats := transport.SamplingStats()
	if stats.HeadPanics != 1 || stats.HeadFull != 1 || stats.RetentionPanics != 1 || stats.Retained != 1 {
		t.Fatalf("sampling stats = %+v", stats)
	}
}

func TestRateHeadSamplerIsDeterministicAndDistributed(t *testing.T) {
	t.Parallel()

	policy, err := NewRateHeadSampler(0.1, HeadSampleFull, HeadSampleDrop)
	if err != nil {
		t.Fatalf("NewRateHeadSampler: %v", err)
	}

	const total = 10_000

	sampled := 0

	for i := range total {
		key := fmt.Sprintf("trace-%d", i)
		meta := HeadSamplingMeta{TraceID: key}

		first := policy(context.Background(), meta)
		if second := policy(context.Background(), meta); second != first {
			t.Fatalf("non-deterministic decision for %q", key)
		}

		if first == HeadSampleFull {
			sampled++
		}
	}

	if sampled < 900 || sampled > 1100 {
		t.Fatalf("sampled %d/%d, want 10%% ±1%%", sampled, total)
	}
}

func TestRateHeadSamplerDistributionAcrossCommonKeyShapes(t *testing.T) {
	t.Parallel()

	keyFormats := []struct {
		name   string
		format string
	}{
		{name: "decimal", format: "%d"},
		{name: "client-prefix", format: "client-%d"},
		{name: "user-prefix", format: "user:%d"},
		{name: "fixed-width-hex", format: "%032x"},
	}

	for _, rate := range []struct {
		fraction float64
		total    int
	}{
		{fraction: 0.5, total: 20_000},
		{fraction: 0.1, total: 20_000},
		{fraction: 0.01, total: 20_000},
		{fraction: 0.001, total: 200_000},
	} {
		policy, err := NewRateHeadSampler(rate.fraction, HeadSampleFull, HeadSampleDrop)
		if err != nil {
			t.Fatalf("NewRateHeadSampler(%v): %v", rate.fraction, err)
		}

		for _, keyFormat := range keyFormats {
			t.Run(fmt.Sprintf("rate-%g/%s", rate.fraction, keyFormat.name), func(t *testing.T) {
				sampled := 0

				for i := range rate.total {
					key := fmt.Sprintf(keyFormat.format, i)

					decision := policy(context.Background(), HeadSamplingMeta{SamplingKey: key})
					if decision == HeadSampleFull {
						sampled++
					}
				}

				expected := float64(rate.total) * rate.fraction
				minimum := int(expected * 0.75)
				maximum := int(expected * 1.25)

				if sampled < minimum || sampled > maximum {
					t.Fatalf("sampled %d/%d, want %g within ±25%% (%d..%d)",
						sampled, rate.total, rate.fraction, minimum, maximum)
				}
			})
		}
	}
}

func TestRateHeadSamplerBoundariesAndValidation(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		fraction float64
		want     HeadSamplingDecision
	}{
		{fraction: 0, want: HeadSampleDrop},
		{fraction: 1, want: HeadSampleFull},
	} {
		policy, err := NewRateHeadSampler(test.fraction, HeadSampleFull, HeadSampleDrop)
		if err != nil {
			t.Fatalf("NewRateHeadSampler(%v): %v", test.fraction, err)
		}

		for _, meta := range []HeadSamplingMeta{{TraceID: "trace"}, {}} {
			if got := policy(context.Background(), meta); got != test.want {
				t.Fatalf("fraction %v decision = %v, want %v", test.fraction, got, test.want)
			}
		}
	}

	for _, fraction := range []float64{-0.1, 1.1} {
		if _, err := NewRateHeadSampler(fraction, HeadSampleFull, HeadSampleDrop); err == nil {
			t.Fatalf("NewRateHeadSampler(%v) succeeded", fraction)
		}
	}

	if _, err := NewRateHeadSampler(0.5, HeadSamplingDecision(99), HeadSampleDrop); err == nil {
		t.Fatal("NewRateHeadSampler accepted invalid decision")
	}
}

func TestSamplingPoliciesConcurrent(t *testing.T) {
	t.Parallel()

	base := samplingRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusNoContent,
			Status:        "204 No Content",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        make(http.Header),
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}, nil
	})

	transport := NewTransport(base, RecorderFunc(func(*Entry) error { return nil }), configWith(withHeadSamplingPolicy(HeadSamplingPolicy(func(_ context.Context, meta HeadSamplingMeta) HeadSamplingDecision {
		if stableSamplingHash(meta.SamplingKey)&1 == 0 {
			return HeadSampleFull
		}

		return HeadSampleDrop
	})),
		withRetentionPolicy(RetentionPolicy(func(context.Context, *Entry) RetentionDecision {
			return RetainEntry
		}))),
	)

	const requests = 1000

	var wg sync.WaitGroup
	wg.Add(requests)

	for i := range requests {
		go func() {
			defer wg.Done()

			ctx := WithSamplingKey(context.Background(), fmt.Sprintf("request-%d", i))

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/concurrent", nil)
			if err != nil {
				t.Errorf("NewRequest: %v", err)

				return
			}

			if _, err := transport.RoundTrip(request); err != nil {
				t.Errorf("RoundTrip: %v", err)
			}
		}()
	}

	wg.Wait()

	stats := transport.SamplingStats()
	if stats.HeadFull+stats.HeadDropped != requests || stats.Retained != stats.HeadFull {
		t.Fatalf("sampling stats = %+v", stats)
	}
}

func TestHeadSamplingKeepsRedirectChainConsistentWithTraceContext(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusFound) })
	mux.HandleFunc("/c", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	for _, decision := range []HeadSamplingDecision{HeadSampleFull, HeadSampleDrop} {
		t.Run(fmt.Sprint(decision), func(t *testing.T) {
			rec := NewMemoryRecorder()
			transport := NewTransport(server.Client().Transport, rec, configWith(withHeadSamplingPolicy(HeadSamplingPolicy(func(_ context.Context, meta HeadSamplingMeta) HeadSamplingDecision {
				if meta.TraceID != "chain" {
					t.Errorf("TraceID = %q", meta.TraceID)
				}

				return decision
			}))))
			client := server.Client()
			client.Transport = transport

			ctx := WithTraceID(context.Background(), "chain")

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/a", nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}

			if err := response.Body.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			want := 3
			if decision == HeadSampleDrop {
				want = 0
			}

			if got := len(rec.Entries()); got != want {
				t.Fatalf("entries = %d, want %d", got, want)
			}
		})
	}
}
