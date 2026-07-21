// Package otelrecorder exports finished recorder entries as OpenTelemetry
// span events and metrics. It lives in its own Go module so the core
// recorder package stays dependency-free.
//
// The adapter plugs into recorder.WithOnEntryCompleted:
//
//	exporter, err := otelrecorder.NewExporter()
//	if err != nil { ... }
//	client := &http.Client{
//		Transport: recorder.NewTransport(http.DefaultTransport, rec,
//			recorder.WithOnEntryCompleted(exporter.OnEntryCompleted)),
//	}
//
// When the request context carries an active (recording) span, the entry is
// attached to it as a span event named "recorder.http.exchange". Without an
// active span nothing is traced by default; WithCreateSpanIfNone(true) makes
// the exporter synthesize a client span covering the exchange instead.
// Metrics are always recorded.
//
// # Cardinality and safety
//
// Only low-cardinality, bounded fields ever leave this adapter. It never
// exports full URLs, paths, query strings, header or cookie values, body
// content, or raw HAR JSON — not even in redacted form. server.address
// carries the host only. High-cardinality correlation IDs (_traceId /
// _exchangeId) are excluded by default and can be opted into as span event
// attributes only (WithIncludeIDs); they are never metric attributes.
// Metric attributes are limited to method, status code *class* ("2xx",
// "0"), state, scheme and protocol. Specialized instruments add only bounded
// dimensions: HTTP phase, body direction/capture outcome, protection mode,
// fixed fail-closed reason, body-redactor kind/outcome, and fixed async drop
// reason. Numeric facts
// (timings, body sizes and protection counts) are measurements, never labels.
// String attribute values are clamped to MaxAttributeLength.
//
// In addition to total duration, streamed body sizes and exchange failures,
// the exporter reports per-phase latency, retained body bytes, capture
// outcomes, redacted/encrypted/tokenized value counts, fail-closed fallbacks,
// body-redactor outcomes, and optional AsyncRecorder queue/backpressure health.
// No rule names, key IDs, protected values, error messages, body content,
// storage paths or sink identities become attributes.
//
// Custom attributes (WithSpanEventAttributes / WithMetricAttributes) are the
// intended hook for user-controlled low-cardinality dimensions such as a URL
// path *template* ("/users/{id}"). Never derive them from raw request data:
// every distinct metric attribute value creates a new time series, and
// unbounded values (IDs, full paths, tokens) will blow up your metrics
// backend.
//
// When the full HAR entry is needed, write it to a dedicated sink
// (recorder.HARFileRecorder, recorder.JSONStreamRecorder) — an OTel
// attribute is the wrong place for a document.
package otelrecorder

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	recorder "github.com/mgurevin/recorder"
)

const (
	instrumentationName = "github.com/mgurevin/recorder/otelrecorder"

	// EventName is the span event name used for every exchange.
	EventName = "recorder.http.exchange"

	// maxCustomAttributes bounds user-supplied attribute lists.
	maxCustomAttributes = 16

	// defaultMaxAttributeLength clamps string attribute values.
	defaultMaxAttributeLength = 128
)

type config struct {
	tracerProvider  trace.TracerProvider
	meterProvider   metric.MeterProvider
	createSpan      bool
	spanErrorStatus bool
	includeIDs      bool
	maxAttrLen      int
	spanAttrsFn     func(*recorder.Entry) []attribute.KeyValue
	metricAttrsFn   func(*recorder.Entry) []attribute.KeyValue
	asyncRecorder   *recorder.AsyncRecorder
	fileBodyStore   *recorder.FileBodyStore
}

// Option configures the Exporter.
type Option func(*config)

// WithTracerProvider sets the TracerProvider; default otel.GetTracerProvider().
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithMeterProvider sets the MeterProvider; default otel.GetMeterProvider().
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// WithCreateSpanIfNone makes the exporter start (and immediately end) a
// client span spanning the exchange when the context has no active span.
// Default: no span is created.
func WithCreateSpanIfNone(v bool) Option {
	return func(c *config) { c.createSpan = v }
}

// WithSpanErrorStatus sets the touched span's status to Error when the entry
// carries a transport failure. Default off: only the event and the failure
// counter are produced.
func WithSpanErrorStatus(v bool) Option {
	return func(c *config) { c.spanErrorStatus = v }
}

// WithIncludeIDs adds recorder.trace_id / recorder.exchange_id to span event
// attributes. They are high-cardinality and never become metric attributes.
func WithIncludeIDs(v bool) Option {
	return func(c *config) { c.includeIDs = v }
}

// WithMaxAttributeLength clamps string attribute values to n bytes
// (default 128; n <= 0 disables clamping).
func WithMaxAttributeLength(n int) Option {
	return func(c *config) { c.maxAttrLen = n }
}

