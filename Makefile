# Wegweiser build and development tasks.
#
# Run `make` or `make help` for the available targets.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BINARY  := weg
BIN_DIR := bin
PKG     := github.com/wegweiserzone/wegweiser
INFO    := $(PKG)/internal/buildinfo

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# --verify keeps git quiet on an unborn HEAD. An empty value is deliberate:
# buildinfo then falls back to the VCS metadata Go embeds, which is more
# accurate than a placeholder.
COMMIT  ?= $(shell git rev-parse --verify HEAD 2>/dev/null)
# SOURCE_DATE_EPOCH keeps the build reproducible for distribution packagers.
DATE    ?= $(shell date -u -d "@$${SOURCE_DATE_EPOCH:-$$(date +%s)}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X '$(INFO).version=$(VERSION)' \
	-X '$(INFO).commit=$(COMMIT)' \
	-X '$(INFO).date=$(DATE)'

GOBIN         := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(GOBIN)/golangci-lint
GOVULNCHECK   := $(GOBIN)/govulncheck
OAPI_CODEGEN  := $(GOBIN)/oapi-codegen

FUZZTIME ?= 30s

# The web interface is built with npm but is not needed to build the binary:
# the output is committed (docs/decisions.md D16), so `go build` works on a
# machine with no Node installed.
NPM := npm --prefix web

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## --- build ---------------------------------------------------------------

.PHONY: build
build: ## Build the weg binary into bin/
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/weg
	@echo "built $(BIN_DIR)/$(BINARY) $(VERSION)"

.PHONY: install
install: ## Install weg into GOPATH/bin
	CGO_ENABLED=0 go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/weg

IMAGE ?= wegweiser

.PHONY: image
image: ## Build the container image
	podman build --format docker \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-f packaging/Containerfile -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .
	@echo "built $(IMAGE):$(VERSION)"

## --- web ------------------------------------------------------------------

.PHONY: web-deps
web-deps: ## Install the web interface's dependencies and its test browser
	$(NPM) ci
	$(NPM) exec -- playwright install firefox

.PHONY: web-generate
web-generate: ## Regenerate the web interface's API types from the spec
	@test -d web/node_modules || { echo "web/node_modules is missing; run 'make web-deps'"; exit 1; }
	$(NPM) run generate

.PHONY: web
web: ## Build the web interface into internal/api/dist
	@test -d web/node_modules || { echo "web/node_modules is missing; run 'make web-deps'"; exit 1; }
	$(NPM) run build

# Both artefacts the interface derives from sources are committed: the API
# types, so that a field renamed in the spec breaks this build rather than
# production, and the build output, so that installing the binary needs no
# Node. This target keeps both honest, the same way generate-check does for the
# Go side. The build is deterministic: the version SvelteKit stamps into the
# document is pinned in svelte.config.js, because its default is a timestamp
# and this could then never be green.
.PHONY: web-check
web-check: ## Fail if the committed web types or build are out of date
	@$(MAKE) --no-print-directory web-generate web
	@if [ -n "$$(git status --porcelain -- internal/api/dist web/src/lib/api/schema.d.ts)" ]; then \
		echo "the committed web output is out of date; run 'make web-generate web' and commit the result"; \
		git --no-pager status --short -- internal/api/dist web/src/lib/api/schema.d.ts; \
		exit 1; \
	fi

.PHONY: web-lint
web-lint: ## Type-check the web interface
	$(NPM) run check

# What nothing else here can catch is "the interface does not render at all":
# a component that throws produces a blank page, a console message, and a
# perfectly good 200. These start a real weg on ports the kernel picks and
# drive it through a browser. A smoke suite on purpose — anything past it is
# cheaper and steadier to test against the API.
.PHONY: web-test
web-test: build ## Run the browser smoke tests against a real server
	@test -d web/node_modules || { echo "web/node_modules is missing; run 'make web-deps'"; exit 1; }
	$(NPM) test

## --- demo -----------------------------------------------------------------

# A server to look at, on unprivileged ports with a temporary database. It
# needs no capability and no root, and it fills itself with what a small
# network actually looks like — including the reverse zones that fill
# themselves, which is the thing worth seeing.
.PHONY: demo
demo: ## Run a Wegweiser with something in it, to look at
	@scripts/demo.sh start

.PHONY: demo-stop
demo-stop: ## Stop the demo and remove its database
	@scripts/demo.sh stop

.PHONY: unit-check
unit-check: ## Check the systemd unit for mistakes and score its exposure
	systemd-analyze verify packaging/systemd/wegweiser.service || true
	systemd-analyze security --offline=true packaging/systemd/wegweiser.service | tail -1

.PHONY: clean
clean: ## Remove build and coverage artefacts
	rm -rf $(BIN_DIR) coverage.out coverage.html

## --- verify --------------------------------------------------------------

.PHONY: check
check: tidy-check generate-check web-check web-lint web-test fmt-check vet lint test ## Run every check CI runs

.PHONY: test
test: ## Run tests with the race detector
	go test -race -shuffle=on ./...

.PHONY: test-short
test-short: ## Run tests without the race detector
	go test -shuffle=on ./...

.PHONY: cover
cover: ## Run tests and report coverage
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Open the coverage report in a browser
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: bench
bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format the code
	$(GOLANGCI_LINT) fmt ./...

.PHONY: fmt-check
fmt-check: $(GOLANGCI_LINT) ## Fail if the code is not formatted
	$(GOLANGCI_LINT) fmt --diff ./...

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod or go.sum would change
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak 2>/dev/null || true
	@go mod tidy
	@if ! diff -q go.mod go.mod.bak >/dev/null 2>&1 || ! diff -q go.sum go.sum.bak >/dev/null 2>&1; then \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum 2>/dev/null || true; \
		echo "go.mod or go.sum is not tidy; run 'make tidy'"; exit 1; \
	fi
	@rm -f go.mod.bak go.sum.bak

.PHONY: generate
generate: $(OAPI_CODEGEN) ## Regenerate the API models, server and client from the spec
	cd internal/api/gen && $(OAPI_CODEGEN) --config config.yaml ../openapi.yaml

# The generated code is committed, so that building the project needs no code
# generator. This target is what keeps the committed copy honest.
.PHONY: generate-check
generate-check: ## Fail if the generated API code is out of date
	@$(MAKE) --no-print-directory generate
	@if [ -n "$$(git status --porcelain -- internal/api/gen)" ]; then \
		echo "internal/api/gen is out of date; run 'make generate' and commit the result"; \
		git --no-pager diff -- internal/api/gen; \
		exit 1; \
	fi

.PHONY: vuln
vuln: $(GOVULNCHECK) ## Check dependencies for known vulnerabilities
	$(GOVULNCHECK) ./...

## --- fuzzing -------------------------------------------------------------

# The DNS parser is fuzzed from day one (see docs/conventions.md, Quality gates). This
# target runs every fuzz target briefly, which is what CI needs; long campaigns
# are run separately with a larger FUZZTIME.
.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME (default 30s)
	@set -euo pipefail; \
	found=0; \
	for pkg in $$(go list ./...); do \
		dir=$$(go list -f '{{.Dir}}' $$pkg); \
		targets=$$(grep -hoE '^func (Fuzz[A-Za-z0-9_]*)' $$dir/*_test.go 2>/dev/null \
			| sed 's/^func //' | sort -u || true); \
		for t in $$targets; do \
			found=1; \
			echo "==> fuzzing $$pkg $$t for $(FUZZTIME)"; \
			go test -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) $$pkg; \
		done; \
	done; \
	if [ $$found -eq 0 ]; then echo "no fuzz targets yet"; fi

## --- tooling -------------------------------------------------------------

.PHONY: tools
tools: $(GOLANGCI_LINT) $(GOVULNCHECK) $(OAPI_CODEGEN) ## Install the development tools

$(GOLANGCI_LINT):
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

$(GOVULNCHECK):
	go install golang.org/x/vuln/cmd/govulncheck@latest

$(OAPI_CODEGEN):
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
