# Makefile for AI-HA
#
# Usage:
#   make            # build all binaries into bin/
#   make build      # same as above
#   make test       # run unit tests
#   make clean      # remove build artifacts

BINARIES    := aiha-framework aiha-agent
BIN_DIR     := bin
MODULE      := github.com/cloud-barista/ai-ha
# Binary started by "make run".
BINARY      := aiha-agent

# aiha-rescue is aiha-agent built with "-tags rescue": the same program without
# the backup engine and repository backends, small enough for the rescue
# environment to download into RAM. It is built separately because its source
# directory does not match its binary name.
RESCUE_SRC  := aiha-agent
RESCUE_BIN  := aiha-rescue

# Version injected into <binary>/cmd.version at link time.
VERSION     ?= dev

GO          ?= go
GOOS        ?= $(shell $(GO) env GOOS)
GOARCH      ?= $(shell $(GO) env GOARCH)

# All dependencies (incl. modernc.org/sqlite and pgx) are pure Go, so the
# binaries are built statically without cgo.
CGO_ENABLED ?= 0

.PHONY: all build build-rescue run test test-race fmt vet lint swag tidy clean help

all: build

## build: compile all binaries into bin/
build: build-rescue
	@mkdir -p $(BIN_DIR)
	@for b in $(BINARIES); do \
		CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
			$(GO) build -trimpath -ldflags "-s -w -X $(MODULE)/$$b/cmd.version=$(VERSION)" \
			-o $(BIN_DIR)/$$b ./$$b || exit 1; \
		echo "Built $(BIN_DIR)/$$b ($(GOOS)/$(GOARCH), version $(VERSION))"; \
	done

## build-rescue: compile the slim rescue-environment build of aiha-agent
build-rescue:
	@mkdir -p $(BIN_DIR)
	@CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -trimpath -tags rescue \
		-ldflags "-s -w -X $(MODULE)/$(RESCUE_SRC)/cmd.version=$(VERSION)" \
		-o $(BIN_DIR)/$(RESCUE_BIN) ./$(RESCUE_SRC)
	@echo "Built $(BIN_DIR)/$(RESCUE_BIN) ($(GOOS)/$(GOARCH), version $(VERSION))"

## run: build and run aiha-agent; pass arguments with ARGS="..."
run: build
	@$(BIN_DIR)/$(BINARY) $(ARGS)

## test: run unit tests (PostgreSQL tests run when AIHA_TEST_DB_DSN is set)
test:
	$(GO) test ./...

## test-race: run unit tests with the race detector (requires cgo)
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

## fmt: format all source files
fmt:
	$(GO) fmt ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## swag: regenerate the OpenAPI document from the handler annotations
swag:
	$(GO) tool swag init -g pkg/api/rest/server.go -d ./ -o pkg/api/rest/docs \
		--outputTypes json,yaml --parseDependency false

## tidy: sync go.mod / go.sum
tidy:
	$(GO) mod tidy

## clean: remove build artifacts
clean:
	$(GO) clean
	rm -rf $(BIN_DIR)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
