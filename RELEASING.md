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
   contract tests together; a stable schema change requires a new schema
   version rather than silent reinterpretation.
5. Run the complete release check:

   ```bash
   make check
   make vulncheck
   make sbom-check SBOM_VERSION=vX.Y.Z
   ```

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
   golangci-lint run ./...
   go test -race ./...
   go vet ./...
   cd ..
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
`recorder-vX.Y.Z.spdx.json` from the tagged source, attests its provenance, and
attaches it to the release. Confirm that the SBOM asset and attestation were
created before considering the release complete.
