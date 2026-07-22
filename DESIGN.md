# DESIGN

Technical design of `github.com/mgurevin/recorder`: architecture, invariants,
trade-offs, concurrency model, and observability limits. Usage and
configuration live in [README.md](README.md); this document explains *why*
the library behaves the way it does.

## 1. Overview

The library records the complete life cycle of `net/http` client exchanges —
including calls that fail at the DNS/TCP/TLS/context/body layer — and exports
them as HAR 1.2 documents with one versioned `_recorder` extension.

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
| `Transport` | Wires everything per `RoundTrip`; holds frozen `Config`, `redactor`, `BodyStore` |
| `exchange` | Per-call state: IDs, timestamps, response snapshot, finalization |
| `traceCollector` | Collects httptrace events tolerantly (order/duplication/concurrency) |
| `bodyCapture` | Tee: counts always, hashes the full stream, stores content up to a limit |
| `redactor` | Selects immutable built-in/custom redaction rules during capture and entry construction |
| `RedactionConfig` | Defines common and direction-specific selectors plus explicit MIME redactor overrides for a Transport or request context |
| `BodyCapturePolicy` | Freezes per-direction capture/embed/hash/limit/redactor decisions for each exchange |
| `HeadSamplingPolicy` | Selects full, metadata-only, or uninstrumented passthrough before exchange setup |
| `RetentionPolicy` | Keeps or discards a finalized entry after the completion callback |
| `BodyStore` | Pluggable content storage (`MemoryBodyStore`, `FileBodyStore`) |
| `ReleaseEntryAssets(*Entry)` | Structurally discovered store capability used to clean assets for safely discarded entries; intentionally not a public interface |
| `Recorder` | Sink interface (`Record(*Entry) error`); receives finalized entries only |
| `TraceStore` | Optional capability on retaining recorders: query/remove/take by `_recorder.traceId` |

## 3. Entry lifecycle

Each exchange advances through a state machine:

`created` → `request_started` → `request_headers_written` →
`request_body_streaming` → `response_headers_received` →
`response_body_streaming` → terminal state.

Only terminal states appear in exports, because an entry is emitted exactly
once, at finalization (`sync.Once`):

At `RoundTrip` entry, identity resolution consumes the redirect index once and
head sampling runs before recorder initialization, request cloning,
`httptrace`, exchange IDs, redactor/audit clones, body wrappers, or capture
policies. `HeadSampleDrop` calls the original base transport with the original
request and creates no entry; later HTTP errors are intentionally unavailable.
`HeadSampleMetadataOnly` constructs normal lifecycle/timing instrumentation but
forms a hard ceiling over optional content: no body capture/hash/embed,
headers, cookies, query pairs, raw trace, or certificates. A body policy cannot
relax that ceiling.

Rate sampling prefers an application sampling key from context, then the trace
ID. Stable keys use a fixed dependency-free FNV-1a accumulation followed by a
full-width avalanche finalizer, making prefixed/sequential keys well distributed
while keeping the decision reproducible and redirect-consistent. Without either
key, each physical exchange uses crypto-random selection and redirect
consistency is not claimed.
Policy panics and invalid decisions fail open to full recording; unlike body
policy failure, there is no untrusted body-selection result that must fail
closed.

| Trigger | Terminal state |
| --- | --- |
| response body `Read` returns `io.EOF` | `completed` |
| response body `Read` returns another error | `failed` (`_recorder.error.phase = read_response_body`) |
| caller `Close`s the body before EOF | `closed_early` |
| response cannot carry a body (HEAD, 1xx/204/304, explicit `Content-Length: 0`) | `completed`, finalized at `RoundTrip` time |
| `RoundTrip` returns an error | `failed` |

### From `http.Client.Do` to a finalized entry

The caller-visible flow is deliberately explicit:

1. If the recorder's wrapped `RoundTripper` returns an error, the exchange is
   finalized immediately as `failed`; there is no response-body lifecycle to
   wait for. DNS, connect, proxy, TLS, request-write, response-header and
   context failures therefore reach `Recorder.Record` before `Do` returns.
