# snmptune

Go CLI that measures one SNMP v2c agent and recommends OpenNMS `snmp-config.xml` values.

## Conventions

- Licence is Apache-2.0. Every source file starts with the SPDX header (`Copyright 2026 Ronny Trommer <ronny@no42.org>` and `SPDX-License-Identifier: Apache-2.0`).
- CI calls Makefile targets only: `make build`, `make lint`, `make test`, `make integration`.
- Tests first. Search, walker and report logic is unit-tested against the simulated agent in `internal/agent/sim`. Integration tests need `snmpd` on `PATH` and skip otherwise.
- The walker is strictly sequential toward the agent. Do not add concurrency toward the target.