// WithSpanEventAttributes appends user-supplied attributes to every span
// event. Keep them low-cardinality (e.g. a route template); the list is
// capped at 16 entries and string values are clamped.
func WithSpanEventAttributes(fn func(*recorder.Entry) []attribute.KeyValue) Option {
	return func(c *config) { c.spanAttrsFn = fn }
}

// WithMetricAttributes appends user-supplied attributes to every metric
// sample. CARDINALITY WARNING: every distinct value creates a new time
// series — use only bounded, user-controlled values such as a URL path
// template, never raw paths or IDs. Capped at 16 entries, string values
// clamped.
func WithMetricAttributes(fn func(*recorder.Entry) []attribute.KeyValue) Option {
	return func(c *config) { c.metricAttrsFn = fn }
}

// WithAsyncRecorder exports bounded queue, backpressure, drop and downstream
// health measurements for asyncRecorder. The exporter must be closed to
// unregister the OpenTelemetry callback. No sink identity or entry data is
// exported.
func WithAsyncRecorder(asyncRecorder *recorder.AsyncRecorder) Option {
	return func(c *config) { c.asyncRecorder = asyncRecorder }
}

// WithFileBodyStore exports bounded capacity and lifecycle measurements for
// store. The exporter must be closed to unregister the OpenTelemetry callback.
// Asset references and filesystem paths are never exported.
func WithFileBodyStore(store *recorder.FileBodyStore) Option {
	return func(c *config) { c.fileBodyStore = store }
}

// Exporter converts finished entries into OTel span events and metrics.
// Safe for concurrent use; entries arrive from whichever goroutine finished
// the exchange.
type Exporter struct {
	cfg    config
	tracer trace.Tracer

	duration        metric.Float64Histogram
	phase           metric.Float64Histogram
	reqSize         metric.Int64Histogram
	respSize        metric.Int64Histogram
	capturedSize    metric.Int64Histogram
	failures        metric.Int64Counter
	closedEarly     metric.Int64Counter
	truncated       metric.Int64Counter
	captures        metric.Int64Counter
	redacted        metric.Int64Counter
	fallbacks       metric.Int64Counter
	bodyRedactions  metric.Int64Counter
	asyncMetrics    *asyncRecorderMetrics
	fileBodyMetrics *fileBodyStoreMetrics
}

// NewExporter builds an Exporter. Instrument creation errors (invalid meter
// implementations) are returned rather than silently dropped.
func NewExporter(opts ...Option) (*Exporter, error) {
	cfg := config{maxAttrLen: defaultMaxAttributeLength}

	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.tracerProvider == nil {
		cfg.tracerProvider = otel.GetTracerProvider()
	}

	if cfg.meterProvider == nil {
		cfg.meterProvider = otel.GetMeterProvider()
	}

	e := &Exporter{cfg: cfg, tracer: cfg.tracerProvider.Tracer(instrumentationName)}
	meter := cfg.meterProvider.Meter(instrumentationName)

	var err error
	if e.duration, err = meter.Float64Histogram("recorder.http.client.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Total duration of recorded HTTP exchanges")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create duration histogram: %w", err)
	}

	if e.phase, err = meter.Float64Histogram("recorder.http.client.phase.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of measured HTTP exchange phases")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create phase duration histogram: %w", err)
	}

	if e.reqSize, err = meter.Int64Histogram("recorder.http.client.request.body.size",
		metric.WithUnit("By"),
		metric.WithDescription("Request body bytes that flowed through the stream")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create request size histogram: %w", err)
	}

	if e.respSize, err = meter.Int64Histogram("recorder.http.client.response.body.size",
		metric.WithUnit("By"),
		metric.WithDescription("Response body bytes that flowed through the stream")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create response size histogram: %w", err)
	}

	if e.capturedSize, err = meter.Int64Histogram("recorder.body.captured.size",
		metric.WithUnit("By"),
		metric.WithDescription("Body bytes retained by the configured capture policy")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create captured body size histogram: %w", err)
	}

	if e.failures, err = meter.Int64Counter("recorder.http.client.failures",
		metric.WithDescription("Exchanges that failed at the transport or body layer, by phase")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create failure counter: %w", err)
	}

	if e.closedEarly, err = meter.Int64Counter("recorder.http.client.closed_early",
		metric.WithDescription("Response bodies closed before EOF")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create closed-early counter: %w", err)
	}

	if e.truncated, err = meter.Int64Counter("recorder.http.client.body.truncated",
		metric.WithDescription("Body captures truncated by the configured limit")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create truncated counter: %w", err)
	}

	if e.captures, err = meter.Int64Counter("recorder.body.capture.operations",
		metric.WithDescription("Body capture outcomes by direction")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create body capture counter: %w", err)
	}

	if e.redacted, err = meter.Int64Counter("recorder.redaction.values",
		metric.WithDescription("Recorded sensitive values by protection mode and direction")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create redaction value counter: %w", err)
	}

	if e.fallbacks, err = meter.Int64Counter("recorder.redaction.fallbacks",
		metric.WithDescription("Fail-closed protection fallbacks by fixed reason and direction")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create redaction fallback counter: %w", err)
	}

	if e.bodyRedactions, err = meter.Int64Counter("recorder.body.redaction.operations",
		metric.WithDescription("Body redactor executions by bounded kind, outcome and direction")); err != nil {
		return nil, fmt.Errorf("otelrecorder: create body redaction counter: %w", err)
	}

	if cfg.asyncRecorder != nil {
		e.asyncMetrics, err = newAsyncRecorderMetrics(meter, cfg.asyncRecorder)
		if err != nil {
			return nil, err
		}
	}

	if cfg.fileBodyStore != nil {
		e.fileBodyMetrics, err = newFileBodyStoreMetrics(meter, cfg.fileBodyStore)
		if err != nil {
			if e.asyncMetrics != nil {
				_ = e.asyncMetrics.close()
			}

			return nil, err
		}
	}

	return e, nil
}

