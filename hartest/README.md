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
if err != nil {
	return err
}
defer resp.Body.Close()

// Read/assert the response as the application normally would.
body, err := io.ReadAll(resp.Body)
if err != nil {
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

Register cleanup immediately after construction, but ensure every goroutine
using the client has stopped before the test returns. `Verify` is safe to call
concurrently, but it takes the same transport lock as matching and semantically
belongs after all expected requests. Closing fixture response bodies remains
the caller's normal `net/http` responsibility.

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

## Large fixtures

Avoid collecting a large capture when the test naturally follows capture
order. `hario.EntryStream` implements `hartest.EntrySource`:

```go
source, err := hario.NewNDJSONStream(file, hario.DefaultReadConfig())
if err != nil {
	return err
}

fixture, err := hartest.NewStreamTransport(source, hartest.DefaultConfig())
if err != nil {
	return err
}

client := &http.Client{Transport: fixture}
```

`NewHARStream` works the same way. The transport pulls entries only until it
finds a match and releases each consumed entry. In the common case where test
requests follow capture order, memory is bounded near the current entry plus
the response body.

Out-of-order matching remains supported: earlier unmatched entries are retained
so a later request can consume them. A test that searches near the end of a
large capture before using earlier entries may therefore retain that unmatched
prefix. Split unrelated scenarios into smaller fixtures when strict memory
bounds matter.

`Verify` drains the source to validate trailing input and count every unused
entry. This is especially important for streaming HAR because document metadata
may follow `log.entries`. Treat `Verify` as part of the test contract rather
than an optional assertion.

The defaults read at most 4 MiB from a live request and 32 MiB from a response
or external body asset. Tune `Match.MaxRequestBodyBytes`,
`MaxResponseBodyBytes`, and the `hario.ReadConfig` limits together. Embedded
fixture request size is bounded by the capture reader's `MaxEntryBytes`;
programmatically constructed entries should be bounded by their creator.

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

`RecordedError` deliberately preserves only the recorded phase, sanitized
message, and timeout classification. It does not manufacture concrete
`net.DNSError`, `net.OpError`, or TLS error values: HAR evidence does not retain
every field and unwrap relationship required to reconstruct those values
faithfully. Returning a partially invented standard-library error could make
`errors.As` succeed while exposing semantics that were never observed. Assert
on `RecordedError.Phase` and `Timeout()` instead.

Timing is also not replayed. Tests run immediately and deterministically rather
than sleeping for captured DNS, connection, TLS, server-wait, or body-transfer
durations.

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

There are two deliberate fixture workflows:

- Export **protected evidence** (the Inspector default) and configure
  `ProtectedValues` in the test. This keeps committed fixtures protected and is
  preferred when CI can receive short-lived keys securely.
- Export a derived **resolved plaintext** fixture only for a controlled
  environment that cannot resolve tokens at test time. The Inspector marks the
  filename `.resolved`, reports affected locations without showing plaintext,
  and requires two acknowledgements. Such a file is no longer protected
  evidence and must not be committed or shared as sanitized data.

Unresolved tokens in request URL, matched headers, trailers, or bodies fail
closed. Unresolved tokens may remain in response fields because they do not
select a fixture; the application will then observe the protected
representation. Provide a resolver when the response plaintext is material to
the test.

### External body assets

An entry containing `_recorder.requestBody.store` or
`_recorder.responseBody.store` is not a self-contained fixture. The Inspector
exports the opaque reference, not the referenced bytes. Preserve the matching
body-store `assets/` directory and pass the store as `Config.Bodies`:

```go
store, err := recorder.NewFileBodyStore(assetRoot, recorder.DefaultFileBodyStoreConfig())
if err != nil {
	return err
}

fixtureConfig.Bodies = store
```

`filebody:v1` references intentionally contain no pathname, so copying only the
HAR or NDJSON file makes those bodies unavailable. Treat the asset directory
with the same confidentiality and retention policy as the capture. Do not call
`Release` until every test fixture that references the asset has expired.

## Practical fixture workflow

1. Capture representative traffic with recorder and production-appropriate
   capture/redaction policy.
2. Inspect the evidence and export only the exchanges needed by the test as HAR
   or NDJSON. Prefer the protected representation; use resolved export only
   under the handling constraints above.
3. Store the bounded fixture under `testdata`. If it has external body
   references, retain the matching asset directory and configure `Bodies`.
4. Load it with `hario`, create a `hartest.Transport`, and run the application
   through a normal `http.Client`.
5. Consume and close response bodies as production code does, wait for any
   concurrent calls, then let `Verify` report unused interactions or trailing
   stream errors.

Small focused fixtures are easier to review than large captured sessions. Keep
one scenario per fixture where practical—for example `payment-approved`,
`payment-declined`, `upstream-timeout`, and `response-body-read-failure`.
Avoid sharing one mutable fixture transport between parallel test cases:
consumption is intentionally stateful, so construct one transport per test.

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
