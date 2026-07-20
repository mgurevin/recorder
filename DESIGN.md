# DESIGN

Technical design of `github.com/mgurevin/recorder`: architecture, invariants,
trade-offs, concurrency model, and observability limits. Usage and
configuration live in [README.md](README.md); this document explains *why*
the library behaves the way it does.

## 1. Overview

The library records the complete life cycle of `net/http` client exchanges —
including calls that fail at the DNS/TCP/TLS/context/body layer — and exports
them as HAR 1.2 documents with structured `_`-prefixed extensions.

It is implemented as an `http.RoundTripper` **wrapper**: it never
re-implements transport behavior, it observes a base transport
(`http.DefaultTransport` when none is given) through `httptrace` callbacks
and stream tees.

Two goals dominate every decision:

1. **Observe without changing caller-visible behavior.** The wrapped call
   must be indistinguishable from an unwrapped one.
2. **Record only what was actually observed.** Unmeasurable values are `-1`
   or omitted — never estimated, never defaulted to a plausible guess.

The core package uses the standard library only. Integrations that need
dependencies (OpenTelemetry, the inspector UI) are separate modules/tools
(§15).

## 2. Architecture

```text
http.Client
  -> recorder.Transport            (RoundTripper wrapper)
       -> traceCollector           (httptrace events, mutex-guarded)
       -> request bodyCapture      (tee on the caller-supplied body)
       -> response bodyCapture     (tee on resp.Body)
       -> exchange state machine   (created ... completed/failed/closed_early)
       -> Entry builder            (immutable HAR entry + extensions)
       -> Recorder sink            (memory / HAR file / NDJSON / callback)
```

| Component | Responsibility |
| --- | --- |
| `Transport` | Wires everything per `RoundTrip`; holds frozen `Options`, `redactor`, `BodyStore` |
| `exchange` | Per-call state: IDs, timestamps, response snapshot, finalization |
| `traceCollector` | Collects httptrace events tolerantly (order/duplication/concurrency) |
| `bodyCapture` | Tee: counts always, hashes the full stream, stores content up to a limit |
| `redactor` | Applies immutable redaction rules during body capture and entry construction |
| `BodyStore` | Pluggable content storage (`MemoryBodyStore`, `FileBodyStore`) |
| `Recorder` | Sink interface (`Record(*Entry)`); receives finalized entries only |
| `TraceStore` | Optional capability on retaining recorders: query/remove/take by `_traceId` |

## 3. Entry lifecycle

Each exchange advances through a state machine:

`created` → `request_started` → `request_headers_written` →
`request_body_streaming` → `response_headers_received` →
`response_body_streaming` → terminal state.

Only terminal states appear in exports, because an entry is emitted exactly
once, at finalization (`sync.Once`):

| Trigger | Terminal state |
| --- | --- |
| response body `Read` returns `io.EOF` | `completed` |
| response body `Read` returns another error | `failed` (`_error.phase = read_response_body`) |
| caller `Close`s the body before EOF | `closed_early` |
| response cannot carry a body (HEAD, 1xx/204/304, explicit `Content-Length: 0`) | `completed`, finalized at `RoundTrip` time |
| `RoundTrip` returns an error | `failed` |

A body that is never read and never closed produces **no entry**. Finalizers
are deliberately not used: GC timing is unpredictable, and an unclosed body
is a caller bug that leaks the connection in plain `net/http` anyway.

## 4. Caller behavior invariants

- The original `*http.Request` is never mutated: the trace context and body
  wrapper attach to a `req.Clone(ctx)`.
- Response byte/EOF/error semantics pass through verbatim; the tee only
  observes. No double reads, no extra buffering beyond the capture writer.
- Recorder-internal failures (store errors, recorder panics) are contained
  with `recover`, reported through `OnInternalError`, and optionally logged.
  They never replace or alter the wrapped HTTP call's response or error.
