# Brotli and Zstandard content decoders

Recorder keeps its core Go module dependency-free. This independently pinned,
copy-oriented example adds record-time decoding for `Content-Encoding: br` and
`Content-Encoding: zstd` with streaming pure-Go implementations. It is
intentionally a `package main`, not a supported importable package; copy the
decoder functions and required dependencies into an application-owned package.

Register both decoders when constructing the transport:

```go
config := recorder.DefaultConfig()
config.CaptureResponseBody = true
config.MaxResponseBodyBytes = 8 << 20
for name, decoder := range Decoders() {
	config.ContentDecoders[name] = decoder
}
if err := config.Validate(); err != nil {
	return err
}
transport := recorder.NewTransport(http.DefaultTransport, recorder.NewMemoryRecorder(), config)
client := &http.Client{Transport: transport}
```

Or copy only the decoder you need from
[`decoders.go`](./decoders.go). The dependencies and their tested versions are
pinned in this directory's `go.mod`.

Important behavior:

- Decoding applies only to Recorder's captured copy. The live request and the
  response bytes returned to the caller remain encoded and byte-for-byte
  unchanged.
- Decoded content passes through the same streaming body-redaction and
  protection pipeline as uncompressed content before it reaches a BodyStore or
  embedded HAR content.
- Hashes, byte counters, and response `bodySize` continue to describe the
  encoded bytes. HAR content describes the decoded representation and carries
  `_recorder.responseBodyDecoded: true`.
- Register the decoder name that appears in `Content-Encoding`: `br` for
  Brotli and `zstd` for Zstandard. Chained encodings are intentionally not
  accepted.
- Always set directional capture limits. Decoded output beyond the applicable
  limit is refused, which bounds compression-expansion attacks.
- With body redaction active, malformed encoded input or decoder failures stop
  body storage and are reported through Recorder's internal-error policy;
  encoded bytes are never persisted as a plaintext fallback.

Run the executable round-trip tests from this directory:

```sh
go test ./...
```
