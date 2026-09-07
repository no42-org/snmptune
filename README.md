# snmptune

Measures one SNMP v2c agent and recommends the `max-repetitions`, `max-vars-per-pdu`, `timeout` and `retry` values for OpenNMS `snmp-config.xml`.

OpenNMS ships conservative defaults (max-repetitions 2, max-vars-per-pdu 10) and operators tune them by guesswork.
Too low wastes collection time on large tables.
Too high overruns the agent's message size, fragments on the wire, or trips control-plane policing, and the failures look like random timeouts.
snmptune finds the largest setting a given agent sustains without errors and recommends one notch below it.

## What it measures

The two knobs are not symmetric.
A plain `snmpbulkwalk` sends one repeater per PDU and never exercises `max-vars-per-pdu`.
The OpenNMS collector packs up to `max-vars-per-pdu` table columns as repeaters into one GetBulk with `max-repetitions` rows each.
snmptune builds requests in that shape and records, per PDU, the round-trip time, response bytes, rows requested versus returned, and retries.

A run has four phases:

1. **Preflight.**
   Five sysUpTime probes give a canary baseline.
   No answer means no run.
2. **Reference walk.**
   A single-repeater GetBulk walk at max-repetitions 10 over the workload subtrees.
   It yields the OID set every trial is checked against and the table structure.
   Subtrees the walk could not finish within the budget are dropped from the trials with a warning.
3. **Search.**
   Start at the OpenNMS default, double `max-repetitions` until throughput plateaus or a trial fails, bisect, then try other `max-vars-per-pdu` values at the same product.
   Each trial is repeated and scored by its median varbinds per second of agent time.
4. **Report.**
   The trial table, the observed peak, and a recommendation one error-free notch below it.
   The timeout is three times the p99 round-trip time at the recommended setting, rounded up to 100 ms, never below 500 ms.

A trial fails on any timeout, on an agent error status such as tooBig, on a truncated response, or when more than one percent of the reference OIDs are missing.
Some agents sort string-indexed rows as text and answer a walk with an OID smaller than the one requested, which net-snmp reports as "OID not increasing".
snmptune skips the rest of that column, continues with the next one, warns on stderr and lists the skipped columns in the report.
A response is truncated when the agent returned fewer rows than requested before every repeater reached the end of the MIB view.

## Safety

snmptune is a load generator pointed at gear people depend on, so the defaults are meant to be safe on production devices.

- One PDU in flight, always.
  The agent sets the pace.
  There is no flag for concurrency.
- A canary probe runs before every repeat and once after the last trial.
  If it gets much slower than the baseline the search stops escalating.
  If sysUpTime goes backwards the run aborts and the report names the setting that preceded the restart.
  Two lost canaries in a row abort the run.
- The search never tries a product larger than one that already failed.
- Budgets end the run with the best result so far: 15 minutes, 40 trials, 1000 varbinds per PDU, 200 000 reference OIDs.
  The duration budget is a deadline on every request, so no phase can overrun it.
- Cooldown between runs is at least two seconds and never shorter than the run that just finished.
  Ctrl-C ends the cooldown and the run immediately, and the report says it was interrupted.
- Only GetBulk, GetNext and Get are ever sent.
- `--dry-run` prints the plan and sends nothing.

Where you run it from matters.
Run from the OpenNMS server or Minion so the timeout reflects the real path, but pick a quiet window.
If collectd is polling the same agent, the agent sees both loads and the numbers are skewed.

## Usage

```
snmptune --target 192.0.2.1 --community public --oid 1.3.6.1.2.1.2.2 --oid 1.3.6.1.2.1.31.1.1
snmptune --target 192.0.2.1 --group /opt/opennms/etc/datacollection/mib2.xml:mib2-interfaces
snmptune --target 192.0.2.1 --oid 1.3.6.1.2.1.2.2 --format opennms
snmptune --target 192.0.2.1 --oid 1.3.6.1.2.1.2.2 --json --pdu-log pdus.jsonl
snmptune --target 192.0.2.1 --oid 1.3.6.1.2.1.2.2 --dry-run
```