// Close unregisters observable metric callbacks owned by the exporter. It is
// safe to call more than once. Close does not close the AsyncRecorder.
func (e *Exporter) Close() error {
	if e == nil {
		return nil
	}

	var firstErr error
	if e.asyncMetrics != nil {
		firstErr = e.asyncMetrics.close()
	}

	if e.fileBodyMetrics != nil {
		if err := e.fileBodyMetrics.close(); firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// OnEntryCompleted implements recorder.OnEntryCompleted. Wire it up with
// recorder.WithOnEntryCompleted(exporter.OnEntryCompleted).
func (e *Exporter) OnEntryCompleted(ctx context.Context, entry *recorder.Entry) {
	if entry == nil {
		return
	}

	e.recordMetrics(ctx, entry)
	e.recordSpan(ctx, entry)
}

func (e *Exporter) recordSpan(ctx context.Context, entry *recorder.Entry) {
	span := trace.SpanFromContext(ctx)
	created := false

	if !span.IsRecording() {
		if !e.cfg.createSpan {
			return
		}

		start := entry.StartTime()
		_, span = e.tracer.Start(ctx, spanName(entry),
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithTimestamp(start))
		created = true
	}

	end := entry.StartTime().Add(time.Duration(entry.Time * float64(time.Millisecond)))
	span.AddEvent(EventName,
		trace.WithTimestamp(end),
		trace.WithAttributes(e.eventAttributes(entry)...))

	if e.cfg.spanErrorStatus && entry.Error != nil {
		span.SetStatus(codes.Error, e.clamp(entry.Error.Phase))
	}

	if created {
		span.SetAttributes(e.baseAttributes(entry)...)
		span.End(trace.WithTimestamp(end))
	}
}

func (e *Exporter) recordMetrics(ctx context.Context, entry *recorder.Entry) {
	attrs := e.metricAttributes(entry)
	opt := metric.WithAttributes(attrs...)

	e.duration.Record(ctx, entry.Time, opt)
	e.recordPhaseMetrics(ctx, entry, attrs)

	if rb := entry.RequestBody; rb != nil {
		e.reqSize.Record(ctx, rb.TotalBytes, opt)
		e.recordBodyCapture(ctx, rb, "request", attrs)

		if rb.Truncated {
			e.truncated.Add(ctx, 1, metric.WithAttributes(append(attrs,
				attribute.String("recorder.body.direction", "request"))...))
		}
	}

	if rb := entry.ResponseBody; rb != nil {
		e.respSize.Record(ctx, rb.TotalBytes, opt)
		e.recordBodyCapture(ctx, rb, "response", attrs)

		if rb.Truncated {
			e.truncated.Add(ctx, 1, metric.WithAttributes(append(attrs,
				attribute.String("recorder.body.direction", "response"))...))
		}
	}

	if entry.Error != nil {
		e.failures.Add(ctx, 1, metric.WithAttributes(append(attrs,
			attribute.String("recorder.error.phase", e.clamp(entry.Error.Phase)))...))
	}

	if closedEarly(entry) {
		e.closedEarly.Add(ctx, 1, opt)
	}

	e.recordRedactionMetrics(ctx, entry.Redaction, attrs)
}

func (e *Exporter) recordPhaseMetrics(ctx context.Context, entry *recorder.Entry, attrs []attribute.KeyValue) {
	if entry.Timings == nil {
		return
	}

	for _, phase := range []struct {
		name  string
		value float64
	}{
		{"blocked", entry.Timings.Blocked},
		{"dns", entry.Timings.DNS},
		{"connect", entry.Timings.Connect},
		{"tls", entry.Timings.SSL},
		{"send", entry.Timings.Send},
		{"wait", entry.Timings.Wait},
		{"receive", entry.Timings.Receive},
	} {
		if phase.value >= 0 {
			e.phase.Record(ctx, phase.value, metric.WithAttributes(metricAttrs(attrs,
				attribute.String("recorder.http.phase", phase.name))...))
		}
	}
}

func (e *Exporter) recordBodyCapture(ctx context.Context, body *recorder.BodyInfo,
	direction string, attrs []attribute.KeyValue,
) {
	if !body.Present {
		return
	}

	bodyAttrs := metricAttrs(attrs, attribute.String("recorder.body.direction", direction))
	e.capturedSize.Record(ctx, body.CapturedBytes, metric.WithAttributes(bodyAttrs...))
	e.captures.Add(ctx, 1, metric.WithAttributes(metricAttrs(bodyAttrs,
		attribute.String("recorder.body.capture.outcome", bodyCaptureOutcome(body)))...))
}

func bodyCaptureOutcome(body *recorder.BodyInfo) string {
	switch {
	case body.ReadError != "" || body.CloseError != "":
		return "failed"

	case body.ClosedEarly:
		return "closed_early"

	case body.Truncated:
		return "truncated"

	case body.Complete:
		return "complete"

	default:
		return "incomplete"
	}
}

func (e *Exporter) recordRedactionMetrics(ctx context.Context, info *recorder.RedactionInfo,
	attrs []attribute.KeyValue,
) {
	if info == nil {
		return
	}

	e.recordRedactionScope(ctx, info.Request, "request", attrs)
	e.recordRedactionScope(ctx, info.Response, "response", attrs)
}

func (e *Exporter) recordRedactionScope(ctx context.Context, scope *recorder.RedactionScopeInfo,
	direction string, attrs []attribute.KeyValue,
) {
	if scope == nil {
		return
	}

	e.recordProtectionCounts(ctx, scope.Protection, direction, attrs)

	if scope.Body == nil {
		return
	}

	bodyAttrs := metricAttrs(attrs,
		attribute.String("recorder.body.direction", direction),
		attribute.String("recorder.body.redaction.kind", bodyRedactionKind(scope.Body.Kind)),
		attribute.String("recorder.body.redaction.outcome", bodyRedactionOutcome(scope.Body.Outcome)),
	)
	e.bodyRedactions.Add(ctx, 1, metric.WithAttributes(bodyAttrs...))
	e.recordProtectionCounts(ctx, scope.Body.Protection, direction, attrs)
}

func (e *Exporter) recordProtectionCounts(ctx context.Context, counts *recorder.ProtectionCounts,
	direction string, attrs []attribute.KeyValue,
) {
	if counts == nil {
		return
	}

	for _, mode := range []struct {
		name  string
		count int64
	}{
		{"redacted", counts.Redacted},
		{"encrypted", counts.Encrypted},
		{"tokenized", counts.Tokenized},
	} {
		if mode.count > 0 {
			e.redacted.Add(ctx, mode.count, metric.WithAttributes(metricAttrs(attrs,
				attribute.String("recorder.redaction.direction", direction),
				attribute.String("recorder.protection.mode", mode.name))...))
		}
	}

	for reason, count := range counts.Fallbacks {
		if count > 0 {
			e.fallbacks.Add(ctx, count, metric.WithAttributes(metricAttrs(attrs,
				attribute.String("recorder.redaction.direction", direction),
				attribute.String("recorder.protection.reason", protectionFallbackReason(reason)))...))
		}
	}
}

func metricAttrs(base []attribute.KeyValue, extra ...attribute.KeyValue) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(base)+len(extra))
	attrs = append(attrs, base...)
	attrs = append(attrs, extra...)

	return attrs
}

func protectionFallbackReason(reason string) string {
	switch reason {
	case "value_too_large", "encryption_failed", "tokenization_failed":
		return reason

	default:
		return "other"
	}
}

func bodyRedactionKind(kind string) string {
	switch kind {
	case "custom", "builtin:multipart", "builtin:form", "builtin:json", "builtin:xml", "builtin:sniff":
		return kind

	default:
		return "other"
	}
}

func bodyRedactionOutcome(outcome string) string {
	switch outcome {
	case recorder.BodyRedactionRedacted, recorder.BodyRedactionUnchanged, recorder.BodyRedactionFailed:
		return outcome

	default:
		return "other"
	}
}

func spanName(entry *recorder.Entry) string {
	if entry.Request != nil && entry.Request.Method != "" {
		return "HTTP " + entry.Request.Method
	}

	return "HTTP"
}

func closedEarly(entry *recorder.Entry) bool {
	if entry.State == recorder.StateClosedEarly {
		return true
	}

	return entry.ResponseBody != nil && entry.ResponseBody.ClosedEarly
}
