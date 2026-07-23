# Benchmarks

This document describes the benchmark suite, records a reproducible performance
snapshot, and calls out the paths where configuration choices materially affect
CPU, allocation, memory, or I/O cost. Benchmark results are comparative data,
not universal capacity promises: rerun them on the deployment hardware and Go
toolchain used in production.

## Reproducing the results

The snapshot below was collected on 2026-07-23 with:

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

Run the core recorder overhead cases with:

```sh
GOMAXPROCS=1 go test -run '^$' \
  -bench '^(BenchmarkBaselineNoRecorder|BenchmarkHeadSampleDrop|BenchmarkCaptureDisabled|BenchmarkHeaderOnlyCapture|BenchmarkSmallBody|Benchmark1MBBody)$' \
  -benchmem -benchtime=300ms -count=1
```

Run the intentionally large streaming cases once per configuration:

```sh
GOMAXPROCS=1 go test -run '^$' \
  -bench '^(Benchmark100MBStreamingBody|Benchmark100MBStreamingBodyNoHash)$' \
  -benchmem -benchtime=1x -count=1
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

Run the bounded fixture ingestion and deterministic replay benchmarks with:

```sh
GOMAXPROCS=1 go test -run '^$' \
  -bench '^(BenchmarkFixtureReaders|BenchmarkReplayExactRequests)$' \
  -benchmem -benchtime=300ms -count=1 ./hario ./hartest
