GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION_FILE ?= .golangci-version
GOLANGCI_LINT_VERSION := $(strip $(shell cat $(GOLANGCI_LINT_VERSION_FILE)))
GO ?= go
GOVULNCHECK ?= govulncheck
APIDIFF_VERSION_FILE ?= .apidiff-version
APIDIFF_VERSION := $(strip $(shell cat $(APIDIFF_VERSION_FILE)))
APIDIFF ?= $(CURDIR)/build/tools/apidiff
APIDIFF_INSTALLED_VERSION ?= $(APIDIFF).version
APIDIFF_GOCACHE ?= $(CURDIR)/build/cache/apidiff
NPM ?= npm
SYFT ?= syft
SYFT_CHECK_FOR_APP_UPDATE ?= false
SBOM_VERSION ?= $(shell git describe --tags --always --dirty)
SBOM_ASSET_VERSION ?= $(patsubst v%,%,$(SBOM_VERSION))
SBOM_DIR ?= build/sbom
RECORDER_SBOM_FILE ?= $(SBOM_DIR)/recorder-$(SBOM_ASSET_VERSION).spdx.json
OTELRECORDER_SBOM_FILE ?= $(SBOM_DIR)/recorder-otelrecorder-$(SBOM_ASSET_VERSION).spdx.json
INSPECTOR_SBOM_FILE ?= $(SBOM_DIR)/recorder-inspector-$(SBOM_ASSET_VERSION).spdx.json
OTELRECORDER_SBOM_SOURCE ?= ./otelrecorder

export SYFT_CHECK_FOR_APP_UPDATE

.DEFAULT_GOAL := check

.PHONY: lint-version format lint test test-race vet api-diff-tool api-diff go-vulncheck inspector-audit vulncheck go-coverage inspector-coverage coverage coverage-report inspector-check benchmark-smoke sbom sbom-check release-tool-test release-prepare release-verify release-check release-notes release-otel-prepare release-finalize check

lint-version:
	@actual="$$($(GOLANGCI_LINT) version 2>/dev/null | sed -n 's/.* version \([^ ]*\).*/\1/p' | head -n 1)"; \
	if [ "$$actual" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) is required; found $${actual:-not installed}" >&2; \
		exit 1; \
	fi

format:
	$(GOLANGCI_LINT) fmt
	$(GOLANGCI_LINT) run --fix ./...
	cd otelrecorder && $(GOLANGCI_LINT) fmt
	cd otelrecorder && $(GOLANGCI_LINT) run --fix ./...
	cd docs/examples/content-decoders && $(GOLANGCI_LINT) fmt
	cd docs/examples/content-decoders && $(GOLANGCI_LINT) run --fix ./...
	cd inspector && $(NPM) run lint:fix

lint: lint-version
	$(GOLANGCI_LINT) fmt --diff
	$(GOLANGCI_LINT) run ./...
	cd otelrecorder && $(GOLANGCI_LINT) fmt --diff
	cd otelrecorder && $(GOLANGCI_LINT) run ./...
	cd docs/examples/content-decoders && $(GOLANGCI_LINT) fmt --diff
	cd docs/examples/content-decoders && $(GOLANGCI_LINT) run ./...
	cd inspector && $(NPM) run lint

test:
	$(GO) test ./...
	cd otelrecorder && $(GO) test ./...
	cd docs/examples/content-decoders && $(GO) test ./...
	cd inspector && $(NPM) test

test-race:
	$(GO) test -race ./...
	cd otelrecorder && $(GO) test -race ./...
	cd docs/examples/content-decoders && $(GO) test -race ./...

vet:
	$(GO) vet ./...
	cd otelrecorder && $(GO) vet ./...
	cd docs/examples/content-decoders && $(GO) vet ./...

