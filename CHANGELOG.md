# Changelog

All notable changes to this project will be documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Add a bounded, single-subscriber `DebugStreamRecorder` SSE handler and an
  Inspector live mode for ephemeral local development, with loopback-only
  access, explicit gap reporting, tests, and a complete runnable example.
- Add synchronous `NewMultiRecorder` fan-out with non-short-circuiting joined
  errors, mixed batch-capability preservation, and explicit downstream
  lifecycle ownership.
- Add a protocol E2E compatibility matrix over real loopback HTTP/1.1 and
  HTTP/2 servers, covering SSE streams, bidirectional trailers, concurrent
  requests, WebSocket upgrades, deterministic DNS/connect/TLS failures, and
  malformed HTTP/1.1 framing.
- Run protocol, request-comment, recorder batching, managed FileBodyStore,
  MemoryRecorder, HARFileRecorder, TraceStore, config-surface, and wire-contract
  behavioral tests from the external `recorder_test` package so they prove the
  supported public API without access to implementation state.

### Fixed

- Normalize release SBOM assets as `recorder-X.Y.Z.spdx.json`, matching the
  version form used by GitHub's generated source archives instead of leaking
  the Git tag's `v` prefix into only one asset name.
- Preserve upgraded response streams such as WebSocket connections without
  wrapping them or recording post-upgrade protocol frames as HTTP body bytes.

## [0.4.1] - 2026-07-22

### Added

- Add request-scoped standard HAR exchange comments with redirect inheritance,
  Inspector display, copy support, list indicators, and comment filtering.
- Add V8 coverage reporting and enforced CI thresholds for the Inspector's
  framework-independent `src/lib` logic, with text, JSON summary, and LCOV
  output.
- Add automated SPDX JSON SBOM generation and validation for all Go modules
  and Inspector dependencies, with release assets and GitHub provenance
  attestations generated from published tags.
- Add CI and release vulnerability gates using `govulncheck` for every Go
  module and `npm audit` for the Inspector's runtime and build dependencies.
- Add combined Codecov reporting for all Go modules and the Inspector, with
  separate component flags and README status, reference, and coverage badges.

## [0.4.0] - 2026-07-22

### Added

- Add Inspector support for local and remote `JSONStreamRecorder` NDJSON,
  including blank-line tolerance, strict all-or-nothing entry validation,
  physical line-number errors, and an explicit source-format indicator.
- Add a bounded FIFO `AsyncRecorder` decorator with evidence-preserving
  `AsyncBlock` backpressure by default, explicit drop-newest/drop-oldest
  policies, context-aware background drain, optional downstream close
  ownership, contained sink failures, and concurrency-safe queue/block/drop
  statistics.
- Add opt-in size/interval-based `AsyncRecorder` batching through structurally
  discovered `RecordBatch([]*Entry) error`, with FIFO shutdown flush, reusable
  batch storage, built-in recorder support, and batch health metrics.
- Add opt-in bounded `AsyncBlock` waiting with explicit drop fallback, active
  oldest-block age, fixed timeout drop reasons, and a panic-contained drop hook
  for releasing external assets owned by discarded entries.
- Add bounded OpenTelemetry health metrics for `AsyncRecorder` queue depth,
  capacity, throughput, producer blocking, drops, and downstream failures via
  `otelrecorder.Config.AsyncRecorder`.
- Bound `MemoryRecorder` to the newest 1,024 entries by default, with an
  explicit custom-capacity constructor, O(1) ring-buffer eviction, atomic
  snapshots, and retention/eviction statistics.
- Add managed `FileBodyStore` lifecycle APIs with transactional partial-file
  commit/abort, opaque references, byte/file quotas, startup partial recovery,
  explicit release/reconciliation, lifecycle statistics, and OpenTelemetry
  metrics.
- Add separate head-sampling and finalized-entry retention policies, stable
  request/trace sampling keys, an allocation-free uninstrumented drop path,
  metadata-only capture ceilings, automatic discarded-asset cleanup, bounded
  statistics, and OpenTelemetry sampling health metrics.
- Add protected-token key-ID inspection and resolver-based decrypt/verify
  helpers so trusted archive tooling can process mixed key generations.

### Changed

- Change `Recorder.Record` and the optional batch capability to return errors,
  so synchronous sink failures are routed through Transport's internal-error
  policy. Remove `JSONStreamRecorder.Err`; `AsyncRecorder.Close` returns worker
  and downstream failures after draining.
