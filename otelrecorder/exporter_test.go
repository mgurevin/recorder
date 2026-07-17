package otelrecorder

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	recorder "github.com/mgurevin/recorder"
)

// testSetup wires an exporter to in-memory OTel SDKs.
func testSetup(t *testing.T, opts ...Option) (*Exporter, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		tp.Shutdown(context.Background())
		mp.Shutdown(context.Background())
	})
	exp, err := NewExporter(append([]Option{
		WithTracerProvider(tp),
		WithMeterProvider(mp),
	}, opts...)...)
	if err != nil {
		t.Fatalf("NewExporter: %v", err)
	}
	return exp, sr, reader
}

// successEntry fabricates a completed HTTPS exchange stuffed with
// high-cardinality secrets that must never reach OTel.
func successEntry() *recorder.Entry {
	return &recorder.Entry{
		StartedDateTime: time.Now().UTC().Format(time.RFC3339),
		Time:            42.5,
		State:           recorder.StateCompleted,
		TraceID:         "trace-0123456789abcdef",
		ExchangeID:      "exchange-fedcba9876543210",
		Request: &recorder.Request{
			Method:      "GET",
			URL:         "https://api.example.com/v1/orders/9871?token=SECRETTOKEN&customer=42",
			HTTPVersion: "HTTP/2.0",
			Headers: []recorder.NameValuePair{
				{Name: "Authorization", Value: "Bearer SECRETHEADER"},
			},
			QueryString: []recorder.NameValuePair{{Name: "token", Value: "SECRETTOKEN"}},
			Cookies:     []recorder.Cookie{{Name: "session", Value: "SECRETCOOKIE"}},
			HeadersSize: -1,
		},
		Response: &recorder.Response{
			Status:      200,
			StatusText:  "OK",
			HTTPVersion: "HTTP/2.0",
			Headers:     []recorder.NameValuePair{{Name: "Set-Cookie", Value: "SECRETSETCOOKIE"}},
			Content:     &recorder.Content{Size: 512, MimeType: "application/json", Text: `{"card":"SECRETBODY"}`, Decoded: true},
			HeadersSize: -1,
			BodySize:    -1,
		},
		Timings: &recorder.Timings{Blocked: 1, DNS: 2, Connect: 3, SSL: 4, Send: 0.5, Wait: 30, Receive: 2},
		Network: &recorder.NetworkInfo{ConnectionReused: true, WasIdle: true, HTTP2: true, DNSCoalesced: true},
		TLS:     &recorder.TLSInfo{Version: "TLS 1.3", CipherSuite: "TLS_AES_128_GCM_SHA256", DidResume: true},
		RequestBody: &recorder.BodyInfo{
			Present: true, Complete: true, TotalBytes: 128, CapturedBytes: 128,
		},
		ResponseBody: &recorder.BodyInfo{
			Present: true, Complete: true, TotalBytes: 512, CapturedBytes: 512,
		},
	}
}

func failureEntry() *recorder.Entry {
	return &recorder.Entry{
		StartedDateTime: time.Now().UTC().Format(time.RFC3339),
		Time:            5,
		State:           recorder.StateFailed,
		Request: &recorder.Request{
			Method: "POST", URL: "http://broken.example.com/pay?card=SECRET", HTTPVersion: "",
			HeadersSize: -1,
		},
		Response: &recorder.Response{Status: 0, Content: &recorder.Content{MimeType: "x-unknown"}, HeadersSize: -1, BodySize: -1},
		Timings:  &recorder.Timings{Blocked: -1, DNS: -1, Connect: -1, SSL: -1, Send: -1, Wait: -1, Receive: -1},
		Error: &recorder.ErrorInfo{
			Phase:   recorder.PhaseDNS,
			Type:    "*net.DNSError",
			Message: "lookup broken.example.com: no such host SECRETINMESSAGE",
		},
	}
}

