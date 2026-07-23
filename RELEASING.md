# Releasing

The root module and `otelrecorder` are released with the same semantic version.
In the commands below, replace `X.Y.Z` with the version being released.

## 1. Prepare the release

1. Move the changes in `CHANGELOG.md` from **Unreleased** to a dated `X.Y.Z`
   section, then add a new empty **Unreleased** section.
2. Update the Inspector version:

   ```bash
   cd inspector
   npm version X.Y.Z --no-git-tag-version
   npm ci
   cd ..
   ```

3. Set `creatorVersion` in `har.go` to `X.Y.Z` and update the matching
   assertion in `integration_test.go`. Keep `creator.name` equal to
   `github.com/mgurevin/recorder`.
4. Review every exported API and any `_recorder` wire-schema change. Update
   `schema/recorder-har-v1.schema.json`, Inspector types, and the API/schema
   contract tests together. Run `make api-diff` to compare the root, `hario`,
   `hartest`, and `otelrecorder` public APIs with their latest release tags.
   The pinned `apidiff` report distinguishes compatible additions from
   incompatible changes. Any pre-v1 incompatibility must be an intentional,
   temporary entry in `api/compatibility-exceptions.txt`; never add an exception
   merely to make CI pass. Remove exceptions once the release containing those
   changes becomes the comparison baseline. A stable schema change requires a
   new schema version rather than silent reinterpretation.
5. Run the complete release check:

   ```bash
   make check
   make api-diff
   make vulncheck
   make sbom-check SBOM_VERSION=vX.Y.Z
   ```

   The required `golangci-lint` version is declared in `.golangci-version`.
   `make lint` fails fast when the local binary differs, and CI reads the same
   file so local and hosted checks enforce identical rules.

6. Review the diff and commit the release preparation.

## 2. Release the root module

Create and push a signed tag from the release commit:

```bash
git tag -s vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

Wait until the root module is available before continuing.

## 3. Release `otelrecorder`

1. In `otelrecorder/go.mod`, require the root module at `vX.Y.Z` and remove the
   development-only local `replace` directive.
2. Refresh and verify the nested module:

   ```bash
   cd otelrecorder
   go mod tidy
   go test -race ./...
   go vet ./...
   cd ..
   make lint
   ```

3. Commit `otelrecorder/go.mod` and `otelrecorder/go.sum`, then create and push
   its signed tag:

   ```bash
   git tag -s otelrecorder/vX.Y.Z -m "otelrecorder/vX.Y.Z"
   git push origin otelrecorder/vX.Y.Z
   ```

Never publish `otelrecorder` with a local `replace` directive or a placeholder
pseudo-version.

## 4. Publish release notes

Create the GitHub release for `vX.Y.Z` using the matching `CHANGELOG.md`
section, and confirm that CI passed for the release commit. Publishing the
release triggers the SBOM workflow, which generates
`recorder-X.Y.Z.spdx.json` for the standalone Recorder module,
`recorder-otelrecorder-X.Y.Z.spdx.json` for the optional OpenTelemetry module,
and `recorder-inspector-X.Y.Z.spdx.json` for the Inspector from the tagged
source. The Recorder inventory must exclude `docs`, `otelrecorder`, and
`inspector`; the OpenTelemetry inventory is generated from the matching
`otelrecorder/vX.Y.Z` tag rather than the root tag's nested working copy. The
workflow attests each file's provenance and attaches all three to the release.
Confirm that all three SBOM assets and attestations were created before
considering the release complete.

After both signed tags exist, remove compatibility exceptions made obsolete by
the release in the first post-release commit. Its push reruns CI and the
**Deploy Inspector to GitHub Pages** workflow against the new root and
`otelrecorder` baselines. Confirm that the published API compatibility report
uses those tags and that the README badge links to the report. Stale exceptions
cause the compatibility check to fail rather than silently masking new changes.
