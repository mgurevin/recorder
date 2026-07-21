# otelrecorder

`otelrecorder` is the optional OpenTelemetry adapter for
[`github.com/mgurevin/recorder`](../README.md). It exports finalized HTTP
exchanges as span events and metrics while keeping the core recorder module
free of OpenTelemetry dependencies.

The adapter is intended to answer four different operational questions:

1. Are outbound HTTP exchanges slow or failing, and in which phase?
2. Is body capture and sensitive-value protection behaving as configured?
3. Is asynchronous delivery applying backpressure or losing evidence?
4. Are managed body storage and sampling healthy?

It does not export request paths, query strings, headers, cookies, bodies,
error messages, encryption keys, protected values, body-store paths, sampling
keys, or sink identities as metric attributes.

## Requirements and installation

- Go 1.25 or newer
- an OpenTelemetry `TracerProvider` and/or `MeterProvider`
- an SDK reader/exporter configured by the application when telemetry must
  leave the process

```sh
go get github.com/mgurevin/recorder/otelrecorder
```

Creating this adapter does not install an OTLP exporter and does not replace
the application's global OpenTelemetry providers. By default it uses
`otel.GetTracerProvider()` and `otel.GetMeterProvider()`; pass explicit
providers when the application does not use the globals.

## Basic integration

```go
exporter, err := otelrecorder.NewExporter(
	otelrecorder.WithCreateSpanIfNone(false),
	otelrecorder.WithSpanErrorStatus(true),
)
if err != nil {
	return err
}
defer exporter.Close()

transport := recorder.NewTransport(http.DefaultTransport, rec,
	recorder.WithOnEntryCompleted(exporter.OnEntryCompleted),
)

client := &http.Client{Transport: transport}
```

`OnEntryCompleted` runs synchronously when an instrumented exchange is
finalized. The adapter records its metrics and span event during that callback;
it does not retain the entry or body assets.

An entry is not finalized until the response body reaches EOF, is closed, or
fails. A caller that neither consumes nor closes the body produces neither the
entry nor its per-exchange telemetry. Head-dropped exchanges are intentionally
uninstrumented and likewise produce no per-exchange metrics or span event.

## Optional health metrics

`AsyncRecorder`, `FileBodyStore`, and sampling expose process-local snapshots.
Pass the same live instances to an exporter to register their observable
metrics:

```go
asyncRec, err := recorder.NewAsyncRecorder(sink,
	recorder.WithAsyncQueueCapacity(1024),
)
if err != nil {
	return err
}

store, err := recorder.NewFileBodyStore("./spool")
if err != nil {
	return err
}

transport := recorder.NewTransport(http.DefaultTransport, asyncRec,
	recorder.WithBodyStore(store),
)

exporter, err := otelrecorder.NewExporter(
	otelrecorder.WithAsyncRecorder(asyncRec),
	otelrecorder.WithFileBodyStore(store),
	otelrecorder.WithSamplingTransport(transport),
)
if err != nil {
	return err
}
defer exporter.Close()

transport.Options.OnEntryCompleted = exporter.OnEntryCompleted
```

Configure all `Transport` fields before its first request. The explicit field
assignment above solves the construction cycle between the Transport and the
sampling-aware exporter; do not mutate the Transport afterward.

`Exporter.Close` unregisters observable metric callbacks. It does not flush or
shut down the application's OTel providers, close the `AsyncRecorder`, drain
its queue, or close the body store. Those lifecycles remain application-owned.

## Units and instrument semantics

Units use OpenTelemetry/UCUM notation:

- `ms`: milliseconds. Histogram values may be fractional.
- `By`: bytes, not bits and not KiB/MiB.
- `{entry}`, `{file}`, `{producer}`, `{batch}`, `{record}`, `{operation}`,
  `{decision}`, `{failure}`, `{error}`, and `{panic}`: dimensionless annotated
  counts of the named item.
- an empty unit on a counter also means a dimensionless count.

Synchronous counters are monotonic sums. Observable counters expose monotonic
process-lifetime snapshots from the underlying component. Alert on their
increase/rate over a window, not their absolute value. Process restarts reset
them.

Gauges describe the latest observed state. Histograms describe a distribution;
use count/rate, percentile, and bucket-based SLO calculations rather than
averaging percentiles across instances.

## HTTP exchange metrics

