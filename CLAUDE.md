# snmptune

Go CLI that measures one SNMP v2c agent and recommends OpenNMS `snmp-config.xml` values.

## Commands

- `make build`, `make test`, `make lint`, `make lint-workflows`, `make integration`, `make release-build`. CI runs only these.
- One test: `go test ./internal/search/ -run TestName`. Add `-race` to match CI.
- `make integration` needs `snmpd` on `PATH`; the tests start it themselves on a high port and skip otherwise.

## Architecture

`cmd/snmptune` parses flags and wires the run: canary baseline, reference walk, workload, search or `--try`, report.
`internal/agent` is the `Transport` interface with the gosnmp implementation and `sim`, an in-memory agent used by every unit test.
`internal/inventory` does the conservative single-repeater reference walk and infers table columns.
`internal/workload` derives tables and scalars from `--oid` subtrees or an OpenNMS datacollection XML.
`internal/walk` queries in the OpenNMS collector's shape (multi-repeater GetBulk per table, Get for scalars) and records per-PDU telemetry.
`internal/search` escalates, bisects and splits; `internal/safety` holds canary, budgets and cooldown; `internal/report` scores and renders.

## Conventions and gotchas

- Licence is Apache-2.0; every source file starts with the SPDX header.
- Tests first. Search, walker and report logic is tested against `internal/agent/sim`, never against a live agent.
- The walker is strictly sequential toward the agent. Do not add concurrency toward the target.
- Throughput is varbinds per second of agent time (sum of PDU round-trip times), not wall clock.
- Truncation means fewer rows than requested before every repeater reached endOfMibView. A column that ended in the same PDU does not excuse it.
- A non-increasing answer means a misordered agent: skip to the next sibling of the shared prefix, record it, never abort.
- Go struct fields are gofmt-aligned; scripted edits must match on regex, not literal whitespace.
- Commits: Conventional Commits, `git commit -s`, `Assisted-by: ClaudeCode:<model>` trailer before `Signed-off-by`.
