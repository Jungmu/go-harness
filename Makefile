SHELL := /bin/sh

GO ?= go
GOFMT ?= gofmt
BINARY ?= bin/harnessd
MAIN_PKG ?= ./cmd/harnessd
PKGS ?= ./...
FMT_DIRS ?= cmd internal

.PHONY: build test fmt test-live-e2e

build:
	mkdir -p $(dir $(BINARY))
	$(GO) build -o $(BINARY) $(MAIN_PKG)

test:
	set -a; if [ -f .env ]; then . ./.env; fi; set +a; GO_HARNESS_LIVE_E2E= $(GO) test $(PKGS)

fmt:
	$(GOFMT) -w $(FMT_DIRS)

test-live-e2e:
	set -a; if [ -f .env ]; then . ./.env; fi; set +a; GO_HARNESS_LIVE_E2E=1 $(GO) test $(MAIN_PKG) -run TestLiveLinearCodexHandsOffAtMaxTurns -v
