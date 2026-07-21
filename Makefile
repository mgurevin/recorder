GOLANGCI_LINT ?= golangci-lint
GO ?= go
NPM ?= npm

.DEFAULT_GOAL := check

.PHONY: format lint test test-race vet inspector-check benchmark-smoke check

format:
	$(GOLANGCI_LINT) fmt
	$(GOLANGCI_LINT) run --fix ./...
	cd otelrecorder && $(GOLANGCI_LINT) fmt
	cd otelrecorder && $(GOLANGCI_LINT) run --fix ./...
	cd inspector && $(NPM) run lint:fix

lint:
	$(GOLANGCI_LINT) fmt --diff
	$(GOLANGCI_LINT) run ./...
	cd otelrecorder && $(GOLANGCI_LINT) fmt --diff
	cd otelrecorder && $(GOLANGCI_LINT) run ./...
	cd inspector && $(NPM) run lint

test:
	$(GO) test ./...
	cd otelrecorder && $(GO) test ./...
	cd inspector && $(NPM) test

test-race:
	$(GO) test -race ./...
	cd otelrecorder && $(GO) test -race ./...

vet:
	$(GO) vet ./...
	cd otelrecorder && $(GO) vet ./...

inspector-check:
	cd inspector && $(NPM) test
	cd inspector && $(NPM) run build

benchmark-smoke:
	$(GO) test -run '^$$' -bench '^Benchmark' -benchtime=1x

check: lint test-race vet inspector-check benchmark-smoke