api-diff-tool:
	@if [ ! -x "$(APIDIFF)" ] || [ "$$(cat "$(APIDIFF_INSTALLED_VERSION)" 2>/dev/null)" != "$(APIDIFF_VERSION)" ]; then \
		mkdir -p "$(dir $(APIDIFF))"; \
		mkdir -p "$(APIDIFF_GOCACHE)"; \
		GOCACHE="$(APIDIFF_GOCACHE)" GOBIN="$(dir $(APIDIFF))" $(GO) install golang.org/x/exp/cmd/apidiff@$(APIDIFF_VERSION); \
		printf '%s\n' "$(APIDIFF_VERSION)" >"$(APIDIFF_INSTALLED_VERSION)"; \
	fi

api-diff: api-diff-tool
	GOCACHE="$(APIDIFF_GOCACHE)" APIDIFF="$(APIDIFF)" ./scripts/api-diff.sh

go-vulncheck:
	$(GOVULNCHECK) ./...
	cd otelrecorder && $(GOVULNCHECK) ./...
	cd docs/examples/content-decoders && $(GOVULNCHECK) ./...

inspector-audit:
	cd inspector && $(NPM) run audit

vulncheck: go-vulncheck inspector-audit

go-coverage:
	mkdir -p build/coverage
	$(GO) test -covermode=atomic -coverprofile=build/coverage/go-recorder.out . ./hario ./hartest
	$(GO) test -covermode=atomic -coverprofile=build/coverage/go-cli.out ./cmd/recorder
	$(GO) test -covermode=atomic -coverprofile=build/coverage/go-root-examples.out ./docs/examples/...
	cd otelrecorder && $(GO) test -covermode=atomic -coverprofile=../build/coverage/go-otelrecorder.out ./...
	cd docs/examples/content-decoders && $(GO) test -covermode=atomic -coverprofile=../../../build/coverage/go-content-decoders.out ./...
	node scripts/merge-go-coverage.mjs build/coverage/go-examples.out build/coverage/go-root-examples.out build/coverage/go-content-decoders.out

inspector-coverage:
	cd inspector && $(NPM) run test:coverage

coverage: go-coverage inspector-coverage

coverage-report: coverage
	$(GO) tool cover -html=build/coverage/go-recorder.out -o build/coverage/go-recorder.html
	$(GO) tool cover -html=build/coverage/go-cli.out -o build/coverage/go-cli.html
	node scripts/go-workspace-cover.mjs "$(GO)" "$(CURDIR)/build/coverage/go-examples.out" "$(CURDIR)/build/coverage/go-examples.html" "$(CURDIR)" "$(CURDIR)/docs/examples/content-decoders"
	cd otelrecorder && $(GO) tool cover -html=../build/coverage/go-otelrecorder.out -o ../build/coverage/go-otelrecorder.html
	node scripts/coverage-report.mjs

inspector-check:
	cd inspector && $(NPM) run test:coverage
	cd inspector && $(NPM) run build

benchmark-smoke:
	$(GO) test -run '^$$' -bench '^Benchmark' -benchtime=1x
	$(GO) test -run '^$$' -bench '^Benchmark' -benchtime=1x ./hario ./hartest

sbom:
	mkdir -p "$(SBOM_DIR)"
	$(SYFT) scan dir:. \
		--source-name github.com/mgurevin/recorder \
		--source-version "$(SBOM_VERSION)" \
		--exclude './.github/**' \
		--exclude './docs/**' \
		--exclude './inspector/**' \
		--exclude './otelrecorder/**' \
		--exclude './.git/**' \
		--exclude './build/**' \
		--output "spdx-json=$(RECORDER_SBOM_FILE)"
	$(SYFT) scan "dir:$(OTELRECORDER_SBOM_SOURCE)" \
		--source-name github.com/mgurevin/recorder/otelrecorder \
		--source-version "$(SBOM_VERSION)" \
		--output "spdx-json=$(OTELRECORDER_SBOM_FILE)"
	$(SYFT) scan dir:./inspector \
		--source-name github.com/mgurevin/recorder/inspector \
		--source-version "$(SBOM_VERSION)" \
		--exclude './node_modules/**' \
		--exclude './dist/**' \
		--exclude './coverage/**' \
		--output "spdx-json=$(INSPECTOR_SBOM_FILE)"

