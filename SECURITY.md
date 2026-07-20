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

The Inspector accepts keys only into in-memory password fields. It does not
write keys or decrypted values to localStorage, sessionStorage, IndexedDB,
URLs, HAR data, or audit metadata, and clears the session when the selected HAR
entry changes. Decrypted request values enter a reconstructed cURL command only
after a separate, default-off Replay checkbox is enabled. Copying plaintext or
the resulting command transfers responsibility to the operator and OS
clipboard/shell history.
