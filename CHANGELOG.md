# Changelog

All notable changes to this project will be documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/mgurevin/recorder/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/mgurevin/recorder/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/mgurevin/recorder/releases/tag/v0.1.0
