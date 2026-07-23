# Examples

- [Streaming CSV body redactor](./csv-redactor/) — a complete custom
  `BodyRedactor` using Recorder's redact, encrypt, and tokenize pipeline.
- [Brotli and Zstandard content decoders](./content-decoders/) — independently
  pinned, streaming `ContentDecoder` registrations with end-to-end tests.
- [Live local Inspector stream](./debug-stream/) — a bounded,
  single-subscriber SSE recorder for local development only.
- [HAR/NDJSON fixture replay](./har-fixture/) — composition of the optional
  bounded `hario` reader and network-free `hartest` transport.

Every example is compiled and tested by `make test` and the CI Go matrix.
Behavioral tests exercise each example's central integration rather than only
checking that its command builds. `make coverage-report` combines coverage from
the root module and independently versioned content-decoder module into one
**Documented code examples** component and one source-level detail report.
