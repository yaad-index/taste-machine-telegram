# One-stop verification aliases. Run `make check` before pushing; CI runs the
# same target. `make install-hooks` wires githook-check (fmt, vet, lint, tidy;
# no build or test) in as a pre-commit hook. Opt-in: it changes git config.
#
# Every dev tool is tracked in tools/go.mod and run through `go tool`, so
# nothing has to be installed and nothing lands in the module's own go.mod.

TOOL         := go tool -modfile=$(CURDIR)/tools/go.mod
LOCAL_PREFIX := github.com/yaad-index/taste-machine-telegram
HOOK_PATH    := .githooks

# The service binary, cross-compiled by `make dist` for each platform below.
# It runs in a container, so only Linux is built.
BINARY       := taste-machine-telegram
PLATFORMS    := linux/amd64 linux/arm64
VERSION      ?= dev
DIST         := dist

.PHONY: help fmt fmt-check lint vet test build tidy-check githook-check check dist install-hooks

help:
	@echo "Targets:"
	@echo "  fmt            format Go files and tidy every module in place (gofumpt + goimports + go mod tidy)"
	@echo "  fmt-check      verify formatting without changes (fails if dirty)"
	@echo "  lint           run golangci-lint on all packages"
	@echo "  vet            go vet ./..."
	@echo "  test           go test -race -timeout 2m ./..."
	@echo "  build          go build ./..."
	@echo "  tidy-check     verify go.mod / go.sum are tidy in every module (go mod tidy -diff)"
	@echo "  githook-check  fmt-check + vet + lint + tidy-check (what the pre-commit hook runs)"
	@echo "  check          full CI chain: vet + build + test + fmt-check + lint + tidy-check"
	@echo "  dist           cross-compile the service for every release platform into $(DIST)/ (VERSION=vX.Y.Z)"
	@echo "  install-hooks  generate .githooks/pre-commit and point git core.hooksPath at it"

fmt:
	$(TOOL) gofumpt -w .
	$(TOOL) goimports -w -local $(LOCAL_PREFIX) .
	go mod tidy
	cd tools && go mod tidy

fmt-check:
	@out=$$($(TOOL) gofumpt -l .); \
	if [ -n "$$out" ]; then \
		echo "gofumpt: the following files need formatting:"; \
		echo "$$out"; \
		exit 1; \
	fi
	@out=$$($(TOOL) goimports -l -local $(LOCAL_PREFIX) .); \
	if [ -n "$$out" ]; then \
		echo "goimports: the following files need import grouping:"; \
		echo "$$out"; \
		exit 1; \
	fi

lint:
	$(TOOL) golangci-lint run ./...

vet:
	go vet ./...

test:
	go test -race -timeout 2m ./...

build:
	go build ./...

tidy-check:
	go mod tidy -diff
	cd tools && go mod tidy -diff

githook-check: fmt-check vet lint tidy-check

check: vet build test fmt-check lint tidy-check

# Static, reproducible binaries: no cgo, no local paths, no VCS stamp, so the
# same tag builds the same bytes anywhere.
dist:
	rm -rf $(DIST)
	mkdir -p $(DIST)
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -buildvcs=false \
			-ldflags "-s -w -X main.version=$(VERSION)" \
			-o $(DIST)/$(BINARY)_$(VERSION)_$${os}_$${arch} ./cmd/$(BINARY) || exit 1; \
	done
	cd $(DIST) && sha256sum * > checksums.txt

install-hooks:
	@mkdir -p $(HOOK_PATH)
	@printf '#!/bin/sh\nexec make githook-check\n' > $(HOOK_PATH)/pre-commit
	@chmod +x $(HOOK_PATH)/pre-commit
	@git config core.hooksPath $(HOOK_PATH)
	@echo "installed $(HOOK_PATH)/pre-commit; git core.hooksPath -> $(HOOK_PATH)"