sbom-check: sbom
	test -s "$(RECORDER_SBOM_FILE)"
	test -s "$(OTELRECORDER_SBOM_FILE)"
	test -s "$(INSPECTOR_SBOM_FILE)"
	$(SYFT) convert "$(RECORDER_SBOM_FILE)" --output syft-table >/dev/null
	$(SYFT) convert "$(OTELRECORDER_SBOM_FILE)" --output syft-table >/dev/null
	$(SYFT) convert "$(INSPECTOR_SBOM_FILE)" --output syft-table >/dev/null

release-tool-test:
	node --test scripts/release.test.mjs scripts/badge-cache.test.mjs

release-prepare:
	@test -n "$(VERSION)" || (echo "VERSION=X.Y.Z is required" >&2; exit 1)
	node scripts/release.mjs prepare "$(VERSION)"

release-verify:
	@test -n "$(VERSION)" || (echo "VERSION=X.Y.Z is required" >&2; exit 1)
	node scripts/release.mjs verify "$(VERSION)"

release-check: release-verify
	$(MAKE) check
	$(MAKE) api-diff
	$(MAKE) vulncheck
	$(MAKE) sbom-check SBOM_VERSION="v$(VERSION)"

release-notes:
	@test -n "$(VERSION)" || (echo "VERSION=X.Y.Z is required" >&2; exit 1)
	@mkdir -p build/release
	node scripts/release.mjs notes "$(VERSION)" >"build/release/v$(VERSION).md"
	@echo "wrote build/release/v$(VERSION).md"

release-otel-prepare:
	@test -n "$(VERSION)" || (echo "VERSION=X.Y.Z is required" >&2; exit 1)
	@git rev-parse --verify --quiet "refs/tags/v$(VERSION)" >/dev/null || \
		(echo "signed root tag v$(VERSION) is required" >&2; exit 1)
	git verify-tag "v$(VERSION)"
	@test "$$(GOPROXY=https://proxy.golang.org $(GO) list -m github.com/mgurevin/recorder@v$(VERSION))" = \
		"github.com/mgurevin/recorder v$(VERSION)" || \
		(echo "root module v$(VERSION) is not available through the Go proxy" >&2; exit 1)
	node scripts/release.mjs retire-root "$(VERSION)"
	cd otelrecorder && $(GO) mod edit -require="github.com/mgurevin/recorder@v$(VERSION)"
	cd otelrecorder && $(GO) mod edit -dropreplace="github.com/mgurevin/recorder"
	cd otelrecorder && $(GO) mod tidy
	cd otelrecorder && $(GO) test -race ./...
	cd otelrecorder && $(GO) vet ./...
	$(MAKE) lint
	$(MAKE) api-diff

release-finalize:
	@test -n "$(VERSION)" || (echo "VERSION=X.Y.Z is required" >&2; exit 1)
	@git rev-parse --verify --quiet "refs/tags/otelrecorder/v$(VERSION)" >/dev/null || \
		(echo "signed otelrecorder/v$(VERSION) tag is required" >&2; exit 1)
	git verify-tag "otelrecorder/v$(VERSION)"
	@test "$$(GOPROXY=https://proxy.golang.org $(GO) list -m github.com/mgurevin/recorder/otelrecorder@v$(VERSION))" = \
		"github.com/mgurevin/recorder/otelrecorder v$(VERSION)" || \
		(echo "otelrecorder v$(VERSION) is not available through the Go proxy" >&2; exit 1)
	node scripts/release.mjs retire-otel "$(VERSION)"
	$(MAKE) api-diff

check: lint test-race vet inspector-check benchmark-smoke release-tool-test
