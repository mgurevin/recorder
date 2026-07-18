# Releasing

This repository contains the root module and the nested `otelrecorder` module.
They are released together at the same semantic version.

1. Ensure `CHANGELOG.md` describes all user-visible changes under Unreleased.
2. Replace Unreleased with the version and release date, add a fresh
   Unreleased section, and commit the release preparation.
3. Run `go test -race ./...` and `go vet ./...` in both Go modules. Run
   `npm ci`, `npm test`, and `npm run build` in `inspector`.
4. Create and push the signed root-module tag `vX.Y.Z` from the release commit.
5. After that tag is available, set the root-module requirement in
   `otelrecorder/go.mod` to `vX.Y.Z`, remove its local `replace`, and run
   `go mod tidy`.
6. Run `go test -race ./...` and `go vet ./...` in `otelrecorder` again using
   the published root-module dependency.
7. Commit the nested-module version update and create the signed
   `otelrecorder/vX.Y.Z` tag.
8. Push the nested tag and publish GitHub release notes from the changelog.

Do not publish the nested module with its development-only local `replace`
directive or placeholder pseudo-version.
