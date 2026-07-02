.PHONY: build run clean test bench fmt vet lint proto vendor-rete docs docs-serve docs-clean docs-gen

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

# vendor-rete regenerates the committed Rete.js bundle at
# internal/server/static/vendor/rete/rete.min.js. This is the ONLY target that
# invokes node, and it is a MANUAL, OCCASIONAL step (version bumps / plugin
# changes) — it is deliberately NOT a dependency of build/test/CI. The normal
# build ships the committed blob and never runs node. All npm work happens in a
# throwaway temp dir (no node_modules/package-lock in the repo). See
# build/rete/build.sh + internal/server/static/vendor/rete/README.md.
vendor-rete:
	build/rete/build.sh

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

# docs builds the mdBook documentation site from docs/src into docs/book
# (gitignored output). Mermaid renders client-side via the vendored additional-js
# files (docs/mermaid.min.js + mermaid-init.js) — there are no preprocessor
# plugins. Requires mdbook on PATH.
docs:
	mdbook build docs

# docs-serve serves the book locally with live reload and opens a browser.
docs-serve:
	mdbook serve docs --open

# docs-clean removes the built book output.
docs-clean:
	rm -rf docs/book

# docs-gen regenerates the committed generated reference pages under docs/src
# from the Go source (on demand — there is no CI drift gate). Today it emits the
# error-code table from errors.Registry; later stories extend it with more
# generated pages. The output is committed; run this after changing a generated
# source (e.g. the error Registry in errors/codes.go).
docs-gen:
	$(GO) run ./internal/docsgen/errcodes -o docs/src/reference/error-codes.md
	$(GO) run ./internal/docsgen/cliref -o docs/src/reference/cli.md

.DEFAULT_GOAL := build