2. If `Do` returns `err == nil` with a body-bearing response, the entry is
   **not finalized yet**. `RoundTrip` has only observed response headers; the
   caller controls when and how the body stream advances.
3. Reading `resp.Body` until a `Read` returns `io.EOF` finalizes a `completed`
   entry. This is the path that gives body capture, hashing and inspection the
   complete stream. Merely receiving `n == len(Content-Length)` is not the
   recorder's completion signal; the wrapper observes the stream's EOF.
4. Calling `resp.Body.Close()` before EOF still finalizes exactly one entry,
   but its state is `closed_early`. Only the prefix actually read by the caller
   was available for capture. The body is incomplete and a full-stream hash is
   not emitted.
5. A non-EOF body read error finalizes a `failed` entry with
   `_recorder.error.phase = read_response_body` and the bytes observed before the error.
6. Responses that cannot carry a body are finalized as `completed` before
   `RoundTrip` returns, so they do not require a synthetic read or close to
   create the entry.

The recommended success path is therefore: check `err`, defer `Close`, and
consume the body to EOF when a complete body record is required:

```go
resp, err := client.Do(req)
if err != nil {
	// A RoundTripper-level failure has already finalized its failed entry.
	return err
}
defer resp.Body.Close()

_, err = io.Copy(io.Discard, resp.Body) // drives the recorder to EOF
return err
```

`http.Client.Do` can also produce errors above the `RoundTripper` boundary,
notably `CheckRedirect` policy errors. The recorder stores physical exchanges
seen by its Transport; it cannot attach a client-layer error that the wrapped
`RoundTripper` never received to an entry's `_recorder.error` field.

### What leaks when the response body is abandoned

A body that is neither consumed nor closed produces **no entry**. This is not
used as an implicit sampling mechanism; it is an incomplete caller lifecycle.
There is intentionally no GC finalizer because collection time is
unpredictable and cannot safely define recording semantics.

The resources retained depend on how far the body progressed:

- The underlying `net/http` response stream remains unfinished. Its TCP/TLS
  connection generally cannot return to the idle pool for reuse, leaving the
  client socket and corresponding server/proxy resources occupied until some
  external timeout or close releases them. Repeated abandonment can cause
  connection churn, file-descriptor pressure and idle-pool starvation.
- The response body and recorder wrapper retain the per-exchange state,
  response/trace snapshots and any captured prefix. `Recorder.Record`,
  `OnEntryCompleted`, HAR export and OTel export are never invoked for that
  exchange because finalization never occurs.
- If no body byte was ever read, the BodyStore writer and streaming redactor
  are normally not opened yet. The network response and exchange state still
  remain unfinished.
- If some bytes were read before abandonment, an opened `MemoryBodyStore` may
  retain its buffer; an opened `FileBodyStore` may retain both its partial file
  and an open file descriptor; hashing/parser/protection state may retain its
  bounded working buffers. The store commit/abort and redactor close path and final audit
  report do not run.
- For encoded structured bodies, record-time decoding uses a backpressured
  worker. Once partial reading has started that worker can remain blocked on
  the abandoned stream, retaining its goroutine, pipe, decoder and bounded
  redactor buffers until the body is closed or the stream otherwise fails.

Calling `Close` is therefore the minimum cleanup requirement, but it only
produces a `closed_early` record when EOF was not observed. Consuming to EOF
and then closing is the normal path for connection reuse and a complete,
inspectable body record.

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

When a built-in or custom body rule applies, capture inserts the selected
streaming redactor before the `BodyStore`; raw matching values therefore never
reach memory or file stores. One redactor is selected per body, opened once,
fed each input byte once, and closed once. The entry builder reuses that stored
representation instead of redacting it again. Parser-limit,
unsupported-encoding, decoder, and custom-redactor failures stop store capture
rather than falling back to the original bytes. Counting and hashing still
observe the original caller/wire stream.

