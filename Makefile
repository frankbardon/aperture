.PHONY: build run clean test bench fmt vet lint proto

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

# bench runs the performance benchmark suite in ./bench (INFORMATIONAL): it
# prints ns/op, allocs/op, and the computed p99 (p99-ns) + sustained throughput
# (checks/sec) for a cached Check on a sizable seeded model, with decision audit
# both ON and OFF, plus the bounded Enumerate benchmark.
#
# The HARD NFR assertion (p99 cached Check < 1ms AND >= 10k checks/sec/instance)
# is the gated test TestCheckNFR, kept out of the default `make test` so a
# loaded CI machine never flakes the build. Run it explicitly:
#
#   APERTURE_BENCH_ASSERT=1 $(GO) test -run TestCheckNFR ./bench/
#
# See docs/benchmarks.md for the methodology and the latest committed numbers.
bench:
	$(GO) test -run '^$$' -bench=. -benchmem ./bench/

fmt:
	$(GO) fmt ./...

# proto regenerates the Twirp service + protobuf messages from the .proto. The
# generated *.pb.go / *.twirp.go are COMMITTED (CI does not regenerate); this
# target mirrors orbit's. Requires protoc + protoc-gen-go + protoc-gen-twirp on
# PATH (paths=source_relative keeps the output beside the .proto).
proto:
	protoc -I=./internal/wire/rpc \
	  --go_out=./internal/wire/rpc --go_opt=paths=source_relative \
	  --twirp_out=./internal/wire/rpc --twirp_opt=paths=source_relative \
	  ./internal/wire/rpc/service.proto

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
