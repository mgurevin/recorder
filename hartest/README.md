# hartest

`hartest` turns recorder HAR or NDJSON evidence into deterministic,
network-free `http.RoundTripper` fixtures. It is an optional testing helper,
not part of the recording path and not a general-purpose traffic replay
engine.

Unlike record-on-miss VCR tools, an unmatched request is never sent to a real
server. Recording, review/redaction, fixture export, and replay remain separate
steps. That makes fixture creation slightly more explicit and makes CI replay
safer and reproducible.

## Quick start with HAR

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

`Verify` belongs in test cleanup when every captured exchange must be used:

```go
t.Cleanup(func() {
	if err := fixture.Verify(); err != nil {
		t.Error(err)
	}
})
```

## Quick start with NDJSON

The transport accepts the same `[]*recorder.Entry` produced by either reader:

```go
readConfig := hario.DefaultReadConfig()
entries, err := hario.ReadNDJSON(file, readConfig)
if err != nil {
	return err
}

fixture, err := hartest.NewTransport(entries, hartest.DefaultConfig())
if err != nil {
	return err
}

client := &http.Client{Transport: fixture}
```

## Matching contract

Fixtures are consumed once, in capture order. Matching is strict over:

- method, scheme, host, escaped path, and complete query multimap;
- configured request headers (`Content-Type` by default); and
- the exact request body whenever either side contains one; and
- captured request trailers.

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

Repeated identical requests consume repeated captures in their original order.
This supports pagination retries, polling, and stateful APIs without a
"replay forever" switch that could hide an unexpected extra request.

## Dynamic request values

Real requests often contain a generated ID, timestamp, nonce, signature, or
semantically equivalent JSON with different whitespace. Set one
`RequestNormalizer` when those values must not select a fixture. The same
normalizer receives isolated copies of the live request and recorded request;
mutating them cannot modify the application request or source evidence.

For an order API whose path ID, timestamp, and JSON `requestId` vary:

```go
fixtureConfig := hartest.DefaultConfig()
fixtureConfig.Match.Normalize = func(request *hartest.RequestSnapshot) error {
	request.URL.Path = "/orders/{id}"

	query := request.URL.Query()
	query.Del("timestamp")
	request.URL.RawQuery = query.Encode()

	var body map[string]any
	if err := json.Unmarshal(request.Body, &body); err != nil {
		return err
	}

	delete(body, "requestId")
	normalized, err := json.Marshal(body)
	if err != nil {
		return err
	}

	request.Body = normalized

	return nil
}
```

This single hook covers the common reasons to write a fully custom matcher:
canonicalize a path segment, remove selected query parameters, normalize JSON,
or adjust headers/trailers. Matching remains deterministic after normalization.
Do not erase fields that are material to application behavior, such as an order
amount, target account, idempotency key, or authorization scope.

The normalizer may inspect and replace `Method`, `URL`, `Headers`, `Trailers`,
and `Body`. It must leave `URL` non-nil. Errors are reported without including
request values. Keep normalizers deterministic and side-effect free.

### Header scenarios

Only the names in `MatchConfig.Headers` select a fixture. The default is
`Content-Type`. Add business-significant headers explicitly:

```go
fixtureConfig.Match.Headers = []string{
	"Content-Type",
	"Idempotency-Key",
	"X-API-Version",
}
```

Do not match volatile tracing headers merely because they were captured.
Conversely, do not omit a header that changes the server's behavior.

### Body scenarios

`BodyMatchAuto` is the safe default: it compares complete bytes when either
side has a body. A POST/PUT/PATCH request therefore cannot accidentally match a
fixture for different input. Use a normalizer for JSON canonicalization or
field-level volatility. Reserve `BodyMatchIgnore` for tests where the body is
intentionally irrelevant and make that weakening obvious next to the test.

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

## Practical fixture workflow

1. Capture representative traffic with recorder and production-appropriate
   capture/redaction policy.
2. Inspect the evidence and export only the exchanges needed by the test as HAR
   or NDJSON.
3. Store the bounded fixture under `testdata`; keep external body assets beside
   it when used.
4. Load it with `hario`, create a `hartest.Transport`, and run the application
   through a normal `http.Client`.
5. Assert application behavior and call `Verify` so unused or missing
   interactions fail the test.

Small focused fixtures are easier to review than large captured sessions. Keep
one scenario per fixture where practical—for example `payment-approved`,
`payment-declined`, `upstream-timeout`, and `response-body-read-failure`.

## Scope compared with VCR-style libraries

`hartest` provides strict method/URL/query/body/header/trailer matching,
customizable deterministic normalization, ordered repeated interactions,
captured failures, external bodies, protected-value resolution, HAR and NDJSON
input, and unused-fixture verification.

It deliberately does not combine live network access and cassette mutation
inside the replay transport. Record-on-miss, passthrough, and rewriting evidence
while a test runs would weaken the guarantee that CI used only reviewed
fixtures. If a test intentionally needs live traffic, use a separate client and
make that boundary explicit.