These instruments are emitted by `OnEntryCompleted`.

| Instrument | Type | Unit | Meaning |
| --- | --- | --- | --- |
| `recorder.http.client.duration` | histogram | `ms` | Total finalized exchange duration |
| `recorder.http.client.phase.duration` | histogram | `ms` | Measured `blocked`, `dns`, `connect`, `tls`, `send`, `wait`, or `receive` phase duration |
| `recorder.http.client.request.body.size` | histogram | `By` | Request bytes observed on the HTTP stream, independent of the capture limit |
| `recorder.http.client.response.body.size` | histogram | `By` | Response bytes observed on the HTTP stream, independent of the capture limit |
| `recorder.http.client.failures` | counter | count | Transport/body failures, split by bounded failure phase |
| `recorder.http.client.closed_early` | counter | count | Response bodies closed before EOF |
| `recorder.http.client.body.truncated` | counter | count | Request or response captures that reached the configured capture limit |

Every available phase is one histogram measurement. An unobserved HAR timing
has value `-1` internally and is omitted rather than recorded as latency.
`wait` is time to first response byte; it is the closest phase-level signal to
server/proxy response latency, while `receive` reflects body consumption and
can also include caller reading behavior.

The common bounded metric attributes are:

| Attribute | Values/meaning |
| --- | --- |
| `http.request.method` | clamped method string |
| `http.response.status_class` | `0`, `1xx`, `2xx`, `3xx`, `4xx`, or `5xx` |
| `recorder.state` | terminal recorder state |
| `url.scheme` | URL scheme only |
| `network.protocol.name` | currently `http` when known |
| `network.protocol.version` | normalized version such as `1.1`, `2`, or `3` |
| `recorder.http.phase` | phase name, phase histogram only |
| `recorder.error.phase` | bounded recorder failure phase, failure counter only |
| `recorder.body.direction` | `request` or `response`, directional counters only |

Exact URLs, hosts and paths are deliberately absent from default metric
dimensions. To distinguish services or routes, add bounded deployment/resource
attributes in the OTel SDK or return a route template from
`WithMetricAttributes`; never attach raw paths, IDs, or customer values.

## Capture and redaction metrics

| Instrument | Type | Unit | Attributes and meaning |
| --- | --- | --- | --- |
| `recorder.body.captured.size` | histogram | `By` | Retained bytes; `recorder.body.direction` |
| `recorder.body.capture.operations` | counter | count | `complete`, `truncated`, `closed_early`, `failed`, or `incomplete` outcome by direction |
| `recorder.redaction.values` | counter | count | Protected values by request/response direction and `redacted`, `encrypted`, or `tokenized` mode |
| `recorder.redaction.fallbacks` | counter | count | Fail-closed replacements by direction and fixed reason |
| `recorder.body.redaction.operations` | counter | count | Body-redactor execution by direction, bounded kind, and bounded outcome |

Additional bounded attributes are:

- `recorder.body.capture.outcome`: `complete`, `truncated`, `closed_early`,
  `failed`, or `incomplete`.
- `recorder.redaction.direction`: `request` or `response`.
- `recorder.protection.mode`: `redacted`, `encrypted`, or `tokenized`.
- `recorder.protection.reason`: `value_too_large`, `encryption_failed`,
  `tokenization_failed`, or `other`.
- `recorder.body.redaction.kind`: `custom`, `builtin:multipart`,
  `builtin:form`, `builtin:json`, `builtin:xml`, `builtin:sniff`, or `other`.
- `recorder.body.redaction.outcome`: `redacted`, `unchanged`, `failed`, or
  `other`.

`request/response.body.size` measures all streamed bytes; `body.captured.size`
measures only bytes retained after limits and transformations. Their difference
is expected when capture is disabled or bounded and must not be interpreted as
network loss.

## AsyncRecorder health metrics

Enabled by `WithAsyncRecorder`.

