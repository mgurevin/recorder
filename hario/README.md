# hario

`hario` is the optional, dependency-free capture reader for tooling and tests.
It is separate from the recorder's write path: applications that only produce
evidence do not need to import it.

```go
config := hario.DefaultReadConfig()
config.MaxBytes = 32 << 20
config.MaxEntries = 5_000

document, err := hario.ReadHAR(input, config)
if err != nil {
	return err
}
```

Use `ReadNDJSON` for `JSONStreamRecorder` output. NDJSON is decoded one physical
line at a time and is never wrapped into a synthetic HAR document. Both readers
are all-or-nothing, reject malformed entries and unsupported recorder extension
versions, and enforce byte, entry, and per-entry limits before returning data.

`ValidateHAR` and `ValidateEntries` apply the same structural validation to
already-decoded values. Validation establishes that a capture has the shape
required by the helper packages; it does not prove that external body-store
assets exist or that protected values are decryptable.
