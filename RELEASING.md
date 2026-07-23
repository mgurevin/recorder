# Releasing

Recorder and the optional `otelrecorder` module use the same semantic version,
but they are separate Go modules and therefore have separate signed tags. The
release is deliberately split at those two trust boundaries. Deterministic
editing and verification are automated; signed commits, signed tags, pushes,
and the final GitHub Release remain explicit operator actions.

The commands below use `0.6.0` as an example. Run them from the repository root
with Node.js 24 active. Required local tools are `go`, `golangci-lint`,
`govulncheck`, `syft`, `npm`, `git`, `gpg`, and authenticated `gh`.

## 1. Prepare and verify the root release

Start from a clean, up-to-date `main` branch. Keep the `Unreleased` changelog
entries grouped under `Added`, `Changed`, `Fixed`, and `Security` as applicable,
then run:

```bash
make release-prepare VERSION=0.6.0
make release-check VERSION=0.6.0
```

`release-prepare` performs the mechanical edits together:

- moves `Unreleased` to a dated `0.6.0` section and updates comparison links;
- updates the Inspector package and lock-file versions;
- updates the HAR `creatorVersion` and its integration-test assertion; and
- verifies that all release-owned versions agree.

Set `RELEASE_DATE=YYYY-MM-DD` to override the default UTC date. The command
refuses an empty `Unreleased` section, an existing release section, malformed
versions, or an inconsistent file layout.

`release-check` runs the complete local gate: version consistency, `make check`,
API comparison, reachable-vulnerability and npm audits, and all three versioned
SBOM validations. Review the resulting diff and release notes, then create and
push the signed preparation commit:

```bash
make release-notes VERSION=0.6.0
git add CHANGELOG.md har.go integration_test.go inspector/package.json \
  inspector/package-lock.json
git commit -S -m "chore: prepare v0.6.0 release"
git push origin main
```

The generated notes are written to `build/release/v0.6.0.md`. Wait for both CI
and **Deploy Inspector to GitHub Pages** to pass before tagging.

## 2. Publish the root module tag

Create the root tag only on the verified release-preparation commit:

```bash
git tag -s v0.6.0 -m "v0.6.0"
git push origin v0.6.0
GOPROXY=https://proxy.golang.org go list -m github.com/mgurevin/recorder@v0.6.0
```

Do not continue until the final command resolves the exact version. The root
tag intentionally precedes the `otelrecorder` dependency update because the
nested module must depend on an already published root module.

## 3. Prepare and publish `otelrecorder`

Run:

```bash
make release-otel-prepare VERSION=0.6.0
```

This target requires the local root tag, updates `otelrecorder/go.mod` to the
published root version, runs `go mod tidy`, race tests, vet, repository lint,
and API comparison. It also removes API exceptions belonging to the root module
because `v0.6.0` is now its comparison baseline.

Review the diff, then create and push a second signed commit:

```bash
git add otelrecorder/go.mod otelrecorder/go.sum \
  api/compatibility-exceptions.txt
git commit -S -m "chore(otelrecorder): require recorder v0.6.0"
git push origin main
```

Wait for CI to pass, then publish the nested tag:

```bash
git tag -s otelrecorder/v0.6.0 -m "otelrecorder/v0.6.0"
git push origin otelrecorder/v0.6.0
GOPROXY=https://proxy.golang.org \
  go list -m github.com/mgurevin/recorder/otelrecorder@v0.6.0
```

Never publish `otelrecorder` with a local `replace` directive, a pseudo-version,
or a dependency on an unpublished root version.

## 4. Finalize API baselines

After both tags exist, run:

```bash
make release-finalize VERSION=0.6.0
git diff -- api/compatibility-exceptions.txt
```

The target removes any `otelrecorder` exceptions made obsolete by its new tag
and verifies the complete API report against both new baselines. If it changes
the exception file, commit and push that cleanup and wait for CI and Pages:

```bash
git add api/compatibility-exceptions.txt
git commit -S -m "chore: retire v0.5.0 API exceptions"
git push origin main
```

No commit is needed when the file is unchanged.

### API compatibility exceptions

`api/compatibility-exceptions.txt` is a temporary review record, not a general
allowlist. Add an entry only when all of the following are true:

1. `make api-diff` reports a deliberate pre-v1 incompatible API change.
2. The migration and release timing have been reviewed.
3. The entry exactly matches the module and `apidiff` output.

Never add an exception merely to make CI pass. Root exceptions remain necessary
until the new root tag exists; `otelrecorder` exceptions remain necessary until
the nested tag exists. The staged targets retire each module's section at the
correct time. A stale exception is intentionally a CI failure because it can
otherwise hide a future incompatibility.

Wire schema compatibility is separate from Go API compatibility. Any stable
`_recorder` schema change must update the schema, Inspector types, and contract
tests together; incompatible stable schema changes require a new schema version
rather than an API exception.

## 5. Create and verify the GitHub Release

Regenerate the notes after the final commits if necessary, then publish:

```bash
make release-notes VERSION=0.6.0
gh release create v0.6.0 --verify-tag --title v0.6.0 \
  --notes-file build/release/v0.6.0.md
```

Publishing the release triggers **Publish release SBOM**. Wait for it to pass
and confirm that the release contains exactly these versioned assets:

- `recorder-0.6.0.spdx.json`
- `recorder-otelrecorder-0.6.0.spdx.json`
- `recorder-inspector-0.6.0.spdx.json`

The workflow generates the Recorder SBOM without `docs`, `otelrecorder`, or the
Inspector; checks out the matching nested tag for the OpenTelemetry SBOM; and
attests the provenance of all three files before upload. The release is complete
only when both module versions resolve through the Go proxy, CI and Pages are
green, the three assets exist, and all three attestation steps succeeded.

Finally verify signatures and repository state:

```bash
git tag -v v0.6.0
git tag -v otelrecorder/v0.6.0
git status --short --branch
```
