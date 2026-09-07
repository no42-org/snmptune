# Copyright 2026 Ronny Trommer <ronny@no42.org>
# SPDX-License-Identifier: Apache-2.0

GO ?= go
BIN := bin/snmptune

.PHONY: build test lint integration integration-deps clean

build:
	$(GO) build -o $(BIN) ./cmd/snmptune

test:
	$(GO) test -race -cover ./...

lint:
	$(GO) vet ./...
	golangci-lint run ./...

# Runs the integration tests against a net-snmp snmpd started on a high port.
# The tests skip themselves when snmpd is not on PATH.
integration:
	SNMPTUNE_INTEGRATION=1 $(GO) test -race -run Integration -count=1 ./...

# Installs net-snmp on Debian and Ubuntu runners for make integration.
integration-deps:
	sudo apt-get update && sudo apt-get install -y snmpd snmp

clean:
	rm -rf bin coverage.out testdata/run
