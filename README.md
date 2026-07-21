# recorder

`recorder` is a dependency-free evidence-generation tool for Go HTTP clients.
It records complete exchanges as HAR 1.2—including failures, timings,
connection/TLS facts, streamed body lifecycle, redaction audit, and redirect
correlation—using only facts observable at the `http.RoundTripper` boundary.
Information that was not observed remains absent or explicitly unknown.

It was built for cases—especially financial API integrations—where preserving
an accurate, privacy-aware record of what the client observed is operationally
important. It never retries requests, consumes bodies on the caller's behalf,
or modifies live request and response data. Recording failures are contained:
they may reduce the captured evidence, but never replace or alter the HTTP
response or error returned to the application.

Its design is guided by four principles: observe without interference, record
only verifiable facts, fail without affecting HTTP behavior, and keep resource
use and sensitive data bounded.

## Features

- Complete HAR 1.2 records for successful and failed HTTP exchanges
- DNS, connect, proxy, TLS, request, response, and body-stream diagnostics
- Accurate timing waterfalls and redirect/trace correlation
- Bounded body capture with memory or managed file storage
- Streaming redaction for JSON, NDJSON, XML, form, and multipart bodies
- Redact, AES-GCM encrypt, or HMAC-tokenize sensitive values
- Request-scoped redaction, capture policies, sampling, and tail retention
- Bounded asynchronous delivery with batching and backpressure controls
- OpenTelemetry metrics and span-event integration
- Browser-only HAR Inspector with replay, protection audit, and safe previews
- Standard-library-only core package

## Install

```sh
go get github.com/mgurevin/recorder
```

The minimum supported Go release is documented in `go.mod` and verified in CI.

## Quick start

```go
rec := recorder.NewMemoryRecorder()
config := recorder.DefaultConfig()

client := &http.Client{
	Transport: recorder.NewTransport(http.DefaultTransport, rec, config),
}

resp, err := client.Get("https://example.com/")
if err == nil {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

_ = rec.WriteHAR(os.Stdout)
```

A successful exchange finalizes when the response body reaches EOF, is closed,
or fails. Transport errors finalize immediately. A response body that is never
read and never closed produces no entry and also prevents normal `net/http`
connection reuse.

## Configuration

The public configuration model is intentionally explicit. Start with a default
value, modify fields, and pass it to the constructor:

```go
config := recorder.DefaultConfig()
config.CaptureRequestBody = true
config.CaptureResponseBody = true
config.EmbedBodies = false
config.MaxResponseBodyBytes = 4 << 20
config.HashBodies = true
config.Redaction = recorder.RedactionConfig{
	Common: recorder.RedactionRules{
		Headers:    recorder.DefaultRedactedHeaders(),
		JSONFields: []string{"password", "token"},
		XMLElements: []string{"Password"},
	},
}

transport := recorder.NewTransport(http.DefaultTransport, rec, config)
```

`Config{}` is a deliberately minimal zero value. `DefaultConfig()` is the
recommended production baseline. Functional transport options are not part of
the v1 API; one configuration field has one source of truth.

Important defaults:

- body content capture, embedding, hashing, and raw trace capture are off;
- headers, cookies, TLS facts, and certificate metadata are on;
- request and response capture limits are 1 MiB;
- authorization, proxy authorization, cookies, and common API-key headers are
  redacted;
- gzip and deflate record-time decoding are registered;
- internal recorder failures are logged through `log.Printf` unless a custom
  `Logf` is supplied;
- sampling and retention policies are nil, so every exchange is recorded.

Request-scoped redaction can add rules without creating another client:

```go
req = recorder.RequestWithRedaction(req, recorder.RedactionConfig{
	Request: recorder.RedactionRules{JSONFields: []string{"cardNumber"}},
})
```

## Body capture and storage

Captured bytes pass through decoding and streaming redaction once, then reach
the configured `BodyStore`. Embedded and externally stored representations
therefore obey the same rules. JSON, NDJSON, XML, form-urlencoded, and multipart
built-ins preserve non-redacted bytes; malformed input fails closed around a
matched sensitive value.

For managed disk storage:

```go
storeConfig := recorder.DefaultFileBodyStoreConfig()
storeConfig.MaxBytes = 1 << 30
storeConfig.MaxFiles = 10_000

store, err := recorder.NewFileBodyStore("/var/spool/recorder-bodies", storeConfig)
if err != nil { return err }

config := recorder.DefaultConfig()
config.CaptureResponseBody = true
config.BodyStore = store
transport := recorder.NewTransport(http.DefaultTransport, rec, config)
```