- Redaction affects the recorded copy only.
- Once built, an `Entry` is an immutable snapshot; no component mutates it
  after `Recorder.Record`.

## 5. Body capture design

`bodyCapture` separates three concerns:

- **Count always** — `totalBytes` and the HAR size fields reflect the real
  stream even when content capture is off or past the limit.
- **Capture up to the limit** — content goes to the `BodyStore` until
  `Max*BodyBytes`; a store failure stops content capture only, never the
  HTTP flow (reported through the internal-error policy).
- **Hash the full stream** — truncation does not affect the hash; the hash
  is only emitted when the stream completed (a partial hash would mislead).

When JSON/XML rules apply, capture inserts a bounded streaming redactor
before the `BodyStore`; raw matching values therefore never reach memory or
file stores. Parser-limit, unsupported-encoding, and decoder failures stop
store capture rather than falling back to the original bytes. Counting and
hashing still observe the original caller/wire stream.

`EmbedBodies` is a separate decision from capture: content can be captured
into a `FileBodyStore` yet kept out of the HAR document (sizes, hashes,
truncation state and the store path remain). `MemoryBodyStore` pre-sizes its
buffer from a Content-Length-derived hint, clamped both to the capture limit
and to a hard pre-allocation cap — a lying `Content-Length` wastes bounded
memory and never breaks capture.

Two request-side subtleties:

- With a known `Content-Length`, `http.Transport` reads exactly N bytes and
  may never issue the final `Read` returning EOF. A stream closed after
  exactly the announced length therefore counts as `complete`.
- When the transport replays the body via `GetBody` (internal retry), the
  capture **resets**: the record reflects the bytes of the attempt that
  actually went out. Only the final attempt is recorded.

## 6. HAR entry building

- Plain HAR 1.2 plus `_`-prefixed extensions; stripping every extension
  leaves a valid document (tested with an independent map-based validator).
- Exchanges that failed before a response existed record
  `response.status = 0` (consistent with browser exports); detail lives in
  `_error`.
- Deterministic export: struct field order is fixed, header lists are
  sorted (snapshot fallback) or wire-ordered (when observed), entries are
  stable-sorted by start time.
- Honesty over completeness:
  - `headersSize = -1` in both directions — actual wire header bytes
    (transport-added fields, HPACK) are not observable here.
  - Transparent gzip: `bodySize = -1`, `content.size` = decoded size,
    `content._decoded = true`. Wire body ≠ decoded body ≠ stored body, and
    the record says which is which.
  - `request.httpVersion` is filled only from facts: the response's
    negotiated protocol, this exchange's TLS ALPN result, or cleartext
    through `*http.Transport` (which never speaks h2c). Requests that never
    reached the wire record an empty version.
  - `serverIPAddress` is written only when the peer address parses as an IP
    and no proxy is in play (§7).

## 7. httptrace and observability

The collector subscribes to: `GetConn`/`GotConn`, `DNSStart`/`DNSDone`
(including `Coalesced`), `ConnectStart`/`ConnectDone`,
`TLSHandshakeStart`/`TLSHandshakeDone`, `WroteHeaderField`,
`WroteHeaders`/`WroteRequest`, `Wait100Continue`/`Got100Continue`,
`Got1xxResponse` (bounded to 16 per exchange), `GotFirstResponseByte`,
`PutIdleConn`.

Handling rules:

- Callbacks may arrive from transport-internal goroutines, out of order, and
  more than once (Happy-Eyeballs parallel dials, retries). Every path locks
  the collector mutex; each step keeps its **first start** and **last
  successful completion**; failed events are stored separately; a missing
  event is never an error.
- A `GotConn` without a `Conn` carries no information and does not erase an
  earlier complete event.
- `request.headers` prefers the fields actually written to the wire
  (`WroteHeaderField`, wire order, transport-added fields and HTTP/2
  pseudo-headers included); the caller's header snapshot plus `Host` is the
  fallback for exchanges that failed before the request line.
