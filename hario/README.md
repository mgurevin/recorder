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