- Change `AsyncDropHandler` to return errors and remove
  `FileBodyStoreDropHandler`; callers can pass `FileBodyStore.ReleaseEntryAssets`
  through a small closure, while failures now use AsyncRecorder's internal-error
  policy, stats, OpenTelemetry metric, and `Close` result.
- Replace the single-method `ProtectionKeyProvider`/`ProtectionKeyResolver`
  interfaces and their `Func` adapters with direct function types.
- Freeze the pre-v1 public surface around explicit `Config`,
  `AsyncRecorderConfig`, `FileBodyStoreConfig`, and `otelrecorder.Config`
  values; remove functional option APIs and policy adapter/interface pairs.
- Make `Transport` internals private and require the recorder and complete
  configuration at construction time.
- Consolidate every recorder-specific HAR entry field under the versioned
  `_recorder` v1 extension and publish its JSON Schema. The Inspector consumes
  only this schema and rejects unknown versions.
- Keep batch delivery and entry-asset release as structurally discovered
  internal capabilities instead of exported maintenance contracts.
- Replace the pre-1.0 `BodyWriter.Close` contract with explicit `Commit` and
  `Abort` outcomes so custom stores cannot confuse retry cleanup with asset
  publication.
- Invoke `OnEntryCompleted` before retention and Recorder delivery, with
  borrowed asset ownership limited to the callback duration, so discard paths
  cannot invalidate body references before completion observers run.
- Centralize the OpenTelemetry adapter's setup, lifecycle, complete
  metric/unit/attribute reference, data-safety guidance, and production
  alerting scenarios in `otelrecorder/README.md`.
- Pass the original request context to `ProtectionKeyProvider`, enabling
  request-scoped key selection without mutable Transport configuration.
- Use one fail-contained internal-error policy for Transport and
  `AsyncRecorder`, and make both recommended default configs log evidence
  degradation while their explicit zero values remain silent.

### Fixed

- Avalanche deterministic sampling hashes before threshold comparison so low
  rates remain statistically representative for common prefixed, sequential,
  and fixed-width hexadecimal keys. Existing keys may receive a different
  deterministic decision after upgrading to this fix.

## [0.3.0] - 2026-07-21

### Added

- Add pinned Go and ESLint pipelines with automatic Go formatting, whitespace
  enforcement, TypeScript correctness checks, and React Hooks validation.
- Use the stable TypeScript 7 native compiler while providing ESLint with
  Microsoft's supported TypeScript 6 compatibility API.
- Add a complete streaming CSV redactor example and a library-owned, bounded
  streaming `BodyValue` contract so custom formats and built-ins use the same
  centralized redact, encrypt, tokenize, size-limit, failure, and audit path.
- Add an independently pinned, end-to-end tested example for streaming Brotli
  and Zstandard record-time content decoding.
- Add immutable, additive request-scoped redaction rules carried by context,
  with independent request/response selectors, redirect inheritance, shared
  client concurrency isolation, and per-request custom body redactors.
- Extend `otelrecorder` with bounded metrics for HTTP phase latency, captured
  body size and outcome, protection-mode value counts, fail-closed fallbacks,
  and body-redactor outcomes.

### Changed

- Change the pre-1.0 `BodyRedactor.Redact` signature to receive a body-scoped
  `BodyValueProtector`; custom implementations must stream each selected value
  through `NewValue`, `Write`, and `Finish` instead of buffering values or
  constructing replacements and protected tokens themselves. Remove the
  redundant `BodyRedactionReporter`; the central value lifecycle now owns all
  replacement and protection counts.
- Replace the separate legacy redaction option APIs with one
  `RedactionConfig` model shared by `Config.Redaction`, `WithRequestRedaction`,
  and `RequestWithRedaction`. `Common` rules apply to both directions, while
  `Request` and `Response` add direction-specific rules.
- Rename the pre-1.0 `BodyCaptureDecision.BodyRedactor` field to
  `RedactorOverride`, clarifying that nil preserves `RedactionConfig` and a
  non-nil value is the final override for one runtime-selected body.

## [0.2.1] - 2026-07-20

### Added

- Add precise hover details to Inspector timing waterfalls, including phase
  duration, relative bounds, absolute start/end timestamps when available,
  and a live vertical cursor showing the current offset in seconds.
- Retain successfully verified token candidates in Inspector session memory,
  list their matching tokens and occurrences, and show them in resolved detail
  views without enabling them for Replay.
