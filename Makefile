GOLANGCI_LINT ?= golangci-lint
GO ?= go
GOVULNCHECK ?= govulncheck
NPM ?= npm
SYFT ?= syft
SYFT_CHECK_FOR_APP_UPDATE ?= false
SBOM_VERSION ?= $(shell git describe --tags --always --dirty)
SBOM_ASSET_VERSION ?= $(patsubst v%,%,$(SBOM_VERSION))
SBOM_DIR ?= build/sbom
GO_SBOM_FILE ?= $(SBOM_DIR)/recorder-$(SBOM_ASSET_VERSION).spdx.json
INSPECTOR_SBOM_FILE ?= $(SBOM_DIR)/recorder-inspector-$(SBOM_ASSET_VERSION).spdx.json

export SYFT_CHECK_FOR_APP_UPDATE

.DEFAULT_GOAL := check

.PHONY: format lint test test-race vet go-vulncheck inspector-audit vulncheck go-coverage inspector-coverage coverage coverage-report inspector-check benchmark-smoke sbom sbom-check check

format:
	$(GOLANGCI_LINT) fmt
	$(GOLANGCI_LINT) run --fix ./...
	cd otelrecorder && $(GOLANGCI_LINT) fmt
	cd otelrecorder && $(GOLANGCI_LINT) run --fix ./...
	cd docs/examples/content-decoders && $(GOLANGCI_LINT) fmt
	cd docs/examples/content-decoders && $(GOLANGCI_LINT) run --fix ./...
	cd inspector && $(NPM) run lint:fix

lint:
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
	$(GO) test -covermode=atomic -coverprofile=build/coverage/go-root-examples.out ./docs/examples/...
	cd otelrecorder && $(GO) test -covermode=atomic -coverprofile=../build/coverage/go-otelrecorder.out ./...
	cd docs/examples/content-decoders && $(GO) test -covermode=atomic -coverprofile=../../../build/coverage/go-content-decoders.out ./...
	node scripts/merge-go-coverage.mjs build/coverage/go-examples.out build/coverage/go-root-examples.out build/coverage/go-content-decoders.out

inspector-coverage:
	cd inspector && $(NPM) run test:coverage

coverage: go-coverage inspector-coverage

coverage-report: coverage
	$(GO) tool cover -html=build/coverage/go-recorder.out -o build/coverage/go-recorder.html
	$(GO) tool cover -html=build/coverage/go-root-examples.out -o build/coverage/go-root-examples.html
	cd docs/examples/content-decoders && $(GO) tool cover -html=../../../build/coverage/go-content-decoders.out -o ../../../build/coverage/go-content-decoders.html
	cd otelrecorder && $(GO) tool cover -html=../build/coverage/go-otelrecorder.out -o ../build/coverage/go-otelrecorder.html
	node scripts/coverage-report.mjs

inspector-check:
	cd inspector && $(NPM) run test:coverage
	cd inspector && $(NPM) run build

benchmark-smoke:
	$(GO) test -run '^$$' -bench '^Benchmark' -benchtime=1x

sbom:
	mkdir -p "$(SBOM_DIR)"
	$(SYFT) scan dir:. \
		--source-name github.com/mgurevin/recorder \
		--source-version "$(SBOM_VERSION)" \
		--exclude './.github/**' \
		--exclude './inspector/**' \
		--exclude './.git/**' \
		--exclude './build/**' \
		--output "spdx-json=$(GO_SBOM_FILE)"
	$(SYFT) scan dir:./inspector \
		--source-name github.com/mgurevin/recorder/inspector \
		--source-version "$(SBOM_VERSION)" \
		--exclude './node_modules/**' \
		--exclude './dist/**' \
		--exclude './coverage/**' \
		--output "spdx-json=$(INSPECTOR_SBOM_FILE)"

sbom-check: sbom
	test -s "$(GO_SBOM_FILE)"
	test -s "$(INSPECTOR_SBOM_FILE)"
	$(SYFT) convert "$(GO_SBOM_FILE)" --output syft-table >/dev/null
	$(SYFT) convert "$(INSPECTOR_SBOM_FILE)" --output syft-table >/dev/null

check: lint test-race vet inspector-check benchmark-smoke
