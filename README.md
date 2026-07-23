# recorder

[![CI](https://github.com/mgurevin/recorder/actions/workflows/ci.yml/badge.svg)](https://github.com/mgurevin/recorder/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mgurevin/recorder.svg)](https://pkg.go.dev/github.com/mgurevin/recorder)
[![Coverage](https://mgurevin.github.io/recorder/coverage.svg)](https://mgurevin.github.io/recorder/coverage/)

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

Its design is guided by four principles:

- Observe without interference.
- Record only verifiable facts.
- Fail without affecting HTTP behavior.
- Keep resource use and sensitive data bounded.

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
- Loopback-only `DebugStreamRecorder` and Inspector live mode for local debugging
- Bounded HAR/NDJSON readers and deterministic network-free HTTP test fixtures
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
if err := config.Validate(); err != nil {
	log.Fatal(err)
}

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

if err := config.Validate(); err != nil {
	log.Fatal(err)
}

transport := recorder.NewTransport(http.DefaultTransport, rec, config)
```

`Config{}` is a deliberately minimal zero value. `DefaultConfig()` is the
recommended production baseline. Functional transport options are not part of
the v1 API; one configuration field has one source of truth. Call `Validate`
after applying application settings so contradictory certificate flags,
unsupported algorithms and modes, missing protection providers, and malformed
redaction or decoder registrations fail during startup instead of silently
reducing the recorded evidence. Validation is static: it performs no I/O and
does not invoke user callbacks or providers.

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
if err := config.Validate(); err != nil { return err }
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

## Evidence-backed HTTP fixtures

The optional [`hario`](hario) and [`hartest`](hartest) packages turn recorder
evidence into deterministic `http.Client` tests without expanding the core
recording API:

```go
document, err := hario.ReadHAR(file, hario.DefaultReadConfig())
if err != nil { return err }

fixture, err := hartest.NewTransport(document.Log.Entries, hartest.DefaultConfig())
if err != nil { return err }

client := &http.Client{Transport: fixture}
```

`hario` reads and validates bounded HAR or streaming NDJSON input. `hartest`
strictly matches unused exchanges by method, URL/query, selected headers, and
exact body and request trailers, then returns the captured response, response
trailers, or recorded failure. One optional request normalizer handles
real-world volatile IDs, timestamps, query parameters, and semantic JSON
matching without replacing the safe matcher. It has no real-network fallback.
Incomplete or unresolved request evidence fails closed; weakening body matching
requires an explicit test configuration.

For large captures, `hario.NewHARStream` and `NewNDJSONStream` feed
`hartest.NewStreamTransport` one validated entry at a time. Capture-order tests
release consumed entries instead of retaining the complete fixture; `Verify`
drains and validates the remaining source.

Protected-value resolution is optional and source entries remain immutable.
Inspector exports preserve the original protected representation by default,
even after an operator resolves values in browser memory. A separate,
dangerous resolved-export workflow can create a clearly named derived plaintext
fixture only after showing a value-free sensitivity summary and requiring two
explicit acknowledgements. The Inspector can export all entries, explicitly
selected exchanges, or the current trace as HAR or NDJSON and reports external
or incomplete body evidence before download.

See the complete [`hario`](hario/README.md) and
[`hartest`](hartest/README.md) contracts and the non-importable
[`har-fixture` example](docs/examples/har-fixture).

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
redaction audit, in-memory resolution of protected values, and fixture export
as HAR or NDJSON. It is also deployable through GitHub Pages.

For local development, `DebugStreamRecorder` can publish finalized entries to
one Inspector window over a bounded SSE stream. Its queue retains recent
entries until an Inspector connects or reconnects and reports oldest-entry
eviction as a visible gap; it is intentionally not a durable or production
recorder. The application owns the loopback HTTP server:

```go
liveConfig := recorder.DefaultDebugStreamRecorderConfig()
// Optional: trust an exact remotely hosted Inspector origin. This permits
// JavaScript from that origin to read the local stream.
liveConfig.AllowedOrigins = []string{"https://mgurevin.github.io"}

live, err := recorder.NewDebugStreamRecorder(liveConfig)
if err != nil { return err }

server := &http.Server{
	Addr:    "127.0.0.1:7070",
	Handler: live,
}
go func() {
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Printf("debug stream: %v", err)
	}
}()

recorderConfig := recorder.DefaultConfig()
if err := recorderConfig.Validate(); err != nil { return err }
client := &http.Client{
	Transport: recorder.NewTransport(http.DefaultTransport, live, recorderConfig),
}
```

Run the Inspector locally, choose **live**, and connect to
`http://127.0.0.1:7070`. The application must shut down both the server and
recorder. `AsyncRecorder` can isolate response finalization from live-view
encoding, while `MultiRecorder` can send the same entries to an independent
file or evidence sink. Their complete lifecycle and fan-out wiring are shown in
the runnable [debug-stream example](docs/examples/debug-stream).

By default only loopback browser origins may subscribe. `AllowedOrigins`
accepts exact HTTP(S) origins—scheme, host, and non-default port—not URL paths
or wildcards. It does not relax the loopback peer restriction. Add a hosted
Inspector origin only when you trust every script served by that origin;
captured entries may contain credentials and other sensitive data.

For example, an Inspector published at
`https://mgurevin.github.io/recorder/` sends the origin
`https://mgurevin.github.io`. Configure the origin without `/recorder/`:

```go
debugStreamConfig := recorder.DefaultDebugStreamRecorderConfig()
debugStreamConfig.QueueCapacity = 1_000
debugStreamConfig.AllowedOrigins = append(
	debugStreamConfig.AllowedOrigins,
	"https://mgurevin.github.io",
)
```

This configuration supports both the Pages-hosted Inspector and a local
Inspector: loopback origins such as `http://localhost:5173` remain allowed by
default and do not need to be added. Keep the server bound to `127.0.0.1`.

> **Safari note:** Safari/WebKit may block an HTTPS Pages Inspector from
> connecting to an HTTP loopback stream as mixed content even when CORS is
> configured correctly. Use the local HTTP Inspector, or serve the loopback
> endpoint over HTTPS with a certificate trusted by the local machine.

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

Release assets include separate versioned SPDX JSON software bills of materials:
`recorder-X.Y.Z.spdx.json` covers the Go modules and documentation examples,
while `recorder-inspector-X.Y.Z.spdx.json` covers the Inspector application's
locked runtime dependencies. Generate and validate both inventories locally with
`make sbom-check`; generated SBOMs are build artifacts and are not committed.

Run `make vulncheck` to check reachable vulnerabilities in every Go module
with `govulncheck` and audit both runtime and build-time Inspector dependencies.
High or critical npm advisories fail the check; lower-severity findings remain
visible for review.

Run `make coverage-report` to produce the configured Go package/module profiles
and HTML reports, the Inspector's LCOV report, a machine-readable summary, and
the README badge. The Recorder component covers the core, `hario`, and
`hartest` libraries; every example under `docs/examples` is tested and combined
into one documented-code coverage component and one source-level HTML report,
even when it lives in an independently versioned Go module. CI enforces
per-component minimums, writes the table to the GitHub Actions job summary, and
retains the reports as a GitHub artifact for 14 days. GitHub Pages also
publishes a permanent dashboard with
links to every reported component. The displayed project percentage is
weighted across the covered Go statements and Inspector TypeScript lines; it is
not a repository-wide line percentage. No source or coverage report is uploaded
to an external coverage service. Thresholds are intentionally component-
specific: production libraries and the Inspector carry stricter gates than
copy-oriented example commands, while every example still requires a
behavioral test.

## License

MIT
