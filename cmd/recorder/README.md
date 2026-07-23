# recorder CLI

`cmd/recorder` is an optional, standard-library-only command for local capture
workflows. It does not participate in HTTP capture and does not change the core
library API.

Install it with:

```sh
go install github.com/mgurevin/recorder/cmd/recorder@latest
```

The CLI uses the same bounded, structurally validating `hario` readers as
fixture replay. Inputs default to 256 MiB total, 100,000 entries, and 16 MiB per
encoded entry.

## Validate

Validate the complete container and every entry:

```sh
recorder validate capture.har
recorder validate entries.ndjson
recorder validate --json capture.har
```

Use `-format har` or `-format ndjson` for stdin or files without a recognized
extension:

```sh
cat capture.har | recorder validate -format har -
```

## Summarize

Print metadata-only totals without rendering or resolving body content:

```sh
recorder summarize capture.har
recorder summarize --json entries.ndjson
```

The summary includes methods, status classes, hosts, traces, failures, duration,
captured body bytes, and incomplete-body counts. It never resolves protected
values or opens external body assets.

## Convert

Convert only after the complete input has passed validation:

```sh
recorder convert -to ndjson -output entries.ndjson capture.har
recorder convert -to har -output capture.har entries.ndjson
```

Conversion preserves entries as represented in the source. It does not resolve
protected values or embed external body assets. An NDJSON-to-HAR conversion
creates a new HAR 1.2 envelope with the current recorder creator metadata.
Input is pulled entry by entry and output is published only after the complete
source validates. Existing output files are never overwritten.

## Inspect

Validate a local file, serve that exact file from an ephemeral loopback port,
and open the browser-only Inspector:

```sh
recorder inspect capture.har
recorder inspect entries.ndjson
```

The server accepts only `GET` and `HEAD`, rejects browser origins other than the
configured Inspector origin, sends `Cache-Control: no-store`, and stops on
Ctrl-C. It never listens on a non-loopback address. Use `-no-open` to print the
URL without launching a browser, or `-inspector-url http://localhost:5173/` for
a locally built Inspector.

The hosted HTTPS Inspector may be unable to load a loopback HTTP capture in
Safari/WebKit because of mixed-content policy. Use the local HTTP Inspector in
that browser.

## Verify evidence and body assets

Verify structural evidence, committed `FileBodyStore` assets, sizes, and
comparable body checksums:

```sh
recorder verify capture.har --body-store ./spool
recorder verify --json capture.har --body-store ./spool
```

The command is read-only. It reports missing, modified, and unreferenced
committed assets separately and never removes files. Missing or modified
evidence returns a non-zero exit status; unreferenced assets are informational
because a store may be shared by multiple capture files.

A recorded body hash covers the bytes observed by the HTTP client, while an
external or embedded representation may have been decoded, redacted, or
truncated. `verify` compares a checksum only when the stored representation is
provably byte-equivalent; otherwise it reports the checksum as skipped rather
than producing a false mismatch. MD5 and SHA-1 are accepted only to verify
legacy recorded evidence, never for new security decisions.

## Build deterministic fixtures

Select entries without loading the complete capture into memory:

```sh
recorder fixture capture.har \
  --method POST \
  --host api.example.com \
  --status-min 200 \
  --status-max 299 \
  --output testdata/orders.ndjson
```

The output format is inferred from `.har`, `.ndjson`, or `.jsonl`; stdout
defaults to NDJSON. Use `-to` to make it explicit. Selection preserves the
captured representation and protected values. Output is written to a temporary
file and atomically published only after the complete source validates.
Existing files are not overwritten.

## Serve a network-free fixture

Expose one captured origin as a local HTTP server backed by `hartest`:

```sh
recorder serve-fixture testdata/orders.har --listen 127.0.0.1:8080
```

Incoming path, query, headers, and body are mapped onto the fixture's recorded
origin and matched with the same strict, consume-once rules as
`hartest.Transport`. The command never forwards unmatched requests to a real
network. A capture containing multiple origins must first be selected with
`fixture`, or receive an explicit `-origin https://api.example.com`.

Only loopback listen addresses are accepted. On shutdown, unused exchanges
produce a non-zero exit status unless `-allow-unused` is explicit. External
response bodies require `-body-store`.

## Diagnose local compatibility

Check the CLI runtime and supported recorder extension without opening a
capture:

```sh
recorder doctor
```

Add a capture and optional body store to diagnose structural/schema
compatibility and external evidence availability:

```sh
recorder doctor capture.har --body-store ./spool
recorder doctor --json entries.ndjson
```

`doctor` is read-only. A capture with external body references but no
`-body-store` produces a warning rather than pretending the assets were
verified. Invalid captures, unsupported schemas, unreadable stores, and
missing or modified evidence fail the command. Reports use the capture's base
name and do not expose local filesystem paths.

## CLI compatibility

Although `cmd/recorder` is not an importable Go package, its command names,
flags, JSON fields, output safety rules, and exit-status meanings are treated
as a public interface. CI gives it an independent coverage threshold and
publishes its source-level report alongside the library and Inspector reports.

Exit status `0` means the command completed and its evidence checks passed.
Status `1` means input, validation, verification, fixture matching, or runtime
work failed. Status `2` means command-line usage was invalid. Informational
findings such as body assets unreferenced by the inspected capture do not fail
the command.
