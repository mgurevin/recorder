# Streaming CSV body redactor

This copy-oriented example implements `recorder.BodyRedactor` for CSV documents
whose first record is a header. It is intentionally a `package main`, not a
supported importable package. Copy the implementation into an application-owned
package and adapt it there. It selects sensitive columns by case-insensitive
header name and processes arbitrarily chunked input through a bounded `io.Pipe`.
Recorder supplies the value protector, so the same redactor automatically uses
redact, encrypt, or tokenize mode and reports outcomes to the redaction audit.

Register one immutable `Redactor` for the CSV media types used by the service:

```go
csv := Redactor{
	Columns: []string{"email", "card_number", "account_number"},
}

config := recorder.DefaultConfig()
config.CaptureRequestBody = true
config.CaptureResponseBody = true
config.Redaction = recorder.RedactionConfig{Common: recorder.RedactionRules{
		BodyRedactors: map[string]recorder.BodyRedactor{
			"text/csv":        csv,
			"application/csv": csv,
		},
	}}
if err := config.Validate(); err != nil {
	return err
}
transport := recorder.NewTransport(http.DefaultTransport, recorder.NewMemoryRecorder(), config)

client := &http.Client{Transport: transport}
```

Important behavior:

- The first record must contain every configured column. Missing columns fail
  closed before any data row is emitted.
- Malformed CSV, short records, destination failures, and parser errors are
  returned from `Write` or `Close`. Recorder contains custom-redactor failures
  and reports them through its internal-error and redaction-audit mechanisms;
  the original HTTP body seen by the caller is unchanged.
- `encoding/csv` parses and re-serializes records, so delimiters and values are
  preserved semantically, but original quoting and line endings are not
  byte-for-byte preserved.
- The redactor never receives raw keys and never constructs protection tokens.
  For each selected field it streams bytes into a fresh `BodyValue` and uses
  the result of `Finish`; Recorder applies the configured mode, size limit,
  fail-closed fallback, token format, replacement count, and audit.
- Always bound captured body size with Recorder's request/response body limits.