```

For comparison work, prefer `-count=5` and feed the before/after outputs to
`benchstat`. Avoid comparing results collected with different Go versions,
power modes, `GOMAXPROCS` values, or storage devices.

## What is measured

The suite has three layers:

1. `BenchmarkStreamRedactors` and `BenchmarkSensitiveValueProtection` measure
   the streaming transform itself. They cover JSON, NDJSON, XML,
   `application/x-www-form-urlencoded`, and multipart bodies; 32-byte, 4 KiB,
   and whole-body writes; sparse and dense matches; redact, AES-256-GCM encrypt,
   HMAC-SHA-256 tokenize, and oversized-value fail-closed behavior.
2. `BenchmarkTransportBodyPipeline` measures the same work through the complete
   recorder Transport. It covers response-only and request-plus-response
   capture, gzip decoding, a custom `BodyRedactor`, a capture policy callback,
   `FileBodyStore` with embedding disabled, and parallel redaction.
3. `BenchmarkFixtureReaders` and `BenchmarkReplayExactRequests` measure the
   non-core fixture toolchain separately: bounded HAR/NDJSON collection and
   pull streaming, followed by strict method, URL, header, and body replay.

All built-in redactor benchmarks include writer construction, streaming writes,
parser finalization, protection reporting, and output emission to `io.Discard`.
Transport benchmarks additionally include request construction, HTTP framing,
body lifecycle accounting, entry construction, and audit collection.

## Core recorder overhead

These cases use a 1 KiB response unless otherwise noted.

| Case | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Bare `net/http` baseline | 10,507 | 97.46 | 5,747 | 67 |
| Head-sampled Drop fast path | 10,812 | 94.71 | 5,746 | 67 |
| Recorder, capture disabled | 15,222 | 67.27 | 11,684 | 138 |
| Header-only capture | 15,747 | 65.03 | 12,155 | 147 |
| 1 KiB captured, embedded, SHA-256 | 26,516 | 38.62 | 57,887 | 214 |
| 1 MiB captured, embedded, SHA-256 | 858,293 | 1,221.70 | 3,201,311 | 223 |

The recorder wrapper with capture disabled adds about 4.7 µs and 71 allocations
to this deliberately low-latency in-memory baseline. Real network latency makes
the relative percentage smaller, but the absolute local overhead remains
relevant for very high request rates.

The Drop row bypasses recorder initialization, request cloning, `httptrace`,
exchange/redactor state, and body wrappers. Its allocation profile remains
effectively equal to the separately measured bare baseline, and their timing
ranges overlap. Head-policy execution still consumes CPU and should remain
bounded.

## Streaming redactors

Payloads in this group are approximately 42–94 KiB and intentionally contain
many structured values. The table reports the 4 KiB write case, which is the
most representative default for streaming comparisons.

| Format and match density | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| JSON, sparse | 331,888 | 145.11 | 1,008 | 11 |
| JSON, no matching field | 386,503 | 132.55 | 984 | 9 |
| JSON, dense | 356,569 | 137.93 | 1,008 | 11 |
| NDJSON, dense | 314,522 | 133.48 | 1,008 | 11 |
| XML, dense | 532,472 | 136.56 | 936 | 10 |
| Form, dense | 275,019 | 223.42 | 832 | 7 |
| Multipart, dense | 207,592 | 450.45 | 10,786 | 13 |

### Chunk-size sensitivity

| Format | 32-byte writes | 4 KiB writes | Whole body | Observation |
| --- | ---: | ---: | ---: | --- |
| JSON dense | 135.03 MB/s | 137.93 MB/s | 137.08 MB/s | Essentially insensitive |
| NDJSON dense | 129.64 MB/s | 133.48 MB/s | 133.86 MB/s | Small writes cost about 3% |
| XML dense | 128.73 MB/s | 136.56 MB/s | 137.15 MB/s | Small writes cost about 6% |
| Form dense | 211.69 MB/s | 223.42 MB/s | 224.53 MB/s | Small writes cost about 6% |
| Multipart dense | 469.65 MB/s | 450.45 MB/s | 515.51 MB/s | Whole-body input is about 14% faster |

The parsers preserve streaming behavior across chunk boundaries. Artificially
coalescing normal 4–64 KiB reads is unlikely to help materially. Multipart
whole-body input is somewhat faster in this synthetic case, but reusable
boundary lookbehind and pending-buffer compaction keep even 32-byte writes at
the same 13–14 allocation baseline.

## Protection modes

This comparison uses the same dense JSON body and 4 KiB writes.

| Mode | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Redact | 362,674 | 135.61 | 1,008 | 11 |
| AES-256-GCM encrypt | 771,002 | 63.79 | 1,273 | 14 |
| HMAC-SHA-256 tokenize | 532,161 | 92.42 | 1,920 | 19 |
| Encrypt, value over limit (fail closed) | 435,745 | 150.44 | 286,696 | 35 |

Encryption is about 2.13x slower than replacement redaction in this dense-match
workload; tokenization is about 1.47x slower. The difference grows with the
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
| Capture only, memory store | 35,311 | 1,392.84 | 103,227 | 169 |
| Response redaction | 609,561 | 80.68 | 104,376 | 185 |
| Request + response redaction | 1,210,100 | 81.29 | 232,465 | 255 |
| Response encryption | 1,090,999 | 45.08 | 453,120 | 201 |
| Response tokenization | 814,219 | 60.40 | 222,173 | 201 |
| Gzip decode + response redaction | 632,824 | 77.72 | 225,225 | 209 |
| Custom pass-through redactor | 35,604 | 1,381.35 | 103,072 | 166 |
| Pass-through capture policy callback | 35,812 | 1,373.32 | 103,224 | 169 |
| `FileBodyStore` + response redaction | 854,871 | 57.53 | 49,441 | 203 |

The custom-redactor adapter and capture-policy callback add no meaningful cost
at this payload size when their own logic is trivial. The roughly 3% difference
between plain and gzip-decoded redaction is within normal benchmark noise at
this payload size. The file-store result includes partial-file
creation, streaming writes, atomic commit into `assets/`, opaque-reference
publication, and explicit release on the benchmark machine; storage hardware
and filesystem behavior will dominate its portability.

With `GOMAXPROCS=8`, `BenchmarkTransportRedactionParallel` processed the same
dense response at 159,355 ns/op and 308.63 MB/s (7,593 iterations in the sample).
That is roughly 3.8x the single-worker throughput, not linear 8x scaling. The
benchmark exercises shared Transport/recorder operation and is intended to
catch contention or race-driven regressions; repeat it at the production
`GOMAXPROCS` value rather than treating this machine-specific ratio as a limit.

## Large streaming and hashing

The 100 MiB cases use one iteration to avoid turning normal test runs into a
long stress test. Capture is limited to 1 MiB; remaining bytes are counted and,
when enabled, hashed.

| Case | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| SHA-256 enabled | 71,648,208 | 1,463.51 | 2,258,648 | 1,933 |
| Hashing disabled | 10,882,667 | 9,635.29 | 2,229,880 | 1,870 |

On this CPU, SHA-256 is the dominant cost after the capture limit: the measured
run is about 6.6x slower than byte counting alone. Disable body hashes only when
their integrity and correlation value is not needed.

## Bounded asynchronous delivery

These microbenchmarks isolate `Recorder.Record` dispatch. The bounded-block
case uses a 1,024-entry queue and a no-op downstream worker; the full-drop case
holds a one-entry queue full so every measured call takes the explicit
`AsyncDropNewest` path. The batch case uses a 64-entry reusable worker buffer
and a no-op sink exposing `RecordBatch([]*Entry) error` with no linger interval.

| Case | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Direct no-op recorder | 2.22 | 0 | 0 |
| `AsyncRecorder`, bounded/default block | 43.39 | 0 | 0 |
| `AsyncRecorder`, batch size 64 | 17.71 | 0 | 0 |
| Full queue, `AsyncDropNewest` | 13.64 | 0 | 0 |
| Full queue, block timeout → drop newest | 292.2 | 128 | 2 |

The uncontended async queue adds about 40 ns to this synthetic no-op baseline
and performs no per-entry heap allocation. This is not a sink-latency result:
the purpose of the decorator is to move downstream I/O off the finalizing
goroutine until the queue fills. With the default `AsyncBlock` policy, a full
queue intentionally transfers sink backpressure to the application and latency
then approaches the rate at which the sink frees capacity. Benchmark and alert
on blocked duration with a representative sink and queue size; choosing a drop
policy changes the evidence-completeness contract, not merely performance.
The timeout case uses a one-nanosecond deadline to force the exceptional path;
its timer accounts for the two allocations. Normal queue admission retains the
allocation-free hot path, while an actually blocked producer pays this bounded
coordination cost once.
The synthetic batch path reduces queue-lock handoffs and still performs no
per-entry allocation; real gains depend on whether the sink can coalesce
encoding, writes, flushes, or transactions.

## Bounded in-memory retention

These microbenchmarks use a 1,024-entry `MemoryRecorder`. The record case is
already full and therefore measures steady-state oldest-entry eviction. The
snapshot case reads a wrapped ring into an oldest-to-newest result slice.

| Case | ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| Full ring, record and evict oldest | 13.56 | 0 | 0 |
| Wrapped ring, snapshot 1,024 entries | 3,097 | 9,472 | 1 |

Steady-state retention performs no per-entry heap allocation. `Entries` and
`Snapshot` intentionally allocate one result slice so callers cannot mutate
the recorder's ring; their cost scales linearly with retained capacity.

## Fixture ingestion and deterministic replay

The reader cases decode 1,000 small POST exchanges. Collector variants retain
the result slice; stream variants pull and discard each validated entry. Replay
constructs a strict fixture transport and consumes 256 distinct exchanges per
operation, matching method, URL, `Content-Type`, and JSON body.

| Case | ns/op | MB/s | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| HAR collector, 1,000 entries | 12,968,863 | 46.74 | 2,129,154 | 30,814 |
| HAR pull stream, 1,000 entries | 12,424,842 | 48.78 | 2,111,641 | 30,804 |
| NDJSON collector, 1,000 entries | 8,192,266 | 73.97 | 2,113,958 | 30,506 |
| NDJSON pull stream, 1,000 entries | 8,291,210 | 73.09 | 2,096,443 | 30,496 |
| Strict replay, 256 exchanges | 673,468 | — | 924,937 | 10,048 |

Streaming avoids retaining the collector result slice, but each returned entry
is still fully decoded and validated; the allocation difference is therefore
small in this fixture shape. NDJSON avoids HAR envelope token traversal and is
the faster ingestion format here. Replay numbers include transport creation,
request construction, matching, response reconstruction, body consumption, and
unused-fixture verification. The strict default path prepares immutable fixture
URL, query, header, and trailer metadata once, rejects identity mismatches
before opening bodies, and avoids isolated snapshot copies. A configured
request normalizer deliberately retains the copy-on-normalize path so it cannot
mutate live requests or source fixtures. These helpers are designed for
deterministic test fixtures rather than the production capture hot path. Replay
throughput is omitted because the tiny fixture-body byte count does not
represent its method, URL, header, body-matching, and response-reconstruction
workload.

## Performance-critical guidance

- Leave body capture disabled unless the recorded payload is operationally
  necessary. Production-safe defaults already follow this rule.
- Apply `BodyCapturePolicy` early to exclude high-volume endpoints or directions;
  a pass-through policy is cheap, while avoiding parsing is a large saving.
- Prefer request-scoped additive redaction rules over constructing a Transport
  per endpoint. Rules are copied when attached and resolved once per exchange,
  while the shared client's connection pool remains reusable.
- Prefer `FileBodyStore` with `Config.EmbedBodies = false` for large retained
  bodies. This bounds HAR memory growth, but moves throughput and retention
  concerns to
  the filesystem; enforce byte/file quotas and explicitly release or reconcile
  committed assets according to application ownership. Enabling sync-on-commit
  adds filesystem durability work to body finalization and should be benchmarked
  on the production volume.
- Redaction cost scales with structured tokens and matching values. Dense JSON
  plus encryption/tokenization is intentionally the worst normal case in this
  suite. Benchmark representative schemas and secret density.
- Encryption buffers one protected value up to `MaxValueBytes`; tokenization
  streams through HMAC. Recorder resolves key material once per mode and
  exchange, sharing the snapshot across request and response; a remote provider
  may still cache across exchanges. Keep the encryption limit no larger than
  required.
- Hashing continues after the capture limit so hashes describe all bytes read.
  This is useful but CPU-visible for very large streams.
- Gzip decoding is required before structured redaction. Compression bombs are
  bounded by capture limits, but compressed traffic still consumes decoder CPU.
- Multipart boundary lookbehind and header parsing reuse bounded state across
  writes. Normal upstream read sizes remain preferable, but tiny writes no
  longer cause per-part allocation churn.
- Custom redactors execute on the request/response read path. They should remain
  streaming, bounded, panic-safe, and free of blocking external calls.
- `AsyncRecorder` uses `AsyncBlock` by default so queue pressure does not
  silently discard evidence. Size the queue for expected bursts and monitor
  blocked producers plus oldest active block age. When an application latency
  budget must survive a stalled sink, configure a bounded block timeout and an
  explicit fallback; any drop handler adds its own latency and must remain
  bounded. The queue is not crash-durable.
- `MemoryRecorder` retains a capped entry count but entry size still depends on
  capture configuration. Size both the ring and request/response body limits;
  monitor its lifetime eviction count, and do not treat a post-eviction trace
  as complete evidence.

## Current optimization targets

Single-byte output previously used `dst.Write([]byte{b})`, and protected-value
suppression passed each byte through a variadic slice. JSON object keys also
used `json.Unmarshal` even when they were ordinary unescaped ASCII. Reusable
byte sinks, direct single-byte protection writes, an allocation-free ASCII key
matcher, and reusable JSON protection output reduced the no-match JSON case
from roughly 9.2k allocations and 514 KiB to 9 allocations and under 1 KiB.
Dense fixed redaction fell from roughly 18.5k allocations to 11.
Exchange-scoped key and AES-GCM derivation, reusable ciphertext/token encoding
buffers, reusable HMAC state, and buffered token input reduced dense encryption
from about 7.2k allocations and 1.5 MiB to 14 allocations and about 1 KiB.
Dense tokenization fell from roughly 20.5k allocations and 827 KiB to 19
allocations and about 2 KiB. `BodyValue.FinishTo` writes the reusable encoded
bytes directly to the destination, so selected body values no longer require
an intermediate immutable token string. The oversized fail-closed encryption
path remains bounded at 35 allocations.
Allocation-free ASCII name matching and reusable suppression-name storage also
reduced dense XML from about 8.2k allocations and 41 KiB to 10 allocations and
under 1 KiB. The equivalent form-key fast path reduced dense form processing
from about 4.1k allocations and 33 KiB to 7 allocations and under 1 KiB.
Multipart now precomputes boundary search markers, scans header lines without
`bytes.Split`, uses a standard-library-checked fast path for ordinary form-data
parameters, and compacts only small pending tails. The representative 4 KiB
case fell from about 7.2k allocations and 574 KiB to 13 allocations and 11 KiB;
escaped, extended, duplicate, and otherwise complex parameters retain
`mime.ParseMediaType` validation.

Escaped, malformed, and non-ASCII keys retain the full JSON decoding path so
Unicode case folding and redaction correctness are unchanged. A tolerant
cross-toolchain allocation test prevents a return to per-key decoding churn.
Remaining body encryption and tokenization allocations are fixed stream and
exchange setup rather than per-value token objects. Structural destinations
that require Go strings, such as HAR scalar fields, CSV cells, or multipart
filename parameters, still materialize one deliberately. Allocation regression
tests keep AEAD/HMAC construction, encoding, and body output out of the
per-value baseline. Future optimization work should be driven by fresh profiles
rather than pooling plaintext-bearing parser state speculatively.

Treat the documented allocation counts as regression baselines. New features
should not silently increase them; performance changes should include
before/after `benchstat` output and the same correctness, race, and fuzz checks
used for the stream redactors.