`FileBodyStore` publishes committed assets atomically, reconciles abandoned
partials, enforces byte/file caps, and exposes `Open`, `Release`, `Reconcile`,
and bounded statistics. The HAR entry and body asset are separate durability
domains; see [DESIGN.md](DESIGN.md).

Custom redactors implement `BodyRedactor`. They identify sensitive values and
delegate their representation to the supplied `BodyValueProtector`, so redact,
encrypt, and tokenize modes behave identically for built-ins and extensions.
Complete non-importable examples live under [`docs/examples`](docs/examples).

## Sensitive-value protection

Selected values support three modes: fixed redaction, AES-GCM encryption, and
HMAC tokenization. Oversized values and protection failures fall back to
`[REDACTED]`. JSON protection encrypts the raw JSON token, including quotes or
container syntax, so exact reconstruction remains possible.

Key providers receive the request context. Tokens carry a key ID, and
`ProtectionKeyResolver`, `ProtectedTokenKeyID`,
`DecryptProtectedValueWith`, and `VerifyProtectedTokenWith` support archives
containing values written before and after key rotation. Never record keys in
HAR files or logs.

## Sampling and retention

Policies are function types—no interface/adapter pair is required:

```go
config.HeadSamplingPolicy = recorder.HeadSamplingPolicy(
	func(ctx context.Context, meta recorder.HeadSamplingMeta) recorder.HeadSamplingDecision {
		if meta.Path == "/health" { return recorder.HeadSampleDrop }
		return recorder.HeadSampleFull
	},
)
```

`NewRateHeadSampler` provides deterministic rate sampling. It prefers a key
installed with `WithSamplingKey`, then the trace ID. Tail retention runs after
capture and completion callbacks; discarded managed body assets are released.

## Async recording

```go
asyncConfig := recorder.DefaultAsyncRecorderConfig()
asyncConfig.QueueCapacity = 1024
asyncConfig.BatchSize = 64
asyncConfig.FlushInterval = 10 * time.Millisecond

async, err := recorder.NewAsyncRecorder(sink, asyncConfig)
if err != nil { return err }
defer async.Close(context.Background())
```

The default `AsyncBlock` policy preserves evidence but can delay exchange
finalization behind a stalled sink. Internal sink and callback failures are
logged by default with the same policy as Transport. Drop policies and a
bounded block timeout are explicit alternatives. Async recording is bounded
but not crash-durable.

## HAR extension contract

Recorder-specific entry data lives under one namespace:

```json
{
  "_recorder": {
    "schemaVersion": "1",
    "traceId": "...",
    "state": "completed",
    "network": {},
    "tls": {},
    "requestBody": {},
    "responseBody": {},
    "redaction": {}
  }
}
```

Removing `_recorder` leaves plain HAR 1.2. The frozen v1 contract is published
at [`schema/recorder-har-v1.schema.json`](schema/recorder-har-v1.schema.json).
The Inspector rejects unknown recorder schema versions instead of guessing.

## Inspector and OpenTelemetry

The browser-only Inspector opens local HAR files, supports safe body previews,
trace-chain waterfalls, replay commands, redaction audit, and in-memory
resolution of protected values. It is also deployable through GitHub Pages.

The separate [`otelrecorder`](otelrecorder) module exports bounded span events,
metrics, async queue health, managed body-store health, and sampling outcomes.
It never exports body content, raw URLs, key IDs, or error messages. Its full
metric/unit and alerting guide is in
[`otelrecorder/README.md`](otelrecorder/README.md).

## Operational guidance

- Consume or close every successful response body.
- Keep body capture bounded; avoid embedding large production bodies.
- Configure redaction before enabling capture and test representative payloads.
- Treat HAR files and body assets as sensitive evidence.
- Close and flush recorders during graceful shutdown.
- Monitor async drops, blocked writers, store utilization, redaction failures,
  and fail-closed protection fallbacks.

Design details are in [DESIGN.md](DESIGN.md), reproducible performance results
in [BENCHMARK.md](BENCHMARK.md), releases in [CHANGELOG.md](CHANGELOG.md), and
the release procedure in [RELEASING.md](RELEASING.md). Report vulnerabilities
privately as described in [SECURITY.md](SECURITY.md).

## License

MIT
