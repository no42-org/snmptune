# Copyright 2026 Ronny Trommer <ronny@no42.org>
# SPDX-License-Identifier: Apache-2.0

GO ?= go
BIN := bin/snmptune
# VERSION defaults to the nearest tag; the release workflow passes the tag.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# Targets for the multi-arch release build.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
ACTIONLINT_VERSION := v1.7.12

.PHONY: build test lint lint-workflows integration integration-deps release-build clean

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/snmptune

# Cross-compiles every platform into dist/ as tar.gz (zip on Windows),
# each archive holding the binary, LICENSE and README.
release-build:
	rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  name=snmptune_$(VERSION:v%=%)_$${os}_$${arch}; \
	  echo "building $$name"; \
	  mkdir -p dist/$$name; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$name/snmptune$$ext ./cmd/snmptune || exit 1; \
	  cp LICENSE README.md dist/$$name/; \
	  if [ $$os = windows ]; then (cd dist && zip -qr $$name.zip $$name); else tar -czf dist/$$name.tar.gz -C dist $$name; fi; \
	  rm -rf dist/$$name; \
	done
	ls -l dist

test:
	$(GO) test -race -cover ./...

lint:
	$(GO) vet ./...
	golangci-lint run ./...

# Lints the GitHub Actions workflows: unpinned actions, bad expressions,
# script injection sinks, invalid schema.
lint-workflows:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -color

# Runs the integration tests against a net-snmp snmpd started on a high port.
# The tests skip themselves when snmpd is not on PATH.
integration:
	SNMPTUNE_INTEGRATION=1 $(GO) test -race -run Integration -count=1 ./...

# Installs net-snmp on Debian and Ubuntu runners for make integration.
integration-deps:
	sudo apt-get update && sudo apt-get install -y snmpd snmp

clean:
	rm -rf bin coverage.out testdata/run
