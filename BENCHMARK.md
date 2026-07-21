# Benchmarks

This document describes the benchmark suite, records a reproducible performance
snapshot, and calls out the paths where configuration choices materially affect
CPU, allocation, memory, or I/O cost. Benchmark results are comparative data,
not universal capacity promises: rerun them on the deployment hardware and Go
toolchain used in production.

## Reproducing the results

The snapshot below was collected on 2026-07-20 with:

- Apple M1 Max (`darwin/arm64`)
- Go 1.26.5
- `GOMAXPROCS=1`
- an in-memory `net.Pipe` HTTP client/server transport, except for the explicit
  `FileBodyStore` case

Run the main benchmark groups with:

```sh
GOMAXPROCS=1 go test -run '^$' \
  -bench '^(BenchmarkStreamRedactors|BenchmarkSensitiveValueProtection|BenchmarkTransportBodyPipeline)$' \
  -benchmem -benchtime=300ms -count=1
```

Run the async delivery microbenchmarks separately with:

```sh
GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkAsyncRecorder$' \
  -benchmem -benchtime=1s -count=1
```

Run the bounded memory-retention microbenchmarks with:

```sh
GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkMemoryRecorder' \
  -benchmem -benchtime=1s -count=1
```

For comparison work, prefer `-count=5` and feed the before/after outputs to
`benchstat`. Avoid comparing results collected with different Go versions,
power modes, `GOMAXPROCS` values, or storage devices.

## What is measured

The suite has two layers:

1. `BenchmarkStreamRedactors` and `BenchmarkSensitiveValueProtection` measure
   the streaming transform itself. They cover JSON, NDJSON, XML,
   `application/x-www-form-urlencoded`, and multipart bodies; 32-byte, 4 KiB,
   and whole-body writes; sparse and dense matches; redact, AES-256-GCM encrypt,
   HMAC-SHA-256 tokenize, and oversized-value fail-closed behavior.
2. `BenchmarkTransportBodyPipeline` measures the same work through the complete
   recorder Transport. It covers response-only and request-plus-response
   capture, gzip decoding, a custom `BodyRedactor`, a capture policy callback,
   `FileBodyStore` with embedding disabled, and parallel redaction.

All built-in redactor benchmarks include writer construction, streaming writes,
parser finalization, protection reporting, and output emission to `io.Discard`.
Transport benchmarks additionally include request construction, HTTP framing,
body lifecycle accounting, entry construction, and audit collection.

## Core recorder overhead

These cases use a 1 KiB response unless otherwise noted.

| Case | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Bare `net/http` baseline | 10,962 | 93.42 | 5,746 | 67 |
| Recorder, capture disabled | 16,587 | 61.74 | 11,252 | 133 |
| Header-only capture | 17,429 | 58.75 | 11,724 | 142 |
| 1 KiB captured, embedded, SHA-256 | 27,278 | 37.54 | 57,438 | 209 |
| 1 MiB captured, embedded, SHA-256 | 884,566 | 1,185.41 | 3,200,851 | 218 |

The recorder wrapper with capture disabled adds about 5.6 µs and 66 allocations
to this deliberately low-latency in-memory baseline. Real network latency makes
the relative percentage smaller, but the absolute local overhead remains
relevant for very high request rates.

## Streaming redactors

Payloads in this group are approximately 42–94 KiB and intentionally contain
many structured values. The table reports the 4 KiB write case, which is the
most representative default for streaming comparisons.

| Format and match density | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| JSON, sparse | 824,307 | 58.43 | 508,746 | 9,237 |
| JSON, no matching field | 912,109 | 56.17 | 514,144 | 9,228 |
| JSON, dense | 1,043,937 | 47.11 | 549,710 | 18,453 |
| NDJSON, dense | 1,004,135 | 41.81 | 549,334 | 18,438 |
| XML, dense | 688,404 | 105.63 | 49,568 | 14,343 |
| Form, dense | 466,582 | 131.69 | 49,472 | 16,388 |
| Multipart, dense | 525,843 | 177.83 | 573,664 | 7,209 |

### Chunk-size sensitivity