| Instrument | Type | Unit | Meaning |
| --- | --- | --- | --- |
| `recorder.async.queue.depth` | gauge | `{entry}` | Entries waiting in the queue |
| `recorder.async.queue.capacity` | gauge | `{entry}` | Configured queue capacity |
| `recorder.async.producers.blocked` | gauge | `{producer}` | Producers currently blocked for capacity |
| `recorder.async.entries.in_flight` | gauge | `{entry}` | Entries currently being delivered downstream |
| `recorder.async.block.max` | gauge | `ms` | Largest producer block duration seen since process start |
| `recorder.async.block.oldest_age` | gauge | `ms` | Current age of the oldest blocked producer |
| `recorder.async.batch.size.max` | gauge | `{entry}` | Largest delivery batch observed since process start |
| `recorder.async.entries.accepted` | observable counter | `{entry}` | Entries accepted into async delivery |
| `recorder.async.entries.processed` | observable counter | `{entry}` | Downstream record attempts |
| `recorder.async.batches.processed` | observable counter | `{batch}` | Downstream batch attempts |
| `recorder.async.records.blocked` | observable counter | `{record}` | Record calls that had to wait for capacity |
| `recorder.async.block.duration` | observable counter | `ms` | Cumulative producer blocking time |
| `recorder.async.entries.dropped` | observable counter | `{entry}` | Dropped entries by fixed reason |
| `recorder.async.sink.panics` | observable counter | `{panic}` | Recovered downstream panics |
| `recorder.async.sink.errors` | observable counter | `{error}` | Observable downstream record/close errors |
| `recorder.async.drop_handler.panics` | observable counter | `{panic}` | Recovered asset drop-handler panics |

`recorder.async.entries.dropped` has the bounded attribute
`recorder.async.drop.reason`: `policy_newest`, `policy_oldest`,
`timeout_newest`, `timeout_oldest`, or `closed`.

`block.max` and `batch.size.max` are lifetime maxima. They are useful for
diagnosis but normally unsuitable for a self-clearing alert. Prefer
`block.oldest_age`, current blocked producers, queue utilization, and rates of
counter increase for paging conditions.

## FileBodyStore health metrics

Enabled by `WithFileBodyStore`.

| Instrument | Type | Unit | Meaning |
| --- | --- | --- | --- |
| `recorder.body.store.files` | gauge | `{file}` | Current `partial` or `committed` files |
| `recorder.body.store.bytes` | gauge | `By` | Current bytes in `partial` or `committed` files |
| `recorder.body.store.capacity.files` | gauge | `{file}` | Configured file limit |
| `recorder.body.store.capacity.bytes` | gauge | `By` | Configured byte limit |
| `recorder.body.store.operations` | observable counter | `{operation}` | Lifecycle outcomes |

State is carried in `recorder.body.store.state` (`partial` or `committed`).
Operation outcome is carried in `recorder.body.store.outcome`: `committed`,
`aborted`, `released`, `quota_rejected`, `recovered_partial`, `write_failed`,
`commit_failed`, `abort_failed`, or `release_failed`.

Capacity utilization is meaningful only when the corresponding configured
capacity is greater than zero. Partial assets should normally be short-lived;
a sustained non-zero partial count/byte total indicates abandoned or stalled
capture work and deserves investigation.

## Sampling health metrics

Enabled by `WithSamplingTransport`.

| Instrument | Type | Unit | Attributes and meaning |
| --- | --- | --- | --- |
| `recorder.sampling.head.decisions` | observable counter | `{decision}` | `full`, `metadata_only`, or `drop` decisions |
| `recorder.sampling.retention.outcomes` | observable counter | `{decision}` | Final `retained` or successfully `discarded` outcomes |
| `recorder.sampling.policy.failures` | observable counter | `{failure}` | Policy `panic` or `invalid_decision`, at `head` or `retention` stage |
| `recorder.sampling.asset_release.failures` | observable counter | `{failure}` | Failed managed-asset cleanup for a requested discard |

Attributes are `recorder.sampling.decision`, `recorder.sampling.stage`, and
`recorder.sampling.failure.reason`. No sampling key, trace ID, host, or path is
exported. Asset-release failure causes retention to fail open, so the entry is
kept rather than silently orphaning its external evidence.

## Span events

Each finalized exchange adds a `recorder.http.exchange` event to the active
span. If no span is recording, the default is metrics only. Set
`WithCreateSpanIfNone(true)` to synthesize a short client span named
`HTTP <METHOD>` covering the recorded exchange interval.

Event attributes include the low-cardinality HTTP attributes, total and
observed phase durations in milliseconds, streamed body sizes in bytes,
connection reuse/idle and HTTP/2 indicators, and a bounded TLS version/cipher
summary. `WithIncludeIDs(true)` adds recorder trace and exchange IDs to span
events only; these high-cardinality identifiers never become default metric
attributes. `WithSpanErrorStatus(true)` marks a touched span as error when the
entry contains a recorder transport/body failure.