At exchange creation, the request-scoped `RedactionConfig` is read once from
the original request context. Its common and direction-specific rules are
independently unioned with the Transport's immutable `RedactionConfig`, then
cloned with the exchange audit.
Input collections were copied when attached to the context, so caller mutation
cannot race with recording. Context hints follow redirects; every hop resolves
its own immutable pair from the inherited context. Concurrent requests sharing
one Transport never mutate or share request-scoped rule maps.

The optional request-scoped comment is also snapshotted at exchange creation
and written verbatim to the standard HAR `entry.comment` field. It follows
redirect contexts but never enters the recorder extension or redaction
pipeline. The caller therefore owns its sensitivity and must not use comments
for secrets or personal data.

Sensitive-value protectors are likewise cloned and bound to the original
request context once per exchange. The function-typed `ProtectionKeyProvider`
receives that context for optional request-scoped key selection while the
configured provider remains immutable and concurrency-safe. Protected tokens
authenticate and embed a non-secret key ID. Trusted archive tooling can inspect
that ID and resolve historical encryption/tokenization keys through the
function-typed `ProtectionKeyResolver`; resolver state is external to HAR data
and the recorder.

Request-scoped name selectors are additive and therefore cannot remove
default/global header, query, cookie, JSON, or XML protection. An explicit
request-scoped custom body redactor overrides a global registration or built-in
selection for the same normalized base MIME;
`BodyCaptureDecision.RedactorOverride` remains the final, per-body override. This
keeps capture policy responsible for capture decisions while context hints stay
declarative and local to the request construction site.

An optional `BodyCapturePolicy` runs once for the request and once after
response headers arrive. It receives the global capture decision as input and
can override capture, embedding, hashing, the limit, or the selected body
redactor through `RedactorOverride`. A nil override preserves the effective
Transport/request-scoped `RedactionConfig` selection. The
resolved decisions are stored on `exchange`; entry construction never
re-evaluates the policy. Policy errors and panics select a zero, metadata-only
decision and enter the normal internal-error path.

`EmbedBodies` is a separate decision from capture: content can be captured
into a `FileBodyStore` yet kept out of the HAR document (sizes, hashes,
truncation state and an opaque store reference remain). `MemoryBodyStore` pre-sizes its
buffer from a Content-Length-derived hint, clamped both to the capture limit
and to a hard pre-allocation cap — a lying `Content-Length` wastes bounded
memory and never breaks capture.

`BodyWriter` has explicit transactional `Commit` and `Abort` outcomes rather
than an ambiguous `Close`. File capture starts under `partial/`; successful
normal, truncated, read-error, or closed-early finalization closes and
atomically renames it into `assets/` (and optionally syncs it). Retry reset and
processing/storage errors abort it. References are opaque and remain empty
until commit succeeds.
Committed assets transfer to application ownership and are removed only by
explicit release or reconciliation against an authoritative live-ref set.
This avoids invalidating exported HARs through implicit age eviction. Byte and
file quotas bound store ownership; exhaustion stops content capture without
affecting HTTP bytes. Startup recovery removes expired partials but preserves
committed assets.

Finalization first invokes `OnEntryCompleted`, which borrows the immutable
entry and its assets only until the callback returns. Tail retention then runs;
it cannot recover capture cost. Kept entries transfer to `Recorder.Record`.
Discarded entries are removed only after a store exposing
`ReleaseEntryAssets(*Entry)` successfully
releases referenced assets. Missing capability, cleanup failure, policy panic,
or an invalid decision fails open to Recorder delivery so the library does not
silently orphan external content. Callback, retention, and Recorder panics are
contained independently.

One store root has single-process ownership. Applications must not open the
same root from multiple processes concurrently; use a distinct root per
process or coordinate ownership outside the library. The in-process mutex
serializes reservations, release, and reconciliation but is not a filesystem
lock.

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
  `_recorder.error`.
- Deterministic export: struct field order is fixed, header lists are
  sorted (snapshot fallback) or wire-ordered (when observed), entries are
  stable-sorted by start time.
