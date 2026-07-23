# Security policy

## Supported versions

Until the first stable release, security fixes are provided on the latest
minor release only. After v1.0.0, the latest major release will receive
security fixes.

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use
[GitHub private vulnerability reporting](https://github.com/mgurevin/recorder/security/advisories/new)
to share the impact, affected versions, reproduction steps, and any proposed
mitigation.

You should receive an acknowledgement within 3 business days and an initial
assessment within 7 business days. Confirmed issues will be coordinated with
the reporter and disclosed through a GitHub security advisory after a fix is
available.

## Audit metadata

The optional `_recorder.redaction` audit extension contains aggregate counts
and body-redactor outcomes only. It intentionally excludes configured rule
names, original values, concrete implementation types, and internal error
text.

## Startup configuration validation

Call `Config.Validate` after applying application settings and before creating
a `Transport`. It rejects static configurations that could silently reduce or
misrepresent evidence, including contradictory TLS/certificate capture flags,
unsupported hash or protection modes, missing encryption/tokenization key
providers, malformed redaction registrations, and unusable content decoders.
Validation performs no I/O and never invokes providers, policies, callbacks,
stores, decoders, or redactors; operational failures remain contained by the
normal runtime error path.

The zero-value `Config{}` and `DefaultConfig()` are both valid. Validation does
not execute request-dependent policies and cannot prove that application
redaction selectors are complete for real payloads. Continue testing
representative requests and responses before enabling body capture.

## Request-scoped redaction

`WithRequestRedaction` and `RequestWithRedaction` use the same
`RedactionConfig` model as `Config.Redaction` and only add name selectors to the
frozen Transport configuration; they cannot remove global defaults such as
credential-header redaction. Attached slices and maps are copied, and the
effective rules are frozen independently for each request/response exchange.
Hints follow the request context across redirects.

Request-scoped `BodyRedactors` are explicit trusted overrides: for a matching
MIME type they replace the globally registered or built-in body redactor.
Review these implementations as security-sensitive code and make them safe for
concurrent use.

Context values are process-local configuration, not a secret store. Put only
selector names and concurrency-safe redactor implementations in request-scoped
rules. Never attach plaintext credentials, encryption keys, or tenant secrets.
Review `CheckRedirect` hooks that replace a request context, because doing so
can intentionally replace the inherited request-scoped hints for later hops.

## Encrypted and tokenized recorded values

Sensitive-value protection changes only the recorded copy; it does not provide
transport encryption and does not alter the live HTTP exchange. Encryption
uses AES-256-GCM with a new random 96-bit nonce per value. Tokenization uses
HMAC-SHA-256 and deliberately exposes equality for the same input and key.

Applications are responsible for KMS-backed key generation, access control,
rotation, retirement, and usage monitoring. Use independent keys for the two
modes and for unrelated environments. Key IDs are stored in tokens and must
not be secrets. Rotate encryption keys conservatively well before extremely
large per-key message counts make random-nonce collision risk operationally
relevant. Tokenization is unsuitable for low-entropy domains when an attacker
can guess candidates.

The key provider receives the original request context and may use bounded,
application-controlled context metadata for request-scoped key selection. Do
not place raw key material in the context. Recorder resolves the provider at
most once per protection mode and exchange, then shares that immutable key
snapshot across the request and response. A remote KMS-backed provider may
still benefit from an application-level cache across exchanges, but it is not
called for every protected field. Recorder also derives AES-GCM once from each
exchange snapshot; reuse never extends to nonces, which remain independently
random for every encrypted value. Archive tooling should use
`ProtectedTokenKeyID` plus `ProtectionKeyResolver`-based helpers to resolve old
keys after rotation. Keep historical keys only for the authorized recovery
period, and make unknown/retired key IDs explicit per-token failures rather
than falling back to another key.

Encryption buffers one selected value up to a configurable limit. The default
is 64 KiB and the hard ceiling is 16 MiB. Limit, key-provider, key-length,
randomness, and cryptographic failures fail closed to `[REDACTED]`; plaintext
is never emitted as fallback. Tokenization is streaming and does not retain a
whole selected value.

Operational protection failures flow through `OnInternalError` and
`InternalErrorLog`, aggregated once per exchange direction with a value count
and the first wrapped cause. This avoids log storms when one unavailable key
affects thousands of fields. Size-limit fallback is an expected policy outcome
and is audited without being logged as an internal failure.

The Inspector accepts keys only into in-memory password fields. It does not
write keys or decrypted values to localStorage, sessionStorage, IndexedDB,
URLs, HAR data, or audit metadata. Resolved plaintext, verified candidates,
and entered keys remain available across entries in the currently loaded HAR
so its detail views can be inspected consistently. They are cleared when a
different HAR is loaded or the operator uses **clear resolved data**. An
in-flight decrypt/verify operation cannot repopulate cleared values. Decrypted
request values enter a reconstructed cURL command only after a separate,
default-off Replay checkbox is enabled; verified token candidates are never
inserted into Replay. Copying plaintext or the resulting command transfers
responsibility to the operator and OS clipboard/shell history.

## Test fixtures and exported captures

The Inspector's default fixture export uses the immutable capture entries, not
the resolved in-memory detail view. Exporting after decryption therefore retains
`REC-ENC-v1` and `REC-TOK-v1` tokens and does not silently write plaintext.

An explicitly selected resolved export creates a derived `.resolved.har` or
`.resolved.ndjson` file. Before download, the Inspector reports counts, modes,
key IDs, and affected structural areas without displaying plaintext, then
requires two separate acknowledgements of disclosure and handling risk. Only
values already resolved in browser memory are substituted; unresolved tokens
remain protected, no key operation starts during export, and source entries are
not mutated. The resulting file may contain credentials, cookies, personal or
financial data, proxy secrets, and complete bodies in plaintext. It is not
equivalent to the original protected evidence and must not be committed or
shared as though it were sanitized.

`hartest` resolves protected values only when the test explicitly supplies a
resolver. Source entries are not mutated and mismatch errors omit query,
header, body, and resolved plaintext values. Keys and plaintext still live in
the test process and may be exposed by the application's own assertions,
logging, crash dumps, or a debugger. Use short-lived test keys and never commit
decrypted fixture files.

The fixture transport cannot reach a real network. Unmatched, incomplete,
truncated, redacted, or unresolved protected request evidence fails instead of
falling through to another `RoundTripper`. External body references require an
explicit `BodyOpener`; opening them grants the test process access to those
assets and inherits the store's filesystem confidentiality requirements. An
export containing `filebody:v1` references is not self-contained: preserve and
authorize the corresponding managed asset directory separately, and do not
release referenced assets while fixtures still depend on them.

## Managed body files

`FileBodyStore` persists the already processed capture representation, but it
may still contain sensitive fields not selected by redaction policy. Its root,
partial, and asset directories must be restricted to the application account;
files are created with mode `0600` and directories with `0700`. Opaque
`filebody:v1` references prevent HAR documents from disclosing local paths and
are validated before open or deletion.

Treat each store root as single-process state. The store has in-process
synchronization but no cross-process filesystem lock; concurrent processes
must use separate roots or external ownership coordination.

Committed assets are evidence owned by the application and are never deleted
by age automatically. Release them only after every HAR, downstream consumer,
or archive that depends on them has completed its ownership transfer. Run
`Reconcile` only with an authoritative live-reference set and use dry-run plus
a non-zero grace period operationally. Quota exhaustion stops new body content
capture rather than deleting referenced assets or changing live HTTP traffic.
The managed store does not encrypt files at rest; use an encrypted filesystem
or volume where host-level confidentiality is required. Sync-on-commit is an
explicit durability/latency option and does not make the entry referencing the
asset crash-durable.

If an `AsyncRecorder` drop policy or bounded-block fallback is used, install an
`AsyncDropHandler` that releases the exact discarded entry's managed assets.
The handler runs outside the queue lock but on the calling goroutine; keep it
bounded when HTTP finalization has a strict latency budget. Handler errors are
routed through `AsyncRecorderConfig`'s internal-error policy and returned by
`Close`. For `FileBodyStore`, configure the cleanup directly:

```go
asyncConfig.DropHandler = func(entry *recorder.Entry, _ recorder.AsyncDropReason) error {
	return store.ReleaseEntryAssets(entry)
}
```

Startup recovery and authoritative reconciliation remain the safety net for
orphaned files.

The optional CLI exposes the same lifecycle rules through `recorder verify`,
`recorder doctor`, and `recorder reconcile`. Reconciliation is a dry run by
default and opens the store in `FileBodyStoreConfig.MaintenanceMode`, which
does not create directories, change permissions, recover partials, or permit
new writers. Deletion requires both `--apply` and `--authoritative`, refuses to
run while partial files exist, and still provides no cross-process lock. Stop
all writers first and supply every capture owning a live reference; an omitted
capture can cause permanent evidence deletion. Keep a non-zero grace period
and review the dry-run result before applying it.

## Live local Inspector stream

`DebugStreamRecorder` is a local-development convenience, not a security
boundary or production evidence sink. Its handler accepts only loopback peers
and, by default, loopback browser origins and supports one subscriber. Its
bounded in-memory queue retains recent entries while disconnected and exposes
oldest-entry loss through SSE gap events. Bind the
application-owned server explicitly to a loopback address. Do not publish it
through a reverse proxy, tunnel, ingress, or container port mapping; streamed
entries can contain every secret present in the corresponding HAR.

`DebugStreamRecorderConfig.AllowedOrigins` can opt a trusted hosted Inspector
into CORS access using exact HTTP(S) origins. It does not permit non-loopback
network peers. This transfers the ability to read the local stream to every
script executing under the configured origin, including compromised hosted
assets and third-party scripts. Never use a wildcard, and prefer the local
Inspector unless the hosted origin and its deployment supply chain are under
your control.

An origin never includes a path. For example, a Pages deployment at
`https://mgurevin.github.io/recorder/` must allow
`https://mgurevin.github.io`, not the full deployment URL. Loopback browser
origins remain implicitly allowed, so adding a hosted origin does not require
listing the local Inspector separately. Continue binding the HTTP server to
`127.0.0.1`; `AllowedOrigins` is not a substitute for that network boundary.

## Sampling and retention

Head sampling receives only method, scheme/host, escaped path, normalized base
MIME type, content length, and explicit correlation keys; it never receives
header, query, cookie, or body values. `HeadSampleDrop` intentionally bypasses
all recorder observation, so a later DNS/TLS/HTTP error is also absent. Use it
only where that evidence loss is acceptable. Policy panics and invalid values
fail open to full recording.

`OnHeadSamplingDecision` may observe a head drop without manufacturing an
entry. It receives only the bounded sampling metadata documented above, runs
before the wrapped transport, and must not be treated as proof of an HTTP
outcome.

Tail retention and required external-asset cleanup execute before
`OnEntryCompleted`. The callback receives the effective disposition. For
`EntryDispositionKeep`, external assets remain borrowed and readable only
during the callback; lasting ownership requires Recorder delivery. For
`EntryDispositionDiscard`, referenced assets have already been released and
must not be opened. A missing release capability or cleanup failure changes the
effective disposition to keep instead of silently orphaning sensitive files.
Retention reduces sink/storage volume but does not undo body capture, hashing,
redaction, or temporary plaintext/protected-value processing already
performed.