| Format | 32-byte writes | 4 KiB writes | Whole body | Observation |
| --- | ---: | ---: | ---: | --- |
| JSON dense | 46.68 MB/s | 47.11 MB/s | 46.98 MB/s | Essentially insensitive |
| NDJSON dense | 40.92 MB/s | 41.81 MB/s | 41.35 MB/s | Small writes cost about 2% |
| XML dense | 100.10 MB/s | 105.63 MB/s | 105.79 MB/s | Small writes cost about 5% |
| Form dense | 128.88 MB/s | 131.69 MB/s | 131.03 MB/s | Small writes cost about 2% |
| Multipart dense | 137.85 MB/s | 177.83 MB/s | 181.62 MB/s | 32-byte writes cost about 23% |

The parsers preserve streaming behavior across chunk boundaries. Artificially
coalescing normal 4–64 KiB reads is unlikely to help JSON/XML/form materially;
multipart is the exception when an upstream component emits extremely small
writes.

## Protection modes

This comparison uses the same dense JSON body and 4 KiB writes.

| Mode | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Redact | 1,037,676 | 47.40 | 549,711 | 18,453 |
| AES-256-GCM encrypt | 2,202,651 | 22.33 | 2,206,195 | 28,705 |
| HMAC-SHA-256 tokenize | 1,788,610 | 27.50 | 1,337,430 | 29,734 |
| Encrypt, value over limit (fail closed) | 1,250,943 | 52.40 | 351,893 | 65,574 |

Encryption is about 2.12x slower than replacement redaction in this dense-match
workload; tokenization is about 1.72x slower. The difference grows with the
number of protected values, not merely total body size. Oversized encryption
values stop retaining plaintext and fall back to `[REDACTED]`; the benchmark
confirms that this path remains bounded instead of paying the normal encryption
output cost.

## Complete Transport body pipeline

These cases use a dense ~48 KiB decoded JSON response. `request_response` runs
the redactor over the same body in both directions. MB/s is based on decoded
bytes processed, including both directions where applicable.

| Case | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Capture only, memory store | 35,365 | 1,390.68 | 102,762 | 163 |
| Response redaction | 1,181,356 | 41.63 | 652,739 | 18,622 |
| Request + response redaction | 2,400,054 | 40.98 | 1,329,909 | 37,136 |
| Response encryption | 2,411,173 | 20.40 | 2,654,033 | 28,884 |
| Response tokenization | 1,879,450 | 26.17 | 1,555,443 | 29,907 |
| Gzip decode + response redaction | 1,227,526 | 40.07 | 773,653 | 18,647 |
| Custom pass-through redactor | 33,332 | 1,475.54 | 102,471 | 157 |
| Pass-through capture policy callback | 32,983 | 1,491.13 | 102,759 | 163 |
| `FileBodyStore` + response redaction | 1,359,630 | 36.17 | 596,125 | 18,629 |

The custom-redactor adapter and capture-policy callback add no meaningful cost
at this payload size when their own logic is trivial. Gzip decoding reduces
redaction throughput by about 4%. The file-store result includes temp-file
creation, writing, closing, and deletion on the benchmark machine; storage
hardware and filesystem behavior will dominate its portability.

With `GOMAXPROCS=8`, `BenchmarkTransportRedactionParallel` processed the same
dense response at 438,454 ns/op and 112.17 MB/s (1,220 iterations in the sample).
That is roughly 3.3x the single-worker throughput, not linear 8x scaling. The
benchmark exercises shared Transport/recorder operation and is intended to
catch contention or race-driven regressions; repeat it at the production
`GOMAXPROCS` value rather than treating this machine-specific ratio as a limit.

## Large streaming and hashing

The 100 MiB cases use one iteration to avoid turning normal test runs into a
long stress test. Capture is limited to 1 MiB; remaining bytes are counted and,
when enabled, hashed.

| Case | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| SHA-256 enabled | 56,975,583 | 1,840.40 | 2,257,048 | 1,908 |
| Hashing disabled | 10,587,334 | 9,904.06 | 2,228,280 | 1,845 |

On this CPU, SHA-256 is the dominant cost after the capture limit: the measured
run is about 5.4x slower than byte counting alone. Disable body hashes only when
their integrity and correlation value is not needed.

## Bounded asynchronous delivery

These microbenchmarks isolate `Recorder.Record` dispatch. The bounded-block
case uses a 1,024-entry queue and a no-op downstream worker; the full-drop case
holds a one-entry queue full so every measured call takes the explicit
`AsyncDropNewest` path.

