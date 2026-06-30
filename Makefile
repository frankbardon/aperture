.PHONY: build run clean test fmt vet lint

BINARY_NAME=aperture
BUILD_DIR=bin
GO=go

# Aperture is pure-Go end to end. Unlike orbit (which links pulse's h3-go geo
# grouper through CGO), Aperture consumes only pulse's pure-Go expression
# evaluator, so CGO stays hard-off. Strip symbols + trimpath for release builds.
CGO_ENABLED=0
export CGO_ENABLED
BUILD_LDFLAGS=-s -w
BUILD_FLAGS=-ldflags="$(BUILD_LDFLAGS)" -trimpath

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

build:
	$(GO) build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/aperture

run: build
	$(BUILD_DIR)/$(BINARY_NAME)

clean:
	rm -rf $(BUILD_DIR)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

# lint runs go vet plus a static analyser when one is available. In a clean
# environment without staticcheck / golangci-lint on PATH the target degrades to
# a notice so `make lint` never hard-fails; CI installs staticcheck explicitly.
lint: vet
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	elif command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "lint: no staticcheck/golangci-lint on PATH; ran go vet only (skipping static analysis)"; \
	fi

.DEFAULT_GOAL := build