- Honesty over completeness:
  - `headersSize = -1` in both directions — actual wire header bytes
    (transport-added fields, HPACK) are not observable here.
  - Transparent gzip: `bodySize = -1`, `content.size` = decoded size,
    `_recorder.responseBodyDecoded = true`. Wire body ≠ decoded body ≠ stored body, and
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
  and origin connect timings are unobservable client-side. `_recorder.network`
  describes the proxy connection. For a standard `*http.Transport`, recorder
  wraps a clone of the transport and captures the selected proxy URL from the
  callback invocation the transport already performs; the callback is never
  evaluated a second time merely to collect metadata. The captured URL is
  redacted before export. Custom RoundTrippers fall back to the observed
  dial target (`host:port`) because they expose no proxy-selection hook.
- **`PutIdleConn` is best-effort:** the pool return races with entry
  finalization (both happen around body EOF), so `_recorder.network.putIdle` absence
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

- `BodyRedactor` is the common streaming contract for built-ins and extensions.
  Exact normalized base-MIME custom registrations take precedence over a
  built-in for the same type; the last registration wins. Generic JSON/XML
  sniffing is itself selected through the same lifecycle.
- Every `BodyRedactor` receives a body-scoped `BodyValueProtector`. Custom
  parsers select values but never handle keys or construct tokens. Each selected
  value is one library-owned streaming/bounded `BodyValue`; the central
  protector applies redact/encrypt/tokenize mode, bounded fail-closed behavior,
  token formats, failure propagation, replacement counts, and protection audit
  counts for built-ins and extensions alike.
- Every selected writer is opened once, receives the body stream once, and is
  closed once. Captured content is marked already redacted, so later HAR
  embedding never invokes a second redactor.
- Redactor constructor/write/close errors, panics, nil writers, and short writes
  stop capture and enter the internal-error path without changing the caller's
  HTTP bytes or error. The destination writer is not closable by extensions.
- Header/query/cookie redaction by case-insensitive name, applied while
  converting to HAR pairs — live objects are untouched. Cookies are also
  redacted when their carrier header is.
- JSON, XML, URL-encoded form, and multipart form redaction are streaming
  state machines placed before the BodyStore. JSON/form processing buffers
  only the current key, XML the current markup token, and multipart the
  current part headers plus a boundary-sized lookbehind. Matched
  values/subtrees/parts are suppressed, so raw secrets are never written and
  no finalize-time rewrite is needed.
- JSON accepts an initial UTF-8 BOM and resets after each complete top-level
  value so every NDJSON document is redacted. XML suppression tracks element
  names, so a mismatched end tag cannot expose the remainder of a matched
  subtree.
- Form field names use the same percent-decoding and case-insensitive matching
  as query parameters. Only explicit `application/x-www-form-urlencoded`
  content is treated as a form; generic text sniffing remains JSON/XML-only.
- Multipart fields use the same case-insensitive query rules. Matching file
  parts redact both payload and filename. Invalid/ambiguous part headers and
  unmatched nested multiparts fail closed; matching outer nested parts are
  safely suppressed as one payload.
- Parser buffers are capped at 64 KiB, nesting at 1024, and generic MIME
  sniffing at 4 KiB. A limit violation stops capture rather than falling back
  to raw bytes. Redaction suppresses matched values immediately; encryption
  buffers only the current matched value up to its configured 64 KiB default
  (16 MiB hard ceiling); tokenization feeds the matched bytes directly into
  HMAC without retaining them.
- A match remains redacted if the later document is malformed or truncated;
  streaming output cannot safely roll back. Namespaces, prefixes, attributes,
  formatting and other unmatched bytes survive byte-for-byte. XML attribute
  values are not redacted.
- `RedactErrorMessage` filters every recorded error string (errors can embed
  URLs and credentials), including raw httptrace event details.
- Hashes cover the original wire/caller bytes, never redacted bytes: the
  hash is a content fingerprint, not a record of the redacted view.
- Each exchange owns a mutex-protected redaction audit collector. Redactor
  clones carry a fixed request/response direction, so concurrent body
  streaming and finalization cannot misattribute counts.
- `_recorder.redaction` snapshots changed recorded values and body-redactor outcomes.
  Finishing a central `BodyValue` records one replacement; there is no separate
  reporter or parser-owned counter that can double-count it. Every successful
  built-in or custom redactor with no finished values reports `unchanged`. The
  audit never stores rule names, original values, concrete Go types, or error
  text.