| Event attribute | Type/unit | Meaning |
| --- | --- | --- |
| `http.request.method` | string | HTTP method |
| `http.response.status_code` | integer | Exact response status code |
| `url.scheme` | string | Request URL scheme |
| `server.address` | string | Hostname only; no port, userinfo, path, or query |
| `network.protocol.name` / `network.protocol.version` | string | Normalized HTTP protocol |
| `recorder.state` / `recorder.error.phase` | string | Terminal state and optional failure phase |
| `recorder.duration_ms` | float, `ms` | Total exchange duration |
| `recorder.timings.blocked`, `.dns`, `.connect`, `.ssl`, `.send`, `.wait`, `.receive` | float, `ms` | Observed HAR phase duration; unmeasured phases are absent |
| `recorder.request.body.bytes` / `recorder.response.body.bytes` | integer, `By` | Streamed body bytes |
| `recorder.request.body.truncated` / `recorder.response.body.truncated` | boolean | Capture limit reached |
| `recorder.response.decoded` | boolean | Record-time content decoding occurred |
| `recorder.response.closed_early` | boolean | Caller closed before EOF |
| `recorder.network.reused`, `.was_idle`, `.dns_coalesced`, `.http2` | boolean | Bounded connection summary |
| `recorder.tls.version` / `recorder.tls.cipher_suite` | string | Bounded negotiated TLS summary |
| `recorder.tls.resumed` | boolean | TLS session resumed |
| `recorder.trace_id` / `recorder.exchange_id` | string | Opt-in correlation identifiers |

Boolean attributes that would only repeat the default `false` state are
generally omitted. `recorder.network.reused` and `recorder.network.http2` are
the exceptions and are emitted whenever network information exists.

## Alerting scenarios

The examples below are backend-neutral conditions. Choose windows and
thresholds from traffic volume, latency SLOs, queue capacity, storage budget,
and evidence-loss tolerance. Require a minimum event count before calculating
ratios to avoid noisy low-traffic alerts.

### Upstream availability degradation

**Signal:** increase rate of `recorder.http.client.failures`, optionally split
by `recorder.error.phase`, plus the ratio of `5xx` duration histogram count to
all completed exchange count.

**Scenario:** DNS failures isolated to `dns` suggest resolver/network trouble;
`tls` suggests certificate or handshake trouble; `read_response_body` suggests
mid-stream failures. A 5xx spike with low transport failures points instead to
a reachable but unhealthy upstream.

**Typical response:** page when the error ratio breaches the service SLO for
several windows; create a lower-severity ticket for a small sustained increase.

### Latency and phase regression

**Signal:** p95/p99 of `recorder.http.client.duration`, then the same
percentiles of `recorder.http.client.phase.duration` split by
`recorder.http.phase`.

**Scenario:** rising `dns`, `connect`, or `tls` identifies connection setup;
rising `wait` points toward upstream/proxy processing; rising `receive` may be
large payloads, a slow peer, or slow caller consumption.

**Typical response:** alert on an SLO burn rate or a sustained percentile
threshold. Use phase histograms for diagnosis, not as seven independent pages.

### Caller body-lifecycle bug

**Signal:** positive rate of `recorder.http.client.closed_early`, capture
outcomes `closed_early`/`incomplete`, and response failures during body read.

**Scenario:** application code is closing before EOF, abandoning work, or
encountering repeated body failures. This can reduce connection reuse and leave
the recorded evidence incomplete.

**Typical response:** any sustained non-zero ratio can be actionable for APIs
that require complete evidence. Exclude endpoints where partial reads are an
intentional, reviewed behavior.

### Capture policy pressure

**Signal:** rate/ratio of `recorder.http.client.body.truncated` and capture
outcome `truncated`; compare streamed size and captured-size distributions.

**Scenario:** payloads have outgrown configured capture limits. This is not an
HTTP failure, but the retained evidence may no longer satisfy investigation or
audit requirements.

**Typical response:** ticket on an unexpected sustained increase; page only
when complete body evidence is a hard control requirement.

### Protection failure or attack-sized values

**Signal:** any increase in `recorder.redaction.fallbacks`, split by fixed
reason and direction; body-redaction outcome `failed`.