func edgeCaseEntry() *recorder.Entry {
	e := successEntry()
	e.State = recorder.StateClosedEarly
	e.RequestBody.Truncated = true
	e.ResponseBody.Truncated = true
	e.ResponseBody.ClosedEarly = true
	e.ResponseBody.Complete = false
	return e
}

func eventAttrMap(t *testing.T, ev sdktrace.Event) map[string]attribute.Value {
	t.Helper()
	m := make(map[string]attribute.Value, len(ev.Attributes))
	for _, kv := range ev.Attributes {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func forbidSecrets(t *testing.T, attrs map[string]attribute.Value) {
	t.Helper()
	for key, val := range attrs {
		lk := strings.ToLower(key)
		for _, banned := range []string{"url.full", "url.path", "url.query", "header", "cookie", "body.text"} {
			if strings.Contains(lk, banned) {
				t.Errorf("forbidden attribute key %q", key)
			}
		}
		if val.Type() != attribute.STRING {
			continue
		}
		s := val.AsString()
		if strings.Contains(s, "SECRET") {
			t.Errorf("secret leaked through %q = %q", key, s)
		}
		for _, banned := range []string{"/v1/orders", "token=", "customer=", "?"} {
			if strings.Contains(s, banned) {
				t.Errorf("high-cardinality URL material leaked through %q = %q", key, s)
			}
		}
	}
}

func TestSpanEventOnActiveSpan(t *testing.T) {
	exp, sr, _ := testSetup(t, WithIncludeIDs(true))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	defer tp.Shutdown(context.Background())

	ctx, parent := tp.Tracer("test").Start(context.Background(), "logical-op")
	exp.OnEntryCompleted(ctx, successEntry())
	parent.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want only the caller's span", len(spans))
	}
	events := spans[0].Events()
	if len(events) != 1 || events[0].Name != EventName {
		t.Fatalf("events = %+v", events)
	}
	attrs := eventAttrMap(t, events[0])
	forbidSecrets(t, attrs)

	want := map[string]any{
		"http.request.method":            "GET",
		"http.response.status_code":      int64(200),
		"network.protocol.name":          "http",
		"network.protocol.version":       "2",
		"url.scheme":                     "https",
		"server.address":                 "api.example.com",
		"recorder.state":                 "completed",
		"recorder.duration_ms":           42.5,
		"recorder.request.body.bytes":    int64(128),
		"recorder.response.body.bytes":   int64(512),
		"recorder.response.decoded":      true,
		"recorder.network.reused":        true,
		"recorder.network.http2":         true,
		"recorder.network.was_idle":      true,
		"recorder.network.dns_coalesced": true,
		"recorder.tls.version":           "TLS 1.3",
		"recorder.tls.cipher_suite":      "TLS_AES_128_GCM_SHA256",
		"recorder.tls.resumed":           true,
		"recorder.timings.wait":          30.0,
	}
	for key, expect := range want {
		got, ok := attrs[key]
		if !ok {
			t.Errorf("missing attribute %q", key)
			continue
		}
		if got.AsInterface() != expect {
			t.Errorf("%s = %v, want %v", key, got.AsInterface(), expect)
		}
	}
	// IDs opted in: present as span event attributes.
	if _, ok := attrs["recorder.trace_id"]; !ok {
		t.Errorf("recorder.trace_id missing despite WithIncludeIDs")
	}
}

func TestNoSpanCreatedByDefault(t *testing.T) {
	exp, sr, _ := testSetup(t)
	exp.OnEntryCompleted(context.Background(), successEntry())
	if got := len(sr.Ended()); got != 0 {
		t.Fatalf("spans created without opt-in: %d", got)
	}
}

func TestCreateSpanIfNoneAndErrorStatus(t *testing.T) {
	exp, sr, _ := testSetup(t, WithCreateSpanIfNone(true), WithSpanErrorStatus(true))
	exp.OnEntryCompleted(context.Background(), failureEntry())

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	span := spans[0]
	if span.Name() != "HTTP POST" {
		t.Errorf("span name = %q", span.Name())
	}
	if span.Status().Code != codes.Error || span.Status().Description != recorder.PhaseDNS {
		t.Errorf("status = %+v", span.Status())
	}
	if len(span.Events()) != 1 {
		t.Fatalf("events = %d", len(span.Events()))
	}
	attrs := eventAttrMap(t, span.Events()[0])
	forbidSecrets(t, attrs)
	if got := attrs["recorder.error.phase"].AsString(); got != recorder.PhaseDNS {
		t.Errorf("error phase = %q", got)
	}
	// All timings were -1: none may appear.
	for key := range attrs {
		if strings.HasPrefix(key, "recorder.timings.") {
			t.Errorf("unmeasured timing exported: %s", key)
		}
	}
	// Default (no WithIncludeIDs): correlation IDs stay out.
	if _, ok := attrs["recorder.trace_id"]; ok {
		t.Errorf("trace id exported without opt-in")
	}
}

// collectMetrics flattens the manual reader's output by metric name.
func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func attrSetToMap(set attribute.Set) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for iter := set.Iter(); iter.Next(); {
		kv := iter.Attribute()
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func TestMetricsRecorded(t *testing.T) {
	exp, _, reader := testSetup(t)
	ctx := context.Background()
	exp.OnEntryCompleted(ctx, successEntry())
	exp.OnEntryCompleted(ctx, failureEntry())
	exp.OnEntryCompleted(ctx, edgeCaseEntry())

	metrics := collectMetrics(t, reader)
	for _, name := range []string{
		"recorder.http.client.duration",
		"recorder.http.client.request.body.size",
		"recorder.http.client.response.body.size",
		"recorder.http.client.failures",
		"recorder.http.client.closed_early",
		"recorder.http.client.body.truncated",
	} {
		if _, ok := metrics[name]; !ok {
			t.Errorf("metric %q missing", name)
		}
	}

	dur := metrics["recorder.http.client.duration"].Data.(metricdata.Histogram[float64])
	var total uint64
	for _, dp := range dur.DataPoints {
		total += dp.Count
		attrs := attrSetToMap(dp.Attributes)
		forbidSecrets(t, attrs)
		// Metric labels are the class, never the exact code, and never IDs.
		if _, ok := attrs["http.response.status_code"]; ok {
			t.Errorf("exact status code used as metric label")
		}
		cls := attrs["http.response.status_class"].AsString()
		if cls != "2xx" && cls != "0" {
			t.Errorf("status class = %q", cls)
		}
		if _, ok := attrs["recorder.trace_id"]; ok {
			t.Errorf("trace id leaked into metrics")
		}
	}
	if total != 3 {
		t.Errorf("duration count = %d, want 3", total)
	}

	fails := metrics["recorder.http.client.failures"].Data.(metricdata.Sum[int64])
	if len(fails.DataPoints) != 1 || fails.DataPoints[0].Value != 1 {
		t.Fatalf("failures = %+v", fails.DataPoints)
	}
	fattrs := attrSetToMap(fails.DataPoints[0].Attributes)
	if fattrs["recorder.error.phase"].AsString() != recorder.PhaseDNS {
		t.Errorf("failure phase label = %+v", fattrs)
	}
	if fattrs["http.response.status_class"].AsString() != "0" {
		t.Errorf("failure status class = %+v", fattrs)
	}

	closed := metrics["recorder.http.client.closed_early"].Data.(metricdata.Sum[int64])
	var closedTotal int64
	for _, dp := range closed.DataPoints {
		closedTotal += dp.Value
	}
	if closedTotal != 1 {
		t.Errorf("closed_early = %d", closedTotal)
	}

	trunc := metrics["recorder.http.client.body.truncated"].Data.(metricdata.Sum[int64])
	directions := map[string]int64{}
	for _, dp := range trunc.DataPoints {
		attrs := attrSetToMap(dp.Attributes)
		directions[attrs["recorder.body.direction"].AsString()] += dp.Value
	}
	if directions["request"] != 1 || directions["response"] != 1 {
		t.Errorf("truncated directions = %+v", directions)
	}
}

func TestCustomAttributesBoundedAndClamped(t *testing.T) {
	long := strings.Repeat("x", 1000)
	var many []attribute.KeyValue
	for i := 0; i < 100; i++ {
		many = append(many, attribute.String("custom.attr", long))
	}
	exp, sr, reader := testSetup(t,
		WithCreateSpanIfNone(true),
		WithMaxAttributeLength(32),
		WithSpanEventAttributes(func(*recorder.Entry) []attribute.KeyValue {
			return append([]attribute.KeyValue{attribute.String("http.route", "/v1/orders/{id}")}, many...)
		}),
		WithMetricAttributes(func(*recorder.Entry) []attribute.KeyValue {
			return []attribute.KeyValue{attribute.String("http.route", "/v1/orders/{id}")}
		}),
	)
	exp.OnEntryCompleted(context.Background(), successEntry())

	span := sr.Ended()[0]
	attrs := eventAttrMap(t, span.Events()[0])
	if got := attrs["http.route"].AsString(); got != "/v1/orders/{id}" {
		t.Errorf("route = %q", got)
	}
	if got := attrs["custom.attr"].AsString(); len(got) != 32 {
		t.Errorf("custom string not clamped: %d bytes", len(got))
	}
	// 100 supplied, but the list is capped.
	if len(span.Events()[0].Attributes) > 64 {
		t.Errorf("attribute count unbounded: %d", len(span.Events()[0].Attributes))
	}

	metrics := collectMetrics(t, reader)
	dur := metrics["recorder.http.client.duration"].Data.(metricdata.Histogram[float64])
	mattrs := attrSetToMap(dur.DataPoints[0].Attributes)
	if mattrs["http.route"].AsString() != "/v1/orders/{id}" {
		t.Errorf("custom metric attribute missing: %+v", mattrs)
	}
}

func TestConcurrentExport(t *testing.T) {
	exp, sr, reader := testSetup(t, WithCreateSpanIfNone(true))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				exp.OnEntryCompleted(context.Background(), successEntry())
				exp.OnEntryCompleted(context.Background(), failureEntry())
			}
		}()
	}
	wg.Wait()
	if got := len(sr.Ended()); got != 8*25*2 {
		t.Fatalf("spans = %d", got)
	}
	metrics := collectMetrics(t, reader)
	dur := metrics["recorder.http.client.duration"].Data.(metricdata.Histogram[float64])
	var total uint64
	for _, dp := range dur.DataPoints {
		total += dp.Count
	}
	if total != 8*25*2 {
		t.Errorf("duration samples = %d", total)
	}
}

func TestNilEntryIgnored(t *testing.T) {
	exp, sr, _ := testSetup(t, WithCreateSpanIfNone(true))
	exp.OnEntryCompleted(context.Background(), nil)
	if len(sr.Ended()) != 0 {
		t.Errorf("nil entry produced a span")
	}
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{0: "0", -1: "0", 103: "1xx", 200: "2xx", 301: "3xx", 404: "4xx", 503: "5xx"} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestProtocolMapping(t *testing.T) {
	cases := []struct {
		in            string
		name, version string
	}{
		{"HTTP/2.0", "http", "2"},
		{"HTTP/1.1", "http", "1.1"},
		{"", "", ""},
		{"SPDY/3", "", ""},
	}
	for _, c := range cases {
		e := &recorder.Entry{Response: &recorder.Response{HTTPVersion: c.in}}
		name, version := protocol(e)
		if name != c.name || version != c.version {
			t.Errorf("protocol(%q) = %q/%q, want %q/%q", c.in, name, version, c.name, c.version)
		}
	}
}