- The sensitive-value protector is independent of the format parsers. Redact,
  AES-256-GCM encryption, and HMAC-SHA-256 tokenization are mutually exclusive
  sinks for one matched value. Versioned tokens carry a non-secret base64url
  key ID. Encryption authenticates that ID as AAD and uses a fresh random
  96-bit nonce. Every key, randomness, size, and crypto failure maps to the
  literal `[REDACTED]`; no parser has a plaintext fallback path.

## 11. Compression and decoding

- Transparent gzip by `http.Transport`: the tee sees decoded bytes;
  `_recorder.responseBodyDecoded: true`, `bodySize = -1`.
- Record-time decoding: for an explicitly encoded request or response, a
  registered `ContentDecoder` feeds the streaming redactor and BodyStore while
  hashes/counters keep the encoded-byte view; response `bodySize` remains the
  wire view.
- Default decoders: `gzip`, `x-gzip`, `deflate` (zlib-wrapped or raw,
  header-sniffed like browsers) — stdlib only. Brotli/zstd are not bundled;
  `Config.ContentDecoders` is the hook. The independently pinned
  [`docs/examples/content-decoders`](docs/examples/content-decoders/) module
  provides complete registrations and end-to-end tests for both encodings.
- Safety: with body redaction active, unknown/multi-step encodings and
  decoder failures stop store capture instead of persisting raw bytes.
  Decoded output is bounded by the resolved request/response decision's
  directional body limit.

## 12. Recorder and sink model

`Recorder` is deliberately minimal — one method, `Record(*Entry) error` — so a
custom sink is trivial and synchronous persistence failures cannot be silently
lost. Built-ins: `MemoryRecorder`, `HARFileRecorder`
(atomic temp-file + rename on flush), `JSONStreamRecorder` (NDJSON, one
entry per line; the enclosing HAR wrapper is intentionally not emitted so no
top-level JSON document is ever half-written), `RecorderFunc`.

`MemoryRecorder` is a capped recent-history store, not an unbounded slice. Its
default constructor retains the newest 1,024 finalized entries; a custom
positive capacity can be selected explicitly. The fixed-size ring makes
steady-state `Record` O(1), releases the evicted pointer immediately, and
exposes a monotonic eviction count. `Snapshot` reads the logical oldest-to-
newest sequence and retention statistics under one lock. Entry capacity does
not bound embedded body bytes, so capture limits remain part of the memory
budget. An eviction may make a trace partial; retaining trace IDs to track that
fact would itself create an unbounded index, so callers requiring complete
evidence must use a durable sink.

Per-trace access is an **optional capability** (`TraceStore`), discovered by
type assertion, implemented by the retaining recorders. `TakeTrace` removes
and returns under one lock, so an entry finalized concurrently cannot fall
between a query and a separate delete. `JSONStreamRecorder` intentionally
does not implement `TraceStore`: entries leave the process on `Record`.

`AsyncRecorder` is the bounded in-memory delivery decorator. It uses one FIFO
worker so accepted entry order remains stable. Its default `AsyncBlock` policy
favors evidence preservation: a full queue applies backpressure to `Record`
instead of silently dropping an entry. Because finalization occurs from response
body `Read`/`Close`, that choice can increase application latency or accumulate
blocked goroutines when the downstream sink stalls. `AsyncDropNewest` and
`AsyncDropOldest` are explicit availability-over-completeness alternatives;
both expose drop counters and can make a trace chain incomplete.