**Scenario:** encryption/tokenization is failing, selected values exceed the
allowed protected-value size, or a custom/built-in redactor cannot complete.
The library fails closed to `[REDACTED]`, protecting confidentiality while
reducing recoverability.

**Typical response:** page immediately for `encryption_failed` or
`tokenization_failed` in environments that require reversible/tokenized
evidence. Investigate `value_too_large` as either schema drift or hostile input.

### AsyncRecorder saturation and evidence loss

**Signal:** queue depth/capacity ratio, blocked producer count,
`block.oldest_age`, rate of blocked records, and rate of dropped entries.

**Scenario:** a slow or unavailable sink fills the queue. With `AsyncBlock`,
HTTP finalization backpressure grows before evidence is lost; with a drop policy
or timeout fallback, `entries.dropped` is direct evidence loss.

**Typical response:** warn on sustained high utilization (for example a
deployment-calibrated 70–80%); page when producers remain blocked beyond the
allowed request latency or whenever drop rate is non-zero under an
evidence-preserving policy.

### Async sink failure

**Signal:** any increase in `recorder.async.sink.panics`,
`recorder.async.sink.errors`, or `recorder.async.drop_handler.panics`.

**Scenario:** the downstream sink or cleanup integration is malfunctioning.
A drop-handler panic is especially important with external body assets because
cleanup may not have completed.

**Typical response:** page on any increase; these should be exceptional and
are not ordinary capacity signals.

### FileBodyStore capacity or lifecycle failure

**Signal:** committed/partial bytes or files divided by configured capacity;
increases in `quota_rejected`, `write_failed`, `commit_failed`,
`abort_failed`, or `release_failed`; persistent partial assets.

**Scenario:** disk budget is nearly exhausted, filesystem I/O is failing, or
asset ownership/cleanup is unhealthy. `recovered_partial` immediately after a
restart is expected recovery; a recurring rate suggests repeated unclean
shutdowns.

**Typical response:** warn before capacity exhaustion and page on quota
rejection or write/commit failure when body evidence is required. Treat a
sustained release failure as both capacity and sensitive-data-retention risk.

### Sampling policy malfunction or unexpected evidence reduction

**Signal:** any increase in `recorder.sampling.policy.failures` or
`asset_release.failures`; compare the rates of `full`, `metadata_only`, and
`drop` against the configured target.

**Scenario:** a policy panicked/returned an invalid decision, managed cleanup
failed, or a rollout changed the evidence mix unexpectedly. Policy failures
fail open; asset cleanup failures retain entries, so both can increase cost
even though they avoid silent evidence loss.

**Typical response:** page on failures. Alert on decision-ratio drift only with
enough traffic and an understood deterministic sampler distribution.

## Cardinality and data-safety controls

The default metric dimensions are bounded by construction. String attributes
are clamped to 128 bytes by default; configure `WithMaxAttributeLength` to
change the limit. Custom span-event and metric callbacks are capped at 16
attributes each.

`WithMetricAttributes` is powerful and can defeat these protections. Return
only bounded values such as a route template, deployment tier, or known peer
name. Never return a raw URL/path, tenant/customer/account ID, trace ID, error
text, header, cookie, body fragment, protected token, or encryption material.

Span events may safely carry more diagnostic detail than metrics, but they are
still exported data. `WithSpanEventAttributes` and `WithIncludeIDs` should be
enabled only under the application's telemetry data-governance policy.

## Option reference

| Option | Purpose |
| --- | --- |
| `WithTracerProvider` | Use an explicit tracer provider |
| `WithMeterProvider` | Use an explicit meter provider |
| `WithCreateSpanIfNone` | Synthesize a client span when none is recording |
| `WithSpanErrorStatus` | Set span status to error for recorded failures |
| `WithIncludeIDs` | Add recorder IDs to span events only |
| `WithMaxAttributeLength` | Clamp string attribute values; `<= 0` disables clamping |
| `WithSpanEventAttributes` | Add up to 16 application-defined event attributes |
| `WithMetricAttributes` | Add up to 16 application-defined metric attributes |
| `WithAsyncRecorder` | Register async queue/sink health metrics |
| `WithFileBodyStore` | Register managed store health metrics |
| `WithSamplingTransport` | Register sampling/retention health metrics |
