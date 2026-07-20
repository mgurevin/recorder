# Releasing

This repository contains the root module and the nested `otelrecorder` module.
They are released together at the same semantic version.

1. Ensure `CHANGELOG.md` describes all user-visible changes under Unreleased.
2. Replace Unreleased with the version and release date, add a fresh
   Unreleased section, and update the Inspector version in
   `inspector/package.json` / `package-lock.json`.
3. Update `creatorVersion` in `har.go` to exactly `X.Y.Z` (without the `v`
   prefix) for every release. Update the exact assertion in
   `integration_test.go` at the same time; it also pins `creator.name` to the
   module FQDN `github.com/mgurevin/recorder`. Commit the release preparation
   only after these metadata checks agree.
4. Run `go test -race ./...` and `go vet ./...` in both Go modules. Run
   `npm ci`, `npm test`, and `npm run build` in `inspector`. Compile and smoke
   every root benchmark with
   `go test -run '^$' -bench '^Benchmark' -benchtime=1x`; for performance-path
   changes, collect repeated before/after `-benchmem` results and compare them
   with `benchstat`, then update `BENCHMARK.md` when the recorded snapshot or
   guidance materially changes.
5. Create and push the signed root-module tag `vX.Y.Z` from the release commit.
6. After that tag is available, set the root-module requirement in
   `otelrecorder/go.mod` to `vX.Y.Z`, remove its local `replace`, and run
   `go mod tidy`.
7. Run `go test -race ./...` and `go vet ./...` in `otelrecorder` again using
   the published root-module dependency.
8. Commit the nested-module version update and create the signed
   `otelrecorder/vX.Y.Z` tag.
9. Push the nested tag and publish GitHub release notes from the changelog.

Do not publish the nested module with its development-only local `replace`
directive or placeholder pseudo-version.
