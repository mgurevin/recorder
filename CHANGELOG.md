# Changelog

All notable changes to this project will be documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Add precise hover details to Inspector timing waterfalls, including phase
  duration, relative bounds, absolute start/end timestamps when available,
  and a live vertical cursor showing the current offset in seconds.
- Retain successfully verified token candidates in Inspector session memory,
  list their matching tokens and occurrences, and show them in resolved detail
  views without enabling them for Replay.
- Add an Inspector control that clears all in-memory plaintext, verified
  candidates, and protection keys and restores the original HAR view.

## [0.2.0] - 2026-07-20

### Added

- Add a pluggable streaming `BodyRedactor` API with exact base-MIME
  registration and explicit custom-over-built-in precedence.
- Add per-request/per-response `BodyCapturePolicy` decisions for capture,
  embedding, hashing, limits, and body-redactor overrides.
- Add a non-sensitive `_redaction` audit extension and inspector summary for
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
- Keep Inspector keys and plaintext session-only and isolate them across HAR
  entries; Replay never consumes decrypted values by default.
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

[Unreleased]: https://github.com/mgurevin/recorder/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/mgurevin/recorder/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/mgurevin/recorder/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/mgurevin/recorder/releases/tag/v0.1.0