- **Reused connections** (HTTP/2 multiplexing included): no
  DNS/connect/TLS events fire; the TLS state comes from `resp.TLS` instead
  of the handshake callback.
- **HTTP/2 limits:** the stream ID is not observable; the `connection` field
  is the local port.
- **Proxy limits:** the TCP peer is the proxy, so the origin IP, origin DNS
  and origin connect timings are unobservable client-side. `_network`
  describes the proxy connection. For a standard `*http.Transport`, recorder
  wraps a clone of the transport and captures the selected proxy URL from the
  callback invocation the transport already performs; the callback is never
  evaluated a second time merely to collect metadata. The captured URL is
  redacted before export. Custom RoundTrippers fall back to the observed
  dial target (`host:port`) because they expose no proxy-selection hook.
- **`PutIdleConn` is best-effort:** the pool return races with entry
  finalization (both happen around body EOF), so `_network.putIdle` absence
  means "not observed", not "did not happen".
- Custom base `RoundTripper`s may fire no httptrace events at all; every
  trace-derived field degrades to absent/`-1`, and error classification
  falls back accordingly.

## 8. Timing semantics

Mapping onto HAR `timings` (milliseconds, `-1` = not observed / not
applicable):

- `blocked`: request start → first network activity (or → `GotConn` on a
  reused connection, covering the wait for the pool).
- `dns`, `connect`, `ssl`: the respective event pairs; `connect` **excludes**
  TLS (`ConnectDone` fires at TCP completion; TLS is its own phase).
- `send`: `GotConn` → `WroteRequest` (falling back to `WroteHeaders`).
- `wait`: request written → `GotFirstResponseByte`.
- `receive`: first byte → finalization.
- Reused connections report `-1` for dns/connect/ssl — not a misleading 0.
- Clock skew between goroutines is clamped to 0; negative durations are
  never produced.

## 9. Error classification

Type-first, in `classifyPhase` order:

1. A recorded request-body read error → `write_request_body`.
2. `errors.As(*net.DNSError)` → `dns`.
3. Typed TLS/x509 errors → `tls` (package-prefix type-name matching covers
   unexported types like `http.tlsHandshakeTimeoutError`).
4. `*net.OpError`: `dial` → `connect`/`proxy`; `write` →
   `write_request[_body]`; `read` → by trace progress.
5. `syscall.ECONNREFUSED` → `connect`/`proxy`.
6. Context errors: the network step in flight if any (dns/connect/tls),
   otherwise `context` — **but only when the request's own context actually
   fired**. Transport-internal timeouts (`ResponseHeaderTimeout`)
   deliberately match `context.DeadlineExceeded` via `errors.Is` and fall
   through to the trace-derived phase; the `contextCanceled` /
   `contextDeadlineExceeded` flags are gated the same way. `context.Cause`
   is recorded when available.
7. String matching is a last resort, only for `http.Client`'s untyped
   redirect-loop error.
8. Fallback: phase derived from httptrace progress.

