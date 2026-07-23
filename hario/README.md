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

For captures that should not be retained in memory, use the pull streams:

```go
stream, err := hario.NewNDJSONStream(input, config)
if err != nil {
	return err
}

for {
	entry, err := stream.Next()
	if errors.Is(err, io.EOF) {
		break
	}
	if err != nil {
		return err
	}

	process(entry)
}
```

`NewHARStream` exposes the same `Next() (*recorder.Entry, error)` contract for
HAR. Each call owns only the current encoded entry and decoded value; previously
returned entries are not retained by `hario`. The caller may stop early without
draining the source. Call `Next` until `io.EOF` when complete-input validation
matters: HAR fields may legally follow `log.entries`, so trailing metadata,
extensions, limits, and trailing-content checks finish at EOF.

The stream does not close or otherwise own the supplied `io.Reader`. After a
terminal parsing or validation error, repeated `Next` calls return that same
error. `ReadHAR` and `ReadNDJSON` are convenience collectors built on the pull
streams.

`ValidateHAR` and `ValidateEntries` apply the same structural validation to
already-decoded values. Validation establishes that a capture has the shape
required by the helper packages; it does not prove that external body-store
assets exist or that protected values are decryptable.

## Bounds and input ownership

The default limits are deliberately finite:

| Limit | Default | Meaning |
|---|---:|---|
| `MaxBytes` | 256 MiB | Total encoded HAR document or NDJSON stream |
| `MaxEntries` | 100,000 | Maximum validated entries |
| `MaxEntryBytes` | 16 MiB | Encoded bytes for one HAR entry or physical NDJSON line |

Start from `DefaultReadConfig`; `ReadConfig{}` is invalid rather than
unbounded. Set limits from the fixture size you actually expect, especially
when a capture comes from a bug report, CI artifact, or other untrusted source.
Limits apply to encoded capture JSON, not bytes stored behind external
`filebody:v1` references.

Both stream formats enforce `MaxEntryBytes` while reading. An oversized HAR
entry is rejected before the complete JSON value is accumulated in memory;
NDJSON applies the same rule while assembling each physical line.

`hario` never opens external body assets, resolves protected values, or closes
the supplied reader. The caller owns file closure. For a pull stream, reading
until `io.EOF` is the only way to prove that the complete input—including HAR
metadata after `log.entries`—was valid. Stopping early is supported when the
caller intentionally accepts that weaker guarantee.
