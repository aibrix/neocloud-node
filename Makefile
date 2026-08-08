SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

GO ?= go
CONTAINER_TOOL ?= docker

BINARY ?= neocloud-node
OUTPUT_DIR ?= bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE ?= ghcr.io/aibrix/neocloud-node:$(VERSION)
BUILDER_IMAGE ?= golang:1.25.0-bookworm
RUNTIME_IMAGE ?= gcr.io/distroless/static-debian12:nonroot
GO_TEST_FLAGS ?= -count=1

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make <target>\n"} /^[a-zA-Z_0-9-]+:.*?##/ {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: fmt
fmt: ## Format Go source files.
	gofmt -w cmd pkg deploy

.PHONY: fmt-check
fmt-check: ## Fail when Go source files are not formatted.
	@files="$$(gofmt -l cmd pkg deploy)"; \
	if [[ -n "$${files}" ]]; then \
		echo "The following files need gofmt:"; \
		echo "$${files}"; \
		exit 1; \
	fi

.PHONY: tidy-check
tidy-check: ## Verify go.mod and go.sum are tidy without changing them.
	$(GO) mod tidy -diff

.PHONY: mod-verify
mod-verify: ## Verify downloaded module content.
	$(GO) mod verify

.PHONY: vet
vet: ## Run Go static analysis.
	$(GO) vet ./...

.PHONY: test
test: ## Run unit and manifest tests.
	$(GO) test $(GO_TEST_FLAGS) ./...

.PHONY: test-race
test-race: ## Run tests with the race detector.
	$(GO) test -race $(GO_TEST_FLAGS) ./...

.PHONY: coverage
coverage: ## Write a unit-test coverage profile to coverage.out.
	$(GO) test $(GO_TEST_FLAGS) -coverprofile=coverage.out ./...

.PHONY: build
build: ## Build the controller binary.
	mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -ldflags="-s -w" -o $(OUTPUT_DIR)/$(BINARY) ./cmd/neocloud-node

.PHONY: docker-build
docker-build: ## Build the controller container image.
	$(CONTAINER_TOOL) build \
		--build-arg BUILDER_IMAGE=$(BUILDER_IMAGE) \
		--build-arg RUNTIME_IMAGE=$(RUNTIME_IMAGE) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE) .

.PHONY: docker-push
docker-push: ## Push IMAGE after it has been built.
	$(CONTAINER_TOOL) push $(IMAGE)

.PHONY: verify
verify: fmt-check tidy-check mod-verify vet test build ## Run all local presubmit checks.

.PHONY: clean
clean: ## Remove local build and coverage output.
	$(RM) -r $(OUTPUT_DIR) coverage.out
