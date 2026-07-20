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
| JSON, sparse | 1,171,476 | 41.11 | 557,852 | 57,390 |
| JSON, no matching field | 1,193,077 | 42.94 | 565,993 | 60,460 |
| JSON, dense | 1,319,169 | 37.28 | 590,631 | 59,435 |
| NDJSON, dense | 1,255,127 | 33.45 | 582,043 | 52,230 |
| XML, dense | 794,774 | 91.49 | 57,696 | 23,559 |
| Form, dense | 597,033 | 102.92 | 65,824 | 40,966 |
| Multipart, dense | 536,639 | 174.25 | 573,664 | 7,209 |

### Chunk-size sensitivity

| Format | 32-byte writes | 4 KiB writes | Whole body | Observation |
| --- | ---: | ---: | ---: | --- |
| JSON dense | 37.10 MB/s | 37.28 MB/s | 36.80 MB/s | Essentially insensitive |
| NDJSON dense | 31.54 MB/s | 33.45 MB/s | 34.04 MB/s | Small writes cost about 7% |
| XML dense | 88.13 MB/s | 91.49 MB/s | 92.00 MB/s | Small writes cost about 4% |
| Form dense | 100.44 MB/s | 102.92 MB/s | 103.19 MB/s | Small writes cost about 3% |
| Multipart dense | 137.16 MB/s | 174.25 MB/s | 174.46 MB/s | 32-byte writes cost about 21% |

The parsers preserve streaming behavior across chunk boundaries. Artificially
coalescing normal 4–64 KiB reads is unlikely to help JSON/XML/form materially;
multipart is the exception when an upstream component emits extremely small
writes.

## Protection modes

This comparison uses the same dense JSON body and 4 KiB writes.

| Mode | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Redact | 1,331,633 | 36.93 | 590,629 | 59,435 |
| AES-256-GCM encrypt | 2,454,395 | 20.04 | 2,247,141 | 69,687 |
| HMAC-SHA-256 tokenize | 2,066,382 | 23.80 | 1,378,356 | 70,716 |
| Encrypt, value over limit (fail closed) | 1,260,324 | 52.01 | 351,852 | 65,587 |

Encryption is about 1.84x slower than replacement redaction in this dense-match
workload; tokenization is about 1.55x slower. The difference grows with the
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
| Capture only, memory store | 34,082 | 1,443.04 | 102,761 | 163 |
| Response redaction | 1,450,479 | 33.91 | 693,686 | 59,604 |
| Request + response redaction | 2,937,308 | 33.49 | 1,411,882 | 119,100 |
| Response encryption | 2,719,965 | 18.08 | 2,694,971 | 69,866 |
| Response tokenization | 2,234,786 | 22.01 | 1,596,395 | 70,889 |
| Gzip decode + response redaction | 1,553,182 | 31.67 | 814,598 | 59,629 |
| Custom pass-through redactor | 34,559 | 1,423.12 | 102,471 | 157 |
| Pass-through capture policy callback | 33,706 | 1,459.14 | 102,758 | 163 |
| `FileBodyStore` + response redaction | 1,666,313 | 29.52 | 637,072 | 59,611 |

The custom-redactor adapter and capture-policy callback add no meaningful cost
at this payload size when their own logic is trivial. Gzip decoding reduces
redaction throughput by about 7%. The file-store result includes temp-file
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

## Performance-critical guidance

- Leave body capture disabled unless the recorded payload is operationally
  necessary. Production-safe defaults already follow this rule.
- Apply `BodyCapturePolicy` early to exclude high-volume endpoints or directions;
  a pass-through policy is cheap, while avoiding parsing is a large saving.
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

## Current optimization targets

The most important finding is allocation pressure in the handwritten structured
parsers: the dense JSON case performs roughly 59k allocations for a ~48 KiB
body, and XML/form also allocate per lexical unit. This is correct and bounded,
but it is not allocation-efficient. The next optimization pass should profile
these benchmarks with `-memprofile` and focus on reusable token buffers and
avoiding short-lived string/byte conversions without weakening malformed-input
fail-closed behavior or byte-preservation guarantees.

Treat those allocation counts as regression baselines. New features should not
silently increase them; performance changes should include before/after
`benchstat` output and the same correctness, race, and fuzz checks used for the
stream redactors.
