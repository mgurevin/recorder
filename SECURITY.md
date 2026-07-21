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

The optional `_redaction` audit extension contains aggregate counts and body
redactor outcomes only. It intentionally excludes configured rule names,
original values, concrete implementation types, and internal error text.

## Request-scoped redaction

`WithRequestRedaction` and `RequestWithRedaction` use the same
`RedactionConfig` model as `WithRedaction` and only add name selectors to the
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
bounded when HTTP finalization has a strict latency budget. Startup recovery
and authoritative reconciliation remain the safety net for orphaned files.