| Case | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Direct no-op recorder | 2.07 | 0 | 0 |
| `AsyncRecorder`, bounded/default block | 42.50 | 0 | 0 |
| Full queue, `AsyncDropNewest` | 13.71 | 0 | 0 |

The uncontended async queue adds about 40 ns to this synthetic no-op baseline
and performs no per-entry heap allocation. This is not a sink-latency result:
the purpose of the decorator is to move downstream I/O off the finalizing
goroutine until the queue fills. With the default `AsyncBlock` policy, a full
queue intentionally transfers sink backpressure to the application and latency
then approaches the rate at which the sink frees capacity. Benchmark and alert
on blocked duration with a representative sink and queue size; choosing a drop
policy changes the evidence-completeness contract, not merely performance.

## Bounded in-memory retention

These microbenchmarks use a 1,024-entry `MemoryRecorder`. The record case is
already full and therefore measures steady-state oldest-entry eviction. The
snapshot case reads a wrapped ring into an oldest-to-newest result slice.

| Case | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Full ring, record and evict oldest | 13.80 | 0 | 0 |
| Wrapped ring, snapshot 1,024 entries | 3,123 | 9,472 | 1 |

Steady-state retention performs no per-entry heap allocation. `Entries` and
`Snapshot` intentionally allocate one result slice so callers cannot mutate
the recorder's ring; their cost scales linearly with retained capacity.

## Performance-critical guidance

- Leave body capture disabled unless the recorded payload is operationally
  necessary. Production-safe defaults already follow this rule.
- Apply `BodyCapturePolicy` early to exclude high-volume endpoints or directions;
  a pass-through policy is cheap, while avoiding parsing is a large saving.
- Prefer request-scoped additive redaction rules over constructing a Transport
  per endpoint. Rules are copied when attached and resolved once per exchange,
  while the shared client's connection pool remains reusable.
- Prefer `FileBodyStore` with `EmbedBodies(false)` for large retained bodies.
  This bounds HAR memory growth, but moves throughput and retention concerns to
  the filesystem; clean up files according to application policy.
- Redaction cost scales with structured tokens and matching values. Dense JSON
  plus encryption/tokenization is intentionally the worst normal case in this
  suite. Benchmark representative schemas and secret density.
- Encryption buffers one protected value up to `MaxValueBytes`; tokenization
  streams through HMAC. Keep the encryption limit no larger than required.
- Hashing continues after the capture limit so hashes describe all bytes read.
  This is useful but CPU-visible for very large streams.
- Gzip decoding is required before structured redaction. Compression bombs are
  bounded by capture limits, but compressed traffic still consumes decoder CPU.
- Avoid upstream middleware that splits multipart bodies into tiny writes. The
  benchmark shows a measurable penalty at 32-byte chunks.
- Custom redactors execute on the request/response read path. They should remain
  streaming, bounded, panic-safe, and free of blocking external calls.
- `AsyncRecorder` uses `AsyncBlock` by default so queue pressure does not
  silently discard evidence. Size the queue for expected bursts and monitor
  blocked producers; opt into a drop policy only when application availability
  is more important than complete capture. The queue is not crash-durable.
- `MemoryRecorder` retains a capped entry count but entry size still depends on
  capture configuration. Size both the ring and request/response body limits;
  monitor its lifetime eviction count, and do not treat a post-eviction trace
  as complete evidence.

## Current optimization targets

Single-byte output previously used `dst.Write([]byte{b})`, causing the slice to
escape through `io.Writer` once per emitted byte. Caching `io.ByteWriter` and
using a reusable one-byte fallback reduced dense JSON from roughly 59k to 18.5k
allocations/op and raised throughput from 37.28 to 47.11 MB/s. Sparse/no-match
JSON fell by about 84% to roughly 9.2k allocations/op; XML and form also
improved materially.

Allocation pressure nevertheless remains in the handwritten structured
parsers: dense JSON still performs roughly 18.5k allocations for a ~48 KiB
body, and XML/form still allocate per lexical unit. The next optimization pass
should profile these benchmarks with `-memprofile` and focus on reusable token
buffers and avoiding short-lived string/byte conversions without weakening
malformed-input fail-closed behavior or byte-preservation guarantees. The
oversized protected-value path remains a separate high-allocation target.

Treat those allocation counts as regression baselines. New features should not
silently increase them; performance changes should include before/after
`benchstat` output and the same correctness, race, and fuzz checks used for the
stream redactors.