`DebugStreamRecorder` is a separate local-development sink. It implements both
`Recorder` and `http.Handler`, but never opens a listener or owns server
lifecycle. Exactly one loopback subscriber receives finalized entries as SSE.
Its bounded queue preserves recent entries across subscriber absence and
reconnects, then evicts the oldest pending UI update rather than blocking
recording when full. The stream emits a counted `gap` event before the next
entry. This makes loss explicit without
turning a debugging view into a source of application backpressure. It is not
a persistence, fan-out, authentication, or production transport abstraction.
`AsyncRecorder` may wrap it when even JSON encoding should be removed from the
HTTP finalization path; the two bounded queues and their distinct loss policy
must then be understood independently. The Inspector separately retains only
the latest 2,000 live entries and reports older-row eviction in the UI. The
Inspector owns reconnect attempts, retries indefinitely with bounded backoff,
and retains already received rows while the endpoint is unavailable.
The handler always requires a loopback network peer. Browser origins are also
loopback-only by default; `AllowedOrigins` is an explicit exact-origin CORS
opt-in for a trusted hosted Inspector and never accepts wildcards or URL paths.

`MultiRecorder` is a stateless synchronous fan-out. It sends the same immutable
entry pointer to every configured recorder in argument order, attempts later
sinks after an earlier failure, and returns indexed downstream failures through
`errors.Join`. Its batch capability calls `RecordBatch` where supported and
otherwise preserves entry order through individual `Record` calls. It adds no
locking because concurrency safety already belongs to the `Recorder` contract,
does not recover sink panics, and never closes sinks. Placing one
`AsyncRecorder` outside the fan-out creates one ordering and backpressure
boundary for all sinks; wrapping individual sinks instead gives each sink an
independent queue and failure boundary at the cost of more moving parts. The
constructor requires its first sink at compile time and panics immediately on
any nil sink because that is a static application-wiring error, not an
operational recording failure.

`AsyncRecorderConfig.BlockTimeout` retains normal blocking for a bounded interval and then
applies an explicit drop-newest or drop-oldest fallback. Timeout-driven drops
are counted separately from permanent drop-policy decisions. Active waiters
carry unique IDs so `OldestBlockAge` exposes an ongoing stall before any waiter
returns. An optional drop handler runs outside the queue lock with the exact
discarded entry and fixed reason; it is the ownership-transfer point for
releasing managed body assets. Returned errors and recovered panics follow the
same internal-error policy as downstream sink failures and are returned by
`Close`. The handler must itself be bounded because its runtime is outside the
queue-wait timeout guarantee. With `FileBodyStore`, the handler should call
`ReleaseEntryAssets` to complete the discarded entry's ownership transfer.

The queue is a mutex/condition-variable protected ring rather than a channel:
drop-oldest, concurrent close, blocked-producer wakeup and exact queue counters
therefore share one state transition. A single worker invokes downstream
`Record` outside the queue lock. When explicitly configured with a batch size
above one, the sink must expose `RecordBatch([]*Entry) error`. This capability is
discovered structurally and is intentionally not a public interface.
The worker reuses one batch slice, flushes on size, interval, or shutdown, and
preserves FIFO order. Built-in retaining sinks append under one lock;
`JSONStreamRecorder` emits one downstream write containing independent NDJSON
documents. Default batch size one preserves the prompt, allocation-free legacy
path. Panics and returned errors are contained and observable; automatic retry
is intentionally absent because `Recorder.Record` has no idempotency contract.

Downstream sink, sink-close, and drop-handler failures occur in the decorator's
worker lifecycle, often after the originating Transport call has returned.
`AsyncRecorder` can also be shared by several Transports or used independently,
so it cannot route those failures to one Transport's `Config.OnInternalError`.
They are exposed through `Close`, `Stats`, and the same `InternalErrorMode`,
`OnInternalError`, and `Logf` policy used by Transport. `Record` can report only
synchronous queue-state failures; it cannot return a downstream failure that
occurs later in the worker. Applications that want one reporting path can
assign the same callback and logger to both configs.
The explicit zero-value configs remain silent, while both recommended
`Default*Config` constructors select `InternalErrorLog` so evidence degradation
is visible unless an application deliberately opts out.

`Close(ctx)` stops acceptance and drains; after a timeout the same drain
continues in the background. An active downstream call cannot be cancelled
through the minimal `Recorder` interface.

This is latency/backpressure and write-coalescing management, not durability.
The in-memory queue and pending batch are lost on process failure, and a
returned downstream call is not proof of storage. A separate durable recorder
would be required for append/acknowledgement, recovery, rotation and bounded
disk ownership; placing synchronous durability before this queue would
reintroduce disk latency into exchange finalization.

