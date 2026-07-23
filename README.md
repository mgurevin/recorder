# recorder

[![CI](https://github.com/mgurevin/recorder/actions/workflows/ci.yml/badge.svg)](https://github.com/mgurevin/recorder/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mgurevin/recorder.svg)](https://pkg.go.dev/github.com/mgurevin/recorder)
[![Coverage](https://mgurevin.github.io/recorder/coverage.svg)](https://mgurevin.github.io/recorder/coverage/)
[![API compatibility](https://mgurevin.github.io/recorder/api-compatibility.svg)](https://mgurevin.github.io/recorder/api-compatibility/)

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
- Optional `cmd/recorder` CLI for bounded validation, summaries, conversion,
  evidence and FileBodyStore verification, deterministic fixture workflows,
  compatibility diagnostics, reconciliation, and local Inspector handoff
- Bounded HAR/NDJSON readers and deterministic network-free HTTP test fixtures
- Standard-library-only core package

## Install

```sh
go get github.com/mgurevin/recorder
```

For capture-file workflows, install the optional
[`recorder` CLI](cmd/recorder):

```sh
go install github.com/mgurevin/recorder/cmd/recorder@latest
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

if err := config.Validate(); err != nil {
	log.Fatal(err)
}

transport := recorder.NewTransport(http.DefaultTransport, rec, config)
```

`Config{}` is a deliberately minimal zero value. `DefaultConfig()` is the
recommended production baseline. Call `Validate` after applying application
settings so invalid combinations, unsupported algorithms, missing protection
providers, and malformed registrations fail during startup. Validation is
static: it performs no I/O and does not invoke callbacks or providers.

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
domains. `FileBodyStoreConfig.MaintenanceMode` opens an existing store without
creating directories, changing permissions, recovering partials, or permitting
new writers; it is intended for explicit `Open`, `Release`, and `Reconcile`
maintenance. See [DESIGN.md](DESIGN.md).

Use `recorder verify --body-store ./spool capture.har` to check referenced
assets and hashes without mutation. Use
`recorder reconcile --body-store ./spool capture.har` to preview unreferenced
committed assets; deletion requires the command's explicit authoritative apply
controls.

Custom redactors implement `BodyRedactor`. They identify sensitive values and
delegate their representation to the supplied `BodyValueProtector`, so redact,
encrypt, and tokenize modes behave identically for built-ins and extensions.
Selected plaintext is streamed into a `BodyValue`, then `FinishTo` writes the
protected bytes directly to the redactor output. Complete non-importable
examples live under [`docs/examples`](docs/examples).

## Sensitive-value protection

Selected values support three modes: fixed redaction, AES-GCM encryption, and
HMAC tokenization. Oversized values and protection failures fall back to
`[REDACTED]`. JSON protection encrypts the raw JSON token, including quotes or
container syntax, so exact reconstruction remains possible.

`ProtectionKeyProvider` and `ProtectionKeyResolver` are direct function types;
no interface adapter is required. Providers receive the request context and
are resolved at most once per protection mode and exchange; the request and
response use the same immutable key snapshot. Tokens carry a key ID, and
`ProtectedTokenKeyID`,
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

For shell-driven fixture preparation, `recorder fixture` selects reviewed
exchanges into HAR or NDJSON. `recorder serve-fixture` exposes the same strict,
network-free matching behavior through a loopback HTTP server for applications
that cannot inject an `http.RoundTripper`.

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
Use `recorder validate` for a bounded structural check, or `recorder doctor`
when schema compatibility and optional FileBodyStore health should be reported
together.

## Inspector and OpenTelemetry

The browser-only Inspector opens local HAR files and `JSONStreamRecorder`
NDJSON, supports safe body previews, trace-chain waterfalls, replay commands,
redaction audit, in-memory resolution of protected values, and fixture export
as HAR or NDJSON. It is also deployable through GitHub Pages.

`recorder inspect capture.har` validates the local file, serves it from a
temporary loopback endpoint, and opens the trusted Inspector without uploading
the capture.

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

## Command-line tools

The optional [`recorder` CLI](cmd/recorder/README.md) manages captured evidence
offline without adding dependencies to applications that only use the library:

| Workflow | Command |
| --- | --- |
| Validate, summarize, or convert bounded HAR/NDJSON input | `validate`, `summarize`, `convert` |
| Open a local capture in the browser Inspector | `inspect` |
| Check body references, hashes, schema, and local compatibility | `verify`, `doctor` |
| Select reviewed exchanges or serve a network-free test fixture | `fixture`, `serve-fixture` |
| Preview or explicitly apply FileBodyStore orphan cleanup | `reconcile` |

```sh
recorder validate capture.har
recorder summarize capture.har --json
recorder inspect capture.har
recorder fixture capture.har --method POST --host api.example.com \
  --output testdata/orders.ndjson
```

Commands use the bounded `hario` readers. They do not record traffic or replay
requests to the real network; `serve-fixture` binds a local fixture server and
`reconcile` is a dry run unless destructive application is explicitly
confirmed. See the [complete command and JSON-output contract](cmd/recorder/README.md).

## Maintenance

The repository keeps release evidence and maintenance gates in GitHub Actions:

- `make check` enforces pinned Go formatting/lint rules, race-enabled tests and
  vet across every Go module, Inspector lint/type/build/coverage checks, and
  benchmark smoke runs. CI also tests each module at its supported minimum Go
  version and at the current stable release.
- `make api-diff` compares the root, `hario`, `hartest`, and `otelrecorder`
  public APIs with their latest release tags. Unreviewed incompatible changes
  fail CI; the README badge links to the repository-hosted report.
- External wire-contract tests pin the `_recorder` JSON encoding and published
  schema identity/version. Internal AST guards preserve the intentionally small
  API design; `apidiff` handles compiler-visible compatibility.
- `make coverage-report` applies component-specific thresholds, publishes the
  weighted Go/Inspector dashboard on GitHub Pages, and retains source-level
  reports as GitHub artifacts for 14 days. No coverage data leaves GitHub.
- `make vulncheck` runs `govulncheck` for every Go module and audits Inspector
  runtime and build dependencies; high and critical npm advisories fail CI.
- `make sbom-check` generates and validates separate SPDX JSON inventories for
  the standalone Recorder module, optional `otelrecorder` module, and
  Inspector. Recorder excludes documentation examples and the other artifacts;
  release assets are provenance-attested and are not committed.

## License

MIT
