# Changelog

All notable changes to this project will be documented in this file. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- HAR 1.2 recording for the complete `net/http` client exchange lifecycle.
- Structured network, TLS, timing, error, body, trace, and redirect metadata.
- Memory, atomic HAR file, NDJSON, and pluggable body storage backends.
- Header, cookie, query, JSON, XML, error-message, and redirect redaction.
- Optional OpenTelemetry exporter and browser-based HAR inspector.

### Security

- Body content capture, embedding, and hashing are opt-in by default.
- Recorder callbacks and storage failures are isolated from HTTP behavior.

[Unreleased]: https://github.com/mgurevin/recorder/compare/v0.1.0...HEAD
