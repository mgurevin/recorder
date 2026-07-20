# recorder

[![CI](https://github.com/mgurevin/recorder/actions/workflows/ci.yml/badge.svg)](https://github.com/mgurevin/recorder/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mgurevin/recorder.svg)](https://pkg.go.dev/github.com/mgurevin/recorder)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`recorder` is an `http.RoundTripper` that records the full life cycle of
`net/http` client exchanges as **HAR 1.2** documents. It records successful
responses and, just as thoroughly, calls that fail before or during the
response: DNS resolution, TCP connect, TLS handshake, proxy dialing, context
cancellation, and request/response body stream errors — each classified into
a structured `_error` extension.

- **Core package is standard library only.** Optional integrations (the
  OpenTelemetry adapter, the HAR inspector UI) live in separate
  modules/directories and never pull dependencies into the core.
- **Caller-visible HTTP behavior never changes.** The original request is not
  mutated, response bytes/EOF/errors pass through untouched, and recorder
  failures never break the HTTP call.
- **Redaction applies to the recording only.** The live request and response
  are never modified; secrets are replaced in the recorded copy.
- **Nothing is invented.** Values that cannot be observed (wire header sizes,
  compressed sizes after transparent gzip, unmeasured timing phases, the
  origin IP behind a proxy) are recorded as `-1` or omitted, per HAR 1.2.

## Motivation

I often needed a reliable way to record outbound API calls, especially for
financial workflows where preserving an accurate account of an exchange can
be essential for debugging, reconciliation, and incident analysis.

Most HTTP recorders focus on successful request/response pairs. In practice,
the failures are often more important: DNS errors, connection failures, TLS
handshake problems, context cancellation, truncated bodies, and stream
errors.

`recorder` captures the complete client exchange lifecycle as HAR 1.2 without
reimplementing HTTP transport behavior. It wraps an existing
`http.RoundTripper`, preserves caller-visible request and response semantics,
and records only what can actually be observed.

The result is a portable debugging artifact combining HTTP data, network
timings, structured failures, TLS details, and configurable redaction. Body
capture remains opt-in by default, which is particularly important when API
traffic may contain financial or other sensitive data.

## Requirements

- Core module: Go 1.24 or newer.
- `otelrecorder` module: Go 1.25 or newer, matching its OpenTelemetry dependencies.

CI tests each module on its minimum supported Go version and the current
stable Go release.

## Quick start

```go
package main

import (
	"io"
	"net/http"
	"os"

	"github.com/mgurevin/recorder"
)

func main() {
	rec := recorder.NewMemoryRecorder()
	client := &http.Client{
		Transport: recorder.NewTransport(http.DefaultTransport, rec),
	}

	resp, err := client.Get("https://example.com/")
	if err == nil {
		io.Copy(io.Discard, resp.Body) // the entry finalizes on body EOF/Close
		resp.Body.Close()
	}

	rec.WriteHAR(os.Stdout) // or: har := rec.HAR()
}
```

An entry is **not** complete when `RoundTrip` returns — the response body has
not been read yet. It reaches the recorder when the body hits EOF, is closed
early, or fails; transport errors finalize immediately. A body that is never
read *and* never closed produces no entry (a caller bug that also leaks the
connection in plain `net/http`).

## What gets recorded

| Area | What is recorded |
| --- | --- |
| HAR request/response | method, URL, status, headers (wire-order when observable), cookies, body metadata |
| timings | blocked, dns, connect, ssl, send, wait, receive |
| `_error` | transport/body failure: phase, type, message, timeout/context flags, unwrap chain |
| `_network` | DNS results + coalescing, local/remote address, IP version, reuse/idle, proxy, HTTP/2, idle-pool return |
| `_tls` | TLS version, cipher suite, ALPN, SNI, resumption, OCSP/SCT, certificate chain |
| `_requestBody` / `_responseBody` | completion, early close, truncation, total/captured bytes, hash, store reference |
| trailers / transfer encoding | `_requestTrailers`, `_responseTrailers`, `_*TransferEncoding` |
| `_trace` | raw httptrace event timeline (when enabled); details pass through `RedactErrorMessage` |
| `_expect100` | `Expect: 100-continue` handshake (waited, received, wait time) |
| `_informational` | 1xx interim responses (100, 103 Early Hints) with redacted headers |
| correlation | `_traceId`, `_exchangeId`, `_redirectIndex` |

Stripping every `_`-prefixed field leaves a valid plain HAR 1.2 document
(verified by test); the files open in standard HAR viewers.

Security issues should be reported privately as described in
[SECURITY.md](SECURITY.md). Release history is maintained in
[CHANGELOG.md](CHANGELOG.md).

## Default configuration

`NewTransport` starts from `DefaultOptions()` and applies your `Option`
values on top:

| Option field | Default | Notes |
| --- | --- | --- |
| `CaptureRequestBody` | `false` | lifecycle and byte counts tracked; content capture is opt-in |
| `CaptureResponseBody` | `false` | lifecycle and byte counts tracked; content capture is opt-in |
| `EmbedBodies` | `false` | body text is not embedded by default |
| `MaxRequestBodyBytes` | `1 MiB` | content capture limit; `<= 0` means unlimited |
| `MaxResponseBodyBytes` | `1 MiB` | content capture limit; `<= 0` means unlimited |
| `CaptureTLS` | `true` | `_tls` extension enabled |
| `CaptureCertificates` | `true` | peer certificate metadata enabled |
| `CaptureRawCertificates` | `false` | raw DER not embedded by default |
| `CaptureHeaders` | `true` | headers and trailers recorded |
| `CaptureCookies` | `true` | parsed cookies recorded |
| `RedactHeaders` | `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-API-Key` | case-insensitive |
| `RedactQueryParameters` | empty | opt-in |
| `RedactCookies` | empty | opt-in; cookies are also redacted when their carrier header is |
| `RedactJSONFields` | empty | opt-in |
| `RedactXMLElements` | empty | opt-in (SOAP bodies) |
| `HashBodies` | `false` | full-stream hashing is opt-in |
| `BodyHashAlgorithm` | `sha256` | `sha1`/`md5` supported; unknown values fall back to sha256 |
| `CaptureRawTrace` | `false` | raw httptrace event list disabled by default |
| `ContentDecoders` | `gzip`, `x-gzip`, `deflate` | stdlib decoders for record-time decoding |
| `BodyStore` | `MemoryBodyStore` | used when nil |
| `InternalErrorMode` | `InternalErrorIgnore` | reports through `OnInternalError` if set |
| `OnInternalError` | `nil` | optional callback for recorder-internal errors |
| `Logf` | `nil` | used by `InternalErrorLog`; nil falls back to the standard log package |
| `OnEntryCompleted` | `nil` | optional per-entry callback (context + entry) |
| `RedactErrorMessage` | `nil` | optional error message redactor |

Also note:

- `WithOptions(Options{})` replaces the whole struct: zero-value options
  disable all capturing (and body embedding).
- `Options` must not be mutated after the transport served its first request.

## Options reference

| Option | Effect |
| --- | --- |
| `WithOptions(o)` | Replace the entire `Options` value; apply first when combining |
| `WithCaptureRequestBody(v)` | Toggle request body content capture (size/state always tracked) |
| `WithCaptureResponseBody(v)` | Toggle response body content capture |
| `WithEmbedBodies(v)` | Off: keep sizes/hashes/store refs but embed no body text — the production setting with `FileBodyStore` |
| `WithMaxRequestBodyBytes(n)` | Request capture limit; counting continues past it |
| `WithMaxResponseBodyBytes(n)` | Response capture limit; also bounds record-time decoding |
| `WithCaptureTLS(v)` | Toggle the `_tls` extension |
| `WithCaptureCertificates(v, raw)` | Toggle certificate details; `raw` embeds Base64 DER |
| `WithCaptureHeaders(v)` | Toggle header/trailer recording |
| `WithCaptureCookies(v)` | Toggle parsed cookie recording |
| `WithRedactHeaders(names...)` | Append case-insensitive header names to redact |
| `WithRedactQueryParameters(names...)` | Redact query params in `url` and `queryString` (and form fields) |
| `WithRedactCookies(names...)` | Redact cookies by name |
| `WithRedactJSONFields(names...)` | Recursively redact JSON object fields in captured bodies |
| `WithRedactXMLElements(names...)` | Redact XML element subtrees by local name (namespace prefixes ignored) |
| `WithHashBodies(enabled, alg)` | Toggle body hashing / choose algorithm |
| `WithBodyStore(s)` | Storage backend for captured bytes (`MemoryBodyStore`, `FileBodyStore`, custom) |
| `WithCaptureRawTrace(v)` | Record every raw httptrace event under `_trace` |
| `WithContentDecoder(enc, dec)` | Register a record-time decoder (e.g. brotli, zstd) for a `Content-Encoding` |
| `WithInternalErrorMode(m)` | `Ignore` (default) or `Log`; recorder failures never alter the HTTP result |
| `WithOnInternalError(fn)` | Callback for recorder-internal errors |
| `WithLogf(fn)` | Logger used by `InternalErrorLog` |
| `WithOnEntryCompleted(fn)` | Per-entry completion callback — the integration hook (OTel adapter uses it) |
| `WithErrorRedactor(fn)` | Filter applied to every recorded error message |

## Production recipes

### Full forensic capture

Explicitly enable body capture and add the redaction your payloads need:

```go
tr := recorder.NewTransport(base, rec,
	recorder.WithCaptureRequestBody(true),
	recorder.WithCaptureResponseBody(true),
	recorder.WithEmbedBodies(true),
	recorder.WithHashBodies(true, "sha256"),
	recorder.WithRedactQueryParameters("token", "api_key"),
	recorder.WithRedactJSONFields("password", "secret"),
	recorder.WithMaxResponseBodyBytes(4<<20),
)
```

### High-volume telemetry

```go
tr := recorder.NewTransport(base, rec,
	recorder.WithCaptureRequestBody(true),
	recorder.WithCaptureResponseBody(true),
	recorder.WithEmbedBodies(false),                      // no body text in the HAR
	recorder.WithBodyStore(recorder.FileBodyStore{Dir: "/var/spool/recorder"}),
	recorder.WithMaxResponseBodyBytes(64<<10),            // small capture budget
	recorder.WithHashBodies(false, ""),                   // hashing caps large streams at ~SHA-256 speed
)
```

Hashing covers every streamed byte and is the throughput ceiling on large
bodies (the 100 MB streaming benchmark runs ~4x faster without it — compare
`Benchmark100MBStreamingBody` and `Benchmark100MBStreamingBodyNoHash`).

### Default header-only capture

```go
tr := recorder.NewTransport(base, rec)
```

### SOAP/XML redaction

```go
tr := recorder.NewTransport(base, rec,
	recorder.WithRedactXMLElements("Username", "Password"), // covers <wsse:Password> etc.
)
```

### Per-request export with a trace ID

```go
ctx, traceID := recorder.TraceContext(context.Background())
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
resp, err := client.Do(req) // redirect hops share the trace ID
if err == nil {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

entries := rec.TakeTrace(traceID)   // atomically remove + return this call's entries
recorder.NewHAR(entries).Write(f)   // standalone HAR for just this call
```

### Streaming NDJSON export

`JSONStreamRecorder` writes each entry as one JSON line the moment it
completes — no buffering, and the top-level HAR wrapper is intentionally not
emitted so no JSON document is ever left half-written:

```go
stream := recorder.NewJSONStreamRecorder(w)
tr := recorder.NewTransport(base, stream)
```

### OpenTelemetry adapter

See [OpenTelemetry export](#opentelemetry-export-otelrecorder) below — the
adapter lives in the separate `otelrecorder` submodule and plugs into
`WithOnEntryCompleted`.

## Body capture, storage, and hashing

Three independent concerns:

- **Counting** always runs, even with capture off: `totalBytes` and the HAR
  size fields reflect the real stream, beyond any limit.
- **Capture** writes content to the `BodyStore` until `Max*BodyBytes` is
  reached; past the limit only counting (and hashing) continues, and the
  record is marked `truncated`. `MemoryBodyStore` (default) buffers in
  memory; `FileBodyStore` spools to temp files so large bodies never live in
  memory — **cleaning up its files is the caller's responsibility** (paths
  are exposed via `_requestBody`/`_responseBody.store`).
- Configured JSON/XML redaction runs as a bounded streaming transform before
  bytes reach the BodyStore. It does not write a raw body and overwrite it
  later. Once a matching field/element has been recognized, its value/subtree
  remains protected even when the later body is malformed, partial, or
  truncated. Malformed syntax before a would-be match can prevent that match
  from being recognized, so redaction is not a substitute for rejecting
  invalid payloads at the application boundary.
- **Embedding** (`EmbedBodies`) decides whether captured content becomes
  `postData.text` / `content.text` in the HAR. With `WithEmbedBodies(false)`
  the document stays small while sizes, hashes, truncation state and the
  store reference remain.

Hashes cover the **entire** stream (truncation does not affect them) and are
only emitted for complete streams — a partial-stream hash would be
misleading.

## Redaction

- **Headers, query parameters, cookies** are redacted by case-insensitive
  name; a cookie is also redacted when its carrier header (`Cookie` /
  `Set-Cookie`) is in the header list.
- **JSON field redaction** recursively replaces matching object-field values
  while preserving every unredacted byte (including whitespace, key order,
  duplicate keys, number spelling, and escapes).
- **XML element redaction** replaces the complete subtree inside matching elements
  (by local name, namespace prefixes ignored — `"Password"` covers
  `<wsse:Password>`), preserving the rest of the document byte-for-byte.
  XML **attribute values are not redacted**.
- A bounded prefix sniffer recognizes JSON/XML sent under a generic or
  incorrect content type such as `text/plain`.

Rules that hold everywhere:

- Redaction applies **only to the recorded copy** — the live HTTP request and
  response are never modified.
- Streaming parsers cap key/tag buffers at 64 KiB, nesting at 1024, and MIME
  sniffing at 4 KiB. Limit violations stop store capture rather than falling
  back to unredacted bytes.
- Because a streaming sink cannot roll back committed output, a matched field
  stays redacted even if later input proves malformed or incomplete.
- Malformed syntax that appears before a field/element can prevent the parser
  from recognizing that later match; do not rely on body redaction as an
  input-validation mechanism.
- Body hashes are computed over the real wire/caller bytes, never over
  redacted bytes.
- Error messages can carry secrets too: `WithErrorRedactor` filters every
  recorded error string.

## Failures and error classification

HTTP 4xx/5xx are **not** transport errors — they are recorded as normal
responses. Transport and body-stream failures produce a `_error` extension:

```json
{
  "_error": {
    "phase": "dns",
    "type": "*net.DNSError",
    "message": "lookup api.example.com: no such host",
    "timeout": false,
    "temporary": false,
    "contextCanceled": false,
    "contextDeadlineExceeded": false,
    "unwrapChain": ["*url.Error", "*net.OpError", "*net.DNSError"]
  }
}
```

Phases: `request_setup`, `dns`, `connect`, `proxy`, `tls`, `write_request`,
`write_request_body`, `wait_response`, `read_response_headers`,
`read_response_body`, `redirect`, `context`, `unknown`.

Classification is type-first (`errors.Is`/`errors.As` over the unwrap
chain), with httptrace progress as fallback; string matching is a last
resort for untyped errors only. Two distinctions worth knowing:

- The `context` phase and the `contextCanceled`/`contextDeadlineExceeded`
  flags require the request's **own context** to have fired. Transport-
  internal timeouts (e.g. `ResponseHeaderTimeout`) merely match context
  errors via `errors.Is` and are attributed to the phase the exchange stood
  in (`wait_response`), with `timeout: true`.
- A response body **read error** records `_error` with phase
  `read_response_body`; a body **closed early** by the caller is not an
  error — it records state `closed_early` and `_responseBody.closedEarly`.

## Timings and observability limits

HAR `timings` are milliseconds; `-1` means "not observed / not applicable",
never zero:

- Reused connections (including HTTP/2 multiplexing) report `-1` for
  dns/connect/ssl — those phases did not happen for the exchange; `blocked`
  covers the wait for a pooled connection.
- `headersSize` is always `-1`: the exact bytes written to the wire
  (including transport-added headers, HPACK on HTTP/2) are not observable at
  the RoundTripper layer.
- With transparent gzip, `bodySize` is `-1` and `content.size` is the
  decoded size (`content._decoded: true`).
- Behind a proxy the TCP peer is the proxy: `serverIPAddress` is omitted and
  `_network`'s addresses describe the proxy connection. With a standard
  `*http.Transport`, `_network.proxy` contains the selected proxy URL after
  password and configured query-parameter redaction. A custom RoundTripper
  may expose only the dialed proxy address as a `host:port` fallback.
- A response body that is neither read nor closed never finalizes — no entry
  is produced (no finalizers by design).
- `_network.putIdle` is best-effort: the connection's return to the idle
  pool can race with entry finalization, so absence means "not observed".

## Compression and content decoding

1. **Transparent gzip** — when you don't set `Accept-Encoding`,
   `http.Transport` negotiates and decompresses gzip itself. The record
   stores decoded bytes (`_decoded: true`), `bodySize` is `-1`.
2. **Record-time decoding** — when you negotiated compression yourself and a
   decoder is registered, structured redaction streams the decoded form into
   the BodyStore. `bodySize`, hashes and stream counters still describe the
   wire bytes.

`gzip`, `x-gzip` and `deflate` (zlib-wrapped or raw, sniffed like browsers)
ship by default using only the standard library. Brotli/zstd are
deliberately not bundled; wiring them is a few lines with the de-facto
standard pure-Go implementations (zero transitive deps, no cgo):

```go
recorder.WithContentDecoder("br", func(r io.Reader) (io.ReadCloser, error) {
	return io.NopCloser(brotli.NewReader(r)), nil // github.com/andybalholm/brotli
})

recorder.WithContentDecoder("zstd", func(r io.Reader) (io.ReadCloser, error) {
	zr, err := zstd.NewReader(r) // github.com/klauspost/compress/zstd
	if err != nil {
		return nil, err
	}
	return zr.IOReadCloser(), nil
})
```

Safety rails: decoded output larger than `MaxResponseBodyBytes` is refused,
so a compression bomb cannot blow the capture budget. When structured
redaction is active, unknown/multi-step encodings and decoder failures stop
store capture rather than falling back to raw bytes. Failures are reported
through `OnInternalError` and never affect bytes received by the caller.

## Recorder implementations

| Recorder | Stores entries? | TraceStore? | Best for |
| --- | --- | --- | --- |
| `MemoryRecorder` | yes | yes | tests, short-lived capture, `HAR()`/`WriteHAR` |
| `HARFileRecorder` | yes, until flush | yes | complete HAR file written atomically on `Flush`/`Close` |
| `JSONStreamRecorder` | no | no | streaming NDJSON, one line per entry |
| `RecorderFunc` | caller-defined | no | custom callbacks |

All built-in recorders are safe for concurrent use; entries are immutable
snapshots. Only finalized entries ever reach a recorder — in-flight
exchanges are absent from every export.

## Trace correlation

`http.Client` follows redirects above the transport, so each hop is a
separate entry. Correlate them through the context:

- `recorder.WithTraceID(ctx, id)` — install your correlation ID; every hop
  records it as `_traceId` with an incrementing `_redirectIndex`.
- `recorder.TraceContext(ctx)` — same, with a generated random ID.

`MemoryRecorder` and `HARFileRecorder` implement the optional `TraceStore`
capability (discovered via type assertion on `Recorder`):

- `EntriesByTrace(id)` — return the trace's entries, keep them stored
- `RemoveTrace(id)` — delete the trace's entries
- `TakeTrace(id)` — **atomically** remove and return them; an entry
  finalized concurrently can never fall between a query and a delete

`MemoryRecorder` additionally provides `HARForTrace(id)` as a convenience
helper that builds a HAR document for one trace; it is not part of the
`TraceStore` interface. For other recorders, combine
`TakeTrace`/`EntriesByTrace` with `recorder.NewHAR(entries)`.

## HAR inspector

The repo ships a local React + TypeScript viewer under
[`inspector/`](inspector/) for the HAR files this library produces,
including every `_` extension field:

![HAR Inspector screenshot](docs/assets/inspector.png)

- Entry list with filtering (method, status class, state, error phase,
  host/path search, traceId, failed/truncated/closed-early), sorting, and
  trace-chain grouping by `_traceId`.
- Detail tabs: overview with a timing waterfall, timings (`-1` shown as
  "not observed"), request/response with JSON/XML pretty printing and
  binary indicators, `_error`, `_network`, `_tls` with the certificate
  chain, raw `_trace` timeline, raw JSON with unknown extensions preserved,
  and a Replay tab that reconstructs a cURL command (including `--proxy`
  from the redacted `_network.proxy` URL when present).
- Virtualized list for large HAR files; responsive down to mobile widths.

```bash
cd inspector
npm install
npm run dev     # http://localhost:5173
npm run build
npm test
```

Workflow:

```text
1. produce a HAR with recorder (WriteHAR / HARFileRecorder)
2. open the inspector
3. drag & drop the .har file (or use the file picker / built-in sample)
4. inspect entry details tab by tab
```

**Security note:** HAR files routinely contain sensitive data. The inspector
parses files **locally in your browser** — nothing is uploaded anywhere. See
[inspector/README.md](inspector/README.md).

## OpenTelemetry export (`otelrecorder`)

The [`otelrecorder`](otelrecorder/) submodule (its own Go module — the core
stays dependency-free) exports finished entries as OTel span events and
metrics through the `OnEntryCompleted` hook:

```go
import "github.com/mgurevin/recorder/otelrecorder"

exporter, err := otelrecorder.NewExporter() // uses the global OTel providers
if err != nil {
	// handle
}
tr := recorder.NewTransport(base, rec,
	recorder.WithOnEntryCompleted(exporter.OnEntryCompleted))
```

With an active span in the request context, each exchange becomes a
`recorder.http.exchange` span event; metrics (duration/body-size histograms,
failure/closed-early/truncation counters) are always recorded.

**Cardinality guidance:** the adapter never exports full URLs, paths, query
strings, header/cookie values, body content or raw HAR JSON.
`server.address` is host-only, metric labels use the status *class* (`2xx`,
`0`), correlation IDs are span-event-only and opt-in, string values are
clamped. Custom attributes are for user-controlled low-cardinality
dimensions such as a route *template* — never raw paths or IDs. When you
need the full HAR entry, write it to a dedicated sink (`HARFileRecorder`,
`JSONStreamRecorder`); an OTel attribute is the wrong place for a document.

## Development

```bash
go test ./...
go test -race ./...
go vet ./...
go test -bench . -run '^$'          # benchmarks (in-memory network, no OS sockets)
go test -fuzz FuzzRedactJSON        # fuzz targets: FuzzRedactJSON, FuzzRedactXML,
                                    # FuzzRedactURL, FuzzQueryPairs, FuzzHeaderPairs,
                                    # FuzzContentClassification, FuzzUnwrapChain,
                                    # FuzzHARSerialization
```

Architecture decisions, invariants and known limitations are documented in
[DESIGN.md](DESIGN.md).