- Add an Inspector control that clears all in-memory plaintext, verified
  candidates, and protection keys and restores the original HAR view.
- Add structured-redactor, protection-mode, complete body-pipeline, storage,
  compression, chunk-size, and parallel benchmark coverage with documented
  measurements and performance-critical guidance.

### Fixed

- Report the module FQDN in HAR `creator.name`, keep `creator.version` aligned
  with each release, and document request-side record-time content decoding
  consistently.
- Eliminate per-byte output allocations in JSON, XML and URL-encoded form
  streaming redactors with a cached `io.ByteWriter` fast path and reusable
  fallback buffer.

## [0.2.0] - 2026-07-20

### Added

- Add a pluggable streaming `BodyRedactor` API with exact base-MIME
  registration and explicit custom-over-built-in precedence.
- Add per-request/per-response `BodyCapturePolicy` decisions for capture,
  embedding, hashing, limits, and body-redactor overrides.
- Add a non-sensitive `_recorder.redaction` audit extension and inspector summary for
  changed recorded values and body-redactor outcomes.
- Add redact, AES-256-GCM encrypt, and HMAC-SHA-256 tokenize modes for values
  selected by built-in protection rules, with versioned tokens and key IDs.
- Add an Inspector Protection tab for in-memory decryption and candidate
  verification, plus explicit opt-in use of decrypted request values in Replay.

### Changed

- Group Inspector protection operations by mode and key ID, with one-key
  exchange-wide or HAR-wide batch decryption and verification, and show the
  decrypted in-memory view consistently across detail tabs and replay proxy
  credentials.
- Refine the Inspector Replay tab with structured option, command, and review
  panels plus clearer availability and warning states.
- Route built-in and custom body redactors through one single-pass lifecycle;
  HAR embedding reuses the already-redacted BodyStore representation.

### Security

- Fail closed to metadata-only body recording when a capture policy errors or
  panics, without altering the live HTTP exchange.
- Redact configured `application/x-www-form-urlencoded` values before they
  reach memory/file body stores or embedded HAR content.
- Stream `multipart/form-data` redaction before body stores, including file
  payloads and filename metadata, with bounded headers and fail-closed parsing.
- Bound each encrypted plaintext value (64 KiB default, 16 MiB ceiling), stream
  tokenization through HMAC, and fall back only to `[REDACTED]` on protection
  failures or limits.
- Keep Inspector keys and plaintext session-only within the loaded HAR;
  Replay never consumes decrypted values by default.
- Report key-provider and cryptographic protection failures through the
  internal-error policy once per exchange direction, with aggregate counts,
  while keeping expected size-limit fallback audit-only.

## [0.1.1] - 2026-07-20

### Security

- Stream JSON/XML redaction before captured bytes reach the BodyStore, with
  bounded parser buffers and fail-closed depth/token limits.
- Redact every NDJSON document, accept an initial JSON UTF-8 BOM, and keep XML
  subtree suppression active across mismatched end tags.

## [0.1.0] - 2026-07-18

### Added

- HAR 1.2 recording for the complete `net/http` client exchange lifecycle.
- Structured network, TLS, timing, error, body, trace, and redirect metadata.
- Memory, atomic HAR file, NDJSON, and pluggable body storage backends.
- Header, cookie, query, JSON, XML, error-message, and redirect redaction.
- Optional OpenTelemetry exporter.
- Browser-based HAR inspector with trace-chain grouping, aggregate timing
  waterfalls, safe image/video previews, JSON/XML highlighting, cURL replay,
  and local or remote HAR loading through deep links.

### Changed

- Production defaults avoid capturing or embedding body content and raw trace
  data unless explicitly enabled.
- JSON and XML redaction preserve all unaffected source bytes.
- The core module requires Go 1.24; the OpenTelemetry module requires Go 1.25.

### Security

- Body content capture, embedding, and hashing are opt-in by default.
- Redaction covers redirect URLs, URL-bearing headers, proxy URLs, structured
  bodies, errors, and raw trace details.
- Recorder callbacks and storage failures are isolated from HTTP behavior.
- Recorder-internal failures never replace the original HTTP transport error.

[Unreleased]: https://github.com/mgurevin/recorder/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/mgurevin/recorder/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/mgurevin/recorder/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/mgurevin/recorder/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/mgurevin/recorder/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/mgurevin/recorder/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/mgurevin/recorder/releases/tag/v0.1.0