## 13. Concurrency model

Lock/ownership map:

| Synchronization | Protects |
| --- | --- |
| `Transport.initOnce` | one-time construction of redactor/store; `Config` frozen afterwards |
| `traceCollector.mu` | all httptrace event state; `view()` returns a deep-enough snapshot |
| `bodyCapture.mu` | counters/hash/writer — the transport's write loop may still stream the request body while the exchange finalizes |
| `exchange.mu` | life-cycle state and the response snapshot (`respSnapshot` cloned before `RoundTrip` returns) |
| `exchange.finalizeOnce` | exactly-once finalization from Read-EOF / read-error / Close / transport-error paths |
| recorder mutexes | each built-in recorder guards its own state |
| `AsyncRecorder.mu` + conditions | bounded ring queue, lifecycle, backpressure and statistics |
| `FileBodyStore.mu` | byte/file reservations, lifecycle counters, release and quota state |

`Transport` fields and `Config` must not be mutated after the first
request. Entries are immutable after emission, so recorder consumers need no
further synchronization. `go test -race ./...` covers concurrent client use,
concurrent trace draining, and the body-wrapper/finalization races.

For mutex-protected functions whose critical section naturally extends to the
function return, `defer mu.Unlock()` is placed immediately after `mu.Lock()`.
An explicit early `Unlock()` is retained when I/O, callbacks, or other expensive
work must run outside the critical section. This is a design convention rather
than a lint rule: a general-purpose linter cannot distinguish every intentional
early unlock from an accidentally omitted defer without changing lock scope.

## 14. Performance model

- Per-request overhead is a few microseconds on top of `net/http` itself;
  current capture-disabled/header-only benchmarks add roughly 60–80
  allocations/op over the baseline
  depending on capture mode (compare `BenchmarkBaselineNoRecorder` with
  `BenchmarkCaptureDisabled`/`BenchmarkHeaderOnlyCapture`/
  `BenchmarkSmallBody` under `-benchmem`). Exact numbers shift with the
  benchmark setup and the Go runtime. Network latency often dominates simple
  capture, but structured redaction and sensitive-value protection can become
  the primary CPU/allocation cost for dense bodies; see `BENCHMARK.md`.
- Retained body-store content is bounded by the capture limit; the memory store
  pre-sizes from Content-Length to avoid growth re-copies. Total allocation
  volume may be higher because streaming parsers create temporary state.
  Embedding adds copies at entry-build time (store read-back + string conversion).
- Streaming redactors bound retained parser/plaintext state, but bounded memory
  is not the same as low allocation count. Current JSON/XML/form parsers create
  many short-lived lexical objects; the benchmark allocation counts are an
  explicit optimization and regression target.
- **Hashing is the CPU ceiling on large streams** — SHA-256 runs at hardware
  speed and everything past the capture limit is hash+count only. Disable
  `HashBodies` when fingerprints aren't needed and throughput matters.
- For large-body production use: `Config.EmbedBodies = false` +
  `FileBodyStore` + low capture limits (counting stays accurate past the limit
  for instrumented exchanges; head-dropped exchanges intentionally have no
  accounting).
- Benchmarks run over an in-memory `net.Pipe` listener — no OS sockets, no
  ephemeral-port churn — so they measure recorder overhead, not kernel
  networking. `net.Pipe` is unbuffered; absolute MB/s numbers are not
  comparable to TCP loopback, relative differences are what matter.

## 15. Extension packages and tools

- **`otelrecorder/`** — a separate Go module exporting finished entries as
  OTel span events and metrics via `Config.OnEntryCompleted`; the core has no OTel
  dependency. Optional observable callbacks poll `AsyncRecorder`,
  `FileBodyStore`, and Transport sampling snapshots without transferring their
  lifecycle ownership. Default metric dimensions are bounded and exclude
  captured content and correlation identifiers. The complete instrument,
  unit, attribute, cardinality, lifecycle, and alerting contract lives in
  [`otelrecorder/README.md`](otelrecorder/README.md).
