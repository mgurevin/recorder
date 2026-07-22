GOLANGCI_LINT ?= golangci-lint
GO ?= go
GOVULNCHECK ?= govulncheck
NPM ?= npm
SYFT ?= syft
SYFT_CHECK_FOR_APP_UPDATE ?= false
SBOM_VERSION ?= $(shell git describe --tags --always --dirty)
SBOM_ASSET_VERSION ?= $(patsubst v%,%,$(SBOM_VERSION))
SBOM_DIR ?= build/sbom
SBOM_FILE ?= $(SBOM_DIR)/recorder-$(SBOM_ASSET_VERSION).spdx.json

export SYFT_CHECK_FOR_APP_UPDATE

.DEFAULT_GOAL := check

.PHONY: format lint test test-race vet go-vulncheck inspector-audit vulncheck go-coverage inspector-coverage coverage inspector-check benchmark-smoke sbom sbom-check check

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
	$(GO) test -covermode=atomic -coverprofile=build/coverage/go-root.out . ./docs/examples/csv-redactor
	cd otelrecorder && $(GO) test -covermode=atomic -coverprofile=../build/coverage/go-otelrecorder.out ./...
	cd docs/examples/content-decoders && $(GO) test -covermode=atomic -coverprofile=../../../build/coverage/go-content-decoders.out ./...

inspector-coverage:
	cd inspector && $(NPM) run test:coverage

coverage: go-coverage inspector-coverage

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
		--exclude './.git/**' \
		--exclude './build/**' \
		--exclude './inspector/node_modules/**' \
		--exclude './inspector/dist/**' \
		--exclude './inspector/coverage/**' \
		--output "spdx-json=$(SBOM_FILE)"

sbom-check: sbom
	test -s "$(SBOM_FILE)"
	$(SYFT) convert "$(SBOM_FILE)" --output syft-table >/dev/null

check: lint test-race vet inspector-check benchmark-smoke
