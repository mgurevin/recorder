# hartest

`hartest` turns recorded exchanges into deterministic, network-free
`http.RoundTripper` fixtures. It is an optional testing helper, not part of the
recording path and not a general-purpose traffic replay engine.

```go
readConfig := hario.DefaultReadConfig()
document, err := hario.ReadHAR(file, readConfig)
if err != nil {
	return err
}

fixtureConfig := hartest.DefaultConfig()
fixture, err := hartest.NewTransport(document.Log.Entries, fixtureConfig)
if err != nil {
	return err
}

client := &http.Client{Transport: fixture}
resp, err := client.Post(
	"https://api.example.test/orders",
	"application/json",
	strings.NewReader(`{"id":42}`),
)
// Assert the application result, then:
if err := fixture.Verify(); err != nil {
	return err
}
```

## Matching contract

Fixtures are consumed once, in capture order. Matching is strict over:

- method, scheme, host, escaped path, and complete query multimap;
- configured request headers (`Content-Type` by default); and
- the exact request body whenever either side contains one.

Header order and duplicate header values remain significant. Body matching can
be disabled only by explicitly setting `BodyMatchIgnore`. This is useful for a
deliberately variable field, but weakens the fixture and should be visible in
the test that opts into it.

The compared body is the representation stored in the capture. If recording
performed request-side content decoding or redaction, construct the test request
from that same representation. Incomplete, truncated, missing, redacted, or
unresolved protected request bodies fail closed instead of matching broadly.
Mismatch diagnostics identify the structural category without printing query,
header, body, or plaintext values.

The transport has no fallback `RoundTripper` and can never send an unmatched
request to a network. `Verify` fails when any fixture remains unused.

## Bodies, failures, and trailers

Embedded text and base64 bodies are supported. Supply `Config.Bodies` to resolve
opaque external body references; `FileBodyStore` already implements the
required interface. Reads are bounded by `MaxResponseBodyBytes`.

Captured transport failures return `RecordedError`. Partial response bodies with
a recorded read error return their captured bytes and then a synthetic read
error. Response trailers become visible at EOF, matching `net/http` semantics.
The helper reproduces recorded observations; it does not claim to recreate the
original concrete network error type or timing.

## Protected values

Protected-value resolution is opt-in. `DecryptProtectedValues` adapts a
rotation-aware `recorder.ProtectionKeyResolver` for encrypted tokens:

```go
fixtureConfig.ProtectedValues = hartest.DecryptProtectedValues(func(keyID string) (recorder.ProtectionKey, error) {
	return keyStore.Lookup(keyID)
})
```

Tokenized values require an application-specific `ProtectedValueResolver`.
Resolution never mutates the source entries. Plaintext is used only to build the
in-memory fixture and is not included in mismatch diagnostics. Keep keys and
fixtures within the test process, and do not commit decrypted exports.

The Inspector deliberately exports the original protected representation even
when values have been resolved in its current browser session.