- **`inspector/`** — a standalone React + TypeScript viewer for the produced
  HAR files and `JSONStreamRecorder` NDJSON (separate npm project, not part of
  the Go build or runtime).

## 16. Known limitations

- A response body that is never read and never closed produces no entry.
- An unread, unclosed compressed body also retains its bounded decode worker
  until the body is closed; this is part of the same caller lifecycle bug.
- Wire header byte sizes are not observable (`headersSize = -1`).
- The origin IP (and origin DNS/connect timing) is not observable through a
  proxy.
- The HTTP/2 stream ID is not observable; `connection` is the local port.
- On internal transport retries via `GetBody`, only the final attempt's body
  is recorded.
- Brotli/zstd decoders are not bundled (dependency-free core); register
  them via `Config.ContentDecoders`.
- XML attribute values are not redacted (only matched element subtrees).
- `_recorder.network.putIdle` is best-effort (finalization race).
- A custom base `RoundTripper` may fire no httptrace events; trace-derived
  fields degrade to absent/`-1`.
- `http.Client.Timeout` firing mid-body surfaces as `read_response_body` —
  a correct observation, though the cancellation originates from the client
  timer.

## 17. Testing strategy

- **Package boundary**: black-box protocol, request-comment, recorder, and
  trace-store behavioral tests use the external `recorder_test` package and
  therefore compile only against exported API. Parser state machines, timing
  math, error classification, fault injection, fuzz targets, and other
  implementation invariants remain in `package recorder`; production symbols
  are never exported merely to make a test external.
- **Unit tests** for timing computation, error classification, redaction
  (including XML byte-preservation), collector event semantics
  (duplicates, out-of-order, bounds), body-capture accounting, and the
  trace-store operations.
- **Integration tests** over `httptest.Server` (plain, TLS, HTTP/2), raw TCP
  listeners (malformed responses, handshake stalls), self-signed
  certificates, custom dialers and broken `RoundTripper`s — covering DNS,
  refused connections, timeouts, cancellation, redirects and loops, gzip,
  chunked, trailers, proxies, truncation, storage failures.
- **Protocol E2E matrix** (`TestProtocolE2E`) runs the same SSE, request and
  response trailer, and concurrent-request contracts through real HTTP/1.1 and
  HTTP/2 clients and loopback servers. The same suite proves deterministic
  DNS/connect/TLS failure entries, malformed HTTP/1.1 framing behavior, and
  the HTTP upgrade boundary: a `101` handshake is recorded, while the returned
  bidirectional WebSocket stream remains untouched and its frames are never
  treated as an HTTP body.
- **HAR validation**: exports are checked with an independent map-based
  validator, then every `_` extension is stripped and the remainder is
  re-validated as plain HAR 1.2.
- **Race coverage**: `go test -race ./...` includes concurrent clients,
  concurrent per-trace draining, and recorder panics.
- **Fuzz targets**: legacy whole-value JSON/XML/URL redaction, query/header
  conversion, content classification, unwrap chains and HAR serialization,
  plus the streaming JSON, XML, form, multipart and body-redactor-selection
  state machines (`FuzzJSONStreamRedactor`, `FuzzXMLStreamRedactor`,
  `FuzzFormStreamRedactor`, `FuzzMultipartStreamRedactor`,
  `FuzzBodyRedactorSelection`).
- **Benchmarks**: baseline (no recorder), capture off, header-only, small and
  large bodies, full-stream hashing, concurrent requests, structured stream
  redactors across chunk sizes, protection modes, request/response pipelines,
  compression, capture policy, custom redactors, bounded memory-ring overwrite
  and snapshot behavior, and memory/file body stores. HTTP
  cases use the in-memory network except the explicit `FileBodyStore` case;
  methodology and a reproducible snapshot are in `BENCHMARK.md` (§14).
- The `otelrecorder` module has its own suite against in-memory OTel SDKs
  (span/metric shapes, secret-leak checks, race); the `inspector` app has
  vitest unit tests for its parser/formatters plus a TypeScript build gate.
