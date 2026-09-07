# Contributing

Thanks for helping with snmptune. This page covers the mechanics; design discussions happen in issues.

## Start from an issue

Bugs and enhancements are tracked as GitHub issues. Open one before a pull request so the change has a place to be discussed, and reference it from the pull request with `Closes #<number>`.

## Building and testing

    make build        # bin/snmptune
    make test         # unit tests against the simulated agent, race detector on
    make lint         # go vet and golangci-lint
    make integration  # needs net-snmp snmpd on PATH; skips otherwise
    make lint-workflows

Run one test with `go test ./internal/search/ -run TestName`. Tests are written first; every behaviour change comes with a test that failed before the change.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `ci:`, `chore:` and so on, with `!` or a `BREAKING CHANGE:` footer for breaking changes. Keep each commit to one change.

## Developer Certificate of Origin

All commits must be signed off (`git commit -s`), certifying the [DCO](https://developercertificate.org/). The `Signed-off-by` trailer must name a human identity, the person responsible for the contribution.

## AI-assisted contributions

AI assistance is welcome. Commits produced with an AI agent additionally carry an `Assisted-by: <Agent>:<model>` trailer, for example `Assisted-by: ClaudeCode:claude-fable-5-1`. The human signer reviews all AI-generated code and remains responsible for its correctness and license compliance.

## Source headers

Every source file starts with the SPDX header:

    /*
     * Copyright 2026 Ronny Trommer <ronny@no42.org>
     * SPDX-License-Identifier: Apache-2.0
     */

## The one rule that is not negotiable

The walker sends one PDU at a time. snmptune is a load generator pointed at production gear, and the agent setting the pace is what keeps it safe. Do not add concurrency toward the target.