The unwrap chain is depth-capped (32) against self-unwrapping errors.
Timeout/temporary flags are probed across the chain. Request-body failures
(`write_request_body`, detected via the tee's recorded read error) are
distinct from response-body failures (`read_response_body`, the only phase
assigned directly rather than classified); an early `Close` by the caller is
state, not error.

## 10. Redaction design

- Header/query/cookie redaction by case-insensitive name, applied while
  converting to HAR pairs — live objects are untouched. Cookies are also
  redacted when their carrier header is.
- JSON and XML redaction are byte-preserving streaming state machines placed
  before the BodyStore. JSON buffers only the current object key; XML buffers
  only the current markup token. Matched values/subtrees are suppressed, so
  raw secrets are never written and no finalize-time rewrite is needed.
- Parser buffers are capped at 64 KiB, nesting at 1024, and generic MIME
  sniffing at 4 KiB. A limit violation stops capture rather than falling back
  to raw bytes. Large matched values themselves are never buffered.
- A match remains redacted if the later document is malformed or truncated;
  streaming output cannot safely roll back. Namespaces, prefixes, attributes,
  formatting and other unmatched bytes survive byte-for-byte. XML attribute
  values are not redacted.
- `RedactErrorMessage` filters every recorded error string (errors can embed
  URLs and credentials), including raw httptrace event details.
- Hashes cover the original wire/caller bytes, never redacted bytes: the
  hash is a content fingerprint, not a record of the redacted view.

## 11. Compression and decoding

- Transparent gzip by `http.Transport`: the tee sees decoded bytes;
  `_decoded: true`, `bodySize = -1`.
- Record-time decoding: when the caller negotiated compression, a registered
  `ContentDecoder` feeds the streaming redactor and BodyStore while
  `bodySize`/hash/counters keep the wire view.
- Default decoders: `gzip`, `x-gzip`, `deflate` (zlib-wrapped or raw,
  header-sniffed like browsers) — stdlib only. Brotli/zstd are not bundled;
  `WithContentDecoder` is the hook.
- Safety: with structured redaction active, unknown/multi-step encodings and
  decoder failures stop store capture instead of persisting raw bytes.
  Decoded output is bounded by `MaxResponseBodyBytes`.

## 12. Recorder and sink model

`Recorder` is deliberately minimal — one method, `Record(*Entry)` — so a
custom sink is trivial. Built-ins: `MemoryRecorder`, `HARFileRecorder`
(atomic temp-file + rename on flush), `JSONStreamRecorder` (NDJSON, one
entry per line; the enclosing HAR wrapper is intentionally not emitted so no
top-level JSON document is ever half-written), `RecorderFunc`.

Per-trace access is an **optional capability** (`TraceStore`), discovered by
type assertion, implemented by the retaining recorders. `TakeTrace` removes
and returns under one lock, so an entry finalized concurrently cannot fall
between a query and a separate delete. `JSONStreamRecorder` intentionally
does not implement `TraceStore`: entries leave the process on `Record`.

**Recommended extension design (not implemented):** a production spool sink
would follow these principles — a bounded queue between finalization and
I/O; an explicit backpressure policy (drop-oldest or drop-newest, never
block the HTTP call); NDJSON entries rather than full HAR documents; local
spool file rotation with gzip after rotation; archival, signing and
timestamping handled by ops tooling outside the core library.

## 13. Concurrency model

Lock/ownership map:

| Synchronization | Protects |
| --- | --- |
| `Transport.initOnce` | one-time construction of redactor/store; `Options` frozen afterwards |
| `traceCollector.mu` | all httptrace event state; `view()` returns a deep-enough snapshot |
| `bodyCapture.mu` | counters/hash/writer — the transport's write loop may still stream the request body while the exchange finalizes |
| `exchange.mu` | life-cycle state and the response snapshot (`respSnapshot` cloned before `RoundTrip` returns) |
| `exchange.finalizeOnce` | exactly-once finalization from Read-EOF / read-error / Close / transport-error paths |
| recorder mutexes | each built-in recorder guards its own state |

`Transport` fields and `Options` must not be mutated after the first
request. Entries are immutable after emission, so recorder consumers need no
further synchronization. `go test -race ./...` covers concurrent client use,
concurrent trace draining, and the body-wrapper/finalization races.

## 14. Performance model

- Per-request overhead is a few microseconds on top of `net/http` itself;
  current benchmarks add roughly 60–80 allocations/op over the baseline
  depending on capture mode (compare `BenchmarkBaselineNoRecorder` with
  `BenchmarkCaptureDisabled`/`BenchmarkHeaderOnlyCapture`/
  `BenchmarkSmallBody` under `-benchmem`). Exact numbers shift with the
  benchmark setup and the Go runtime; the stable takeaway is that the
  network round trip dominates in practice.
- Body capture allocation is bounded by the capture limit; the memory store
  pre-sizes from Content-Length to avoid growth re-copies. Embedding adds
  copies at entry-build time (store read-back + string conversion).
- **Hashing is the CPU ceiling on large streams** — SHA-256 runs at hardware
  speed and everything past the capture limit is hash+count only. Disable
  `HashBodies` when fingerprints aren't needed and throughput matters.
- For large-body production use: `EmbedBodies(false)` + `FileBodyStore` +
  low capture limits (counting stays accurate past the limit).
- Benchmarks run over an in-memory `net.Pipe` listener — no OS sockets, no
  ephemeral-port churn — so they measure recorder overhead, not kernel
  networking. `net.Pipe` is unbuffered; absolute MB/s numbers are not
  comparable to TCP loopback, relative differences are what matter.

## 15. Extension packages and tools

- **`otelrecorder/`** — a separate Go module exporting finished entries as
  OTel span events and metrics via `WithOnEntryCompleted`. The core has no
  OTel dependency. The adapter enforces cardinality constraints by
  construction: no URLs beyond scheme+host, no header/cookie/body material,
  status *class* labels, opt-in span-event-only correlation IDs, clamped
  string values, capped custom attribute lists.
- **`inspector/`** — a standalone React + TypeScript viewer for the produced
  HAR files (separate npm project, not part of the Go build or runtime).

## 16. Known limitations

- A response body that is never read and never closed produces no entry.
- Wire header byte sizes are not observable (`headersSize = -1`).
- The origin IP (and origin DNS/connect timing) is not observable through a
  proxy.
- The HTTP/2 stream ID is not observable; `connection` is the local port.
- On internal transport retries via `GetBody`, only the final attempt's body
  is recorded.
- Brotli/zstd decoders are not bundled (dependency-free core); register
  them via `WithContentDecoder`.
- XML attribute values are not redacted (only matched element subtrees).
- `_network.putIdle` is best-effort (finalization race).
- A custom base `RoundTripper` may fire no httptrace events; trace-derived
  fields degrade to absent/`-1`.
- `http.Client.Timeout` firing mid-body surfaces as `read_response_body` —
  a correct observation, though the cancellation originates from the client
  timer.

## 17. Testing strategy

- **Unit tests** for timing computation, error classification, redaction
  (including XML byte-preservation), collector event semantics
  (duplicates, out-of-order, bounds), body-capture accounting, and the
  trace-store operations.
- **Integration tests** over `httptest.Server` (plain, TLS, HTTP/2), raw TCP
  listeners (malformed responses, handshake stalls), self-signed
  certificates, custom dialers and broken `RoundTripper`s — covering DNS,
  refused connections, timeouts, cancellation, redirects and loops, gzip,
  chunked, trailers, proxies, truncation, storage failures.
- **HAR validation**: exports are checked with an independent map-based
  validator, then every `_` extension is stripped and the remainder is
  re-validated as plain HAR 1.2.
- **Race coverage**: `go test -race ./...` includes concurrent clients,
  concurrent per-trace draining, and recorder panics.
- **Fuzz targets**: `FuzzRedactJSON`, `FuzzRedactXML`, `FuzzRedactURL`,
  `FuzzQueryPairs`, `FuzzHeaderPairs`, `FuzzContentClassification`,
  `FuzzUnwrapChain`, `FuzzHARSerialization`.
- **Benchmarks**: baseline (no recorder), capture off, header-only, small
  body, 1 MiB, 100 MB streaming with and without hashing, ~1000 concurrent
  requests — all over the in-memory network (§14).
- The `otelrecorder` module has its own suite against in-memory OTel SDKs
  (span/metric shapes, secret-leak checks, race); the `inspector` app has
  vitest unit tests for its parser/formatters plus a TypeScript build gate.
