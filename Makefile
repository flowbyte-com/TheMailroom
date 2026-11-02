# Makefile for TheMailroom.
#
# The release build itself lives in internal/release/cmd/release, which is
# the single source of truth for the target matrix, the artifact names,
# the build metadata and the verification. This file is the discoverable
# entry point for it.
#
# The build is reproducible: the same commit produces the same artifacts,
# byte for byte, on any machine. See docs/RELEASE.md.

GO      ?= go
DIST    ?= dist
ROOT    ?= .

RELEASE := $(GO) run ./internal/release/cmd/release

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "TheMailroom build targets:"
	@echo "  make build     build the server for the host platform into $(DIST)/"
	@echo "  make release   build every release artifact and $(DIST)/SHA256SUMS"
	@echo "  make verify    verify the artifacts in $(DIST): checksums, format, metadata"
	@echo "  make smoke     clean-install smoke test of the host artifact"
	@echo "  make check     run the Go gates: build, vet, test, race, gofmt"
	@echo "  make clean     remove $(DIST)/"

.PHONY: build
build:
	$(GO) build -o $(DIST)/themailroom ./cmd/themailroom

.PHONY: release
release:
	$(RELEASE) build --root $(ROOT) --dist $(DIST)

.PHONY: verify
verify:
	$(RELEASE) verify --root $(ROOT) --dist $(DIST)

.PHONY: smoke
smoke:
	$(RELEASE) smoke --root $(ROOT) --dist $(DIST)

.PHONY: check
check:
	$(GO) mod verify
	$(GO) build ./...
	$(GO) vet ./...
	$(GO) test ./...
	$(GO) test -race ./...
	@if [ "$$(gofmt -l . | wc -l)" -ne 0 ]; then \
		echo "Files need gofmt:"; gofmt -l .; exit 1; \
	fi

.PHONY: clean
clean:
	rm -rf $(DIST)
