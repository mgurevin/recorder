# recorder

[![CI](https://github.com/mgurevin/recorder/actions/workflows/ci.yml/badge.svg)](https://github.com/mgurevin/recorder/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mgurevin/recorder.svg)](https://pkg.go.dev/github.com/mgurevin/recorder)
[![Coverage](https://codecov.io/gh/mgurevin/recorder/graph/badge.svg)](https://codecov.io/gh/mgurevin/recorder)

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
- Ephemeral single-browser live inspection for local Go development
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

A request-scoped annotation can describe why an exchange was made without a
recorder-specific extension:

```go
req = recorder.RequestWithComment(req, "Authorize payment for order 42")
```

The annotation is copied verbatim to the standard HAR `entry.comment` field
and appears in the Inspector. Redirect hops inherit it through the request
context. Comments are not redacted; never place credentials, tokens, personal
data, or other secrets in them.

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

`ProtectionKeyProvider` and `ProtectionKeyResolver` are direct function types;
no interface adapter is required. Providers receive the request context. Tokens
carry a key ID, and `ProtectedTokenKeyID`,
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

Multiple sinks can receive the same immutable entry without custom fan-out
code:

```go
fanout := recorder.NewMultiRecorder(durableSink, debugStream)

async, err := recorder.NewAsyncRecorder(fanout, recorder.DefaultAsyncRecorderConfig())
```

`MultiRecorder` calls every sink in argument order and joins failures without
short-circuiting. It preserves batch delivery where a sink supports it and
falls back to ordered `Record` calls otherwise. Fan-out is synchronous and
does not close downstream sinks; wrapping it with `AsyncRecorder` keeps sink
latency out of response finalization, while the application remains responsible
for closing each owned resource. The first sink is a required argument; passing
a nil sink is treated as a startup wiring bug and panics immediately.

```go
asyncConfig := recorder.DefaultAsyncRecorderConfig()
asyncConfig.QueueCapacity = 1024
asyncConfig.BatchSize = 64
asyncConfig.FlushInterval = 10 * time.Millisecond

async, err := recorder.NewAsyncRecorder(sink, asyncConfig)
if err != nil { return err }

// At shutdown, wait for queued entries and surface downstream failures.
if err := async.Close(shutdownContext); err != nil { return err }
```

The default `AsyncBlock` policy preserves evidence but can delay exchange
finalization behind a stalled sink. Internal sink and callback failures are
logged by default with the same policy as Transport, and `Close` returns the
first worker or downstream failure after draining. `Record` can report only a
synchronous queue-state failure because downstream writes happen later. Drop
policies and a bounded block timeout are explicit alternatives. Async recording
is bounded but not crash-durable.

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

The browser-only Inspector opens local HAR files and `JSONStreamRecorder`
NDJSON, supports safe body previews, trace-chain waterfalls, replay commands,
redaction audit, and in-memory resolution of protected values. It is also
deployable through GitHub Pages.

For local development, `DebugStreamRecorder` can publish finalized entries to
one Inspector window over a bounded SSE stream. It retains nothing without a
subscriber and reports dropped UI updates as visible gaps; it is intentionally
not a durable or production recorder. The application owns the loopback HTTP
server, and `AsyncRecorder` can isolate response finalization from live-view
encoding. See the complete [debug-stream example](docs/examples/debug-stream).

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

Release assets include a versioned SPDX JSON software bill of materials (SBOM)
covering the root Go module, `otelrecorder`, documentation examples, and the
Inspector's locked JavaScript dependencies. Generate and validate the same
inventory locally with `make sbom-check`; generated SBOMs are build artifacts
and are not committed.

Run `make vulncheck` to check reachable vulnerabilities in every Go module
with `govulncheck` and audit both runtime and build-time Inspector dependencies.
High or critical npm advisories fail the check; lower-severity findings remain
visible for review.

Run `make coverage` to produce atomic Go coverage profiles for every module and
the Inspector's LCOV report. CI uploads them under separate `go` and
`inspector` flags and reports their combined project coverage in the README
badge.

## License

MIT