`--group` reads an OpenNMS datacollection XML file.
Objects with a non-numeric instance become table columns, numeric instances become scalars, so the trial workload matches what collectd sends.
Without `:name` every group in the file is used.

`--oid 1.3.6.1` walks the whole tree. It prints a warning; the budgets still apply.

`--try R:V` skips the search and measures the named settings, each with the usual repeats, canary and cooldown.
Use it to verify a value before deploying it or to compare two candidates side by side:

```
snmptune --target 192.0.2.1 --oid 1.3.6.1.2.1.31.1.1 --try 4:10 --try 2:20
```

In try mode the recommendation is the best of the requested settings, without the headroom step.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--target`, `--port`, `--community` | 161, public | the agent |
| `--timeout`, `--retries` | 3s, 1 | per request while measuring |
| `--oid` | | subtree to query, repeatable |
| `--group` | | datacollection XML file, optionally `:group` |
| `--reference-tree` | | extra subtree for the inventory report only |
| `--try` | | measure this `R:V` instead of searching, repeatable |
| `--repeats` | 3 | repeats per trial |
| `--cooldown` | 2s | minimum pause between runs |
| `--tolerance` | 1 | percent of reference OIDs a trial may miss |
| `--max-duration`, `--max-trials`, `--max-product`, `--max-oids` | 15m, 40, 1000, 200000 | budgets |
| `--dry-run` | | print the plan, send nothing |
| `--json` | | JSON report on stdout |
| `--format opennms` | | `snmp-config.xml` definition on stdout |
| `--pdu-log` | | append per-PDU telemetry as JSON lines |

Exit codes: 0 with a recommendation, 1 when aborted, unreachable or without a clean trial, 2 usage error.

## Example output

```
Target 192.0.2.1

Reference walk: 1848 OIDs in 2 subtrees, 187 PDUs, 1.2s
  1.3.6.1.2.1.2.2: 1056 OIDs, 31 B/varbind (table, 22 columns)
  1.3.6.1.2.1.31.1.1: 792 OIDs, 27 B/varbind (table, 18 columns)

Trials
  phase     R   V   varbinds/s  p50 ms  p99 ms  result
  escalate  2   10  1830        9.8     14.2    ok
  escalate  4   10  3420        11.0    16.9    ok
  escalate  8   10  6010        13.4    21.0    ok
  escalate  16  10  9870        17.9    30.4    ok
  escalate  32  10  -           -       -       FAILED: 6 truncated responses
  bisect    24  10  12400       22.1    38.7    ok
  bisect    28  10  -           -       -       FAILED: 2 truncated responses
  bisect    26  10  -           -       -       FAILED: 1 truncated responses
  split     48  5   11900       20.3    36.0    ok
  split     12  20  12100       21.6    39.9    ok

Recommendation
  max-repetitions  16
  max-vars-per-pdu 10
  timeout          500 ms  (3 x p99 30.4 ms, rounded up, floor 500 ms)
  retry            1
  observed peak    R=24 V=10
  why              one notch of headroom below the observed peak R=24 V=10 (12400 varbinds/s); production shares the agent with other pollers
```

With `--format opennms`:

```xml
<definition version="v2c" max-repetitions="16" max-vars-per-pdu="10" timeout="500" retry="1">
    <specific>192.0.2.1</specific>
</definition>
```

## Development

```
make build        # bin/snmptune
make test         # unit tests against the simulated agent
make lint         # go vet and golangci-lint
make integration  # starts net-snmp snmpd on a high port, runs the integration tests
```

The search, walker and report are tested against `internal/agent/sim`, an in-memory agent that can truncate responses, simulate round-trip time and restart itself.
The integration tests need `snmpd` on `PATH` and skip otherwise.
`testdata/snmpd.conf` includes a deliberately slow subtree served by a `pass` script.
`make integration-deps` installs net-snmp on Debian and Ubuntu.

Not yet covered: SNMPv3 and fleet mode across many devices.

## License

Apache-2.0. See `LICENSE`.
