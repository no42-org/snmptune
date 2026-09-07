/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package walk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/agent/sim"
	"github.com/no42-org/snmptune/internal/workload"
)

func cols(base string, n ...int) []string {
	var out []string
	for _, c := range n {
		out = append(out, fmt.Sprintf("%s.1.%d", base, c))
	}
	return out
}

func TestTableWiderThanVIsChunked(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 12, 3)
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 5, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := a.Requests()
	if len(reqs) != 2 || len(reqs[0].OIDs) != 10 || len(reqs[1].OIDs) != 2 {
		t.Fatalf("want one PDU with 10 repeaters and one with 2, got %+v", reqs)
	}
	if res.Varbinds != 36 || len(res.OIDs) != 36 {
		t.Fatalf("want 36 in-table varbinds, got %d / %d", res.Varbinds, len(res.OIDs))
	}
	if res.Failure != "" {
		t.Fatalf("unexpected failure %q", res.Failure)
	}
}

func TestColumnFinishingEarlyIsDropped(t *testing.T) {
	a := sim.New()
	base := "1.3.6.1.2.1.2.2"
	for r := 1; r <= 6; r++ {
		a.Set(fmt.Sprintf("%s.1.1.%d", base, r), r)
	}
	a.Set(base+".1.2.1", "x")
	a.Set(base+".1.2.2", "y")
	w := workload.Workload{Tables: []workload.Table{{Base: base, Columns: cols(base, 1, 2)}}}
	_, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := a.Requests()
	// PDU 1: rows 1-2 of both columns. PDU 2: column 2 leaves its subtree,
	// column 1 continues alone from then on.
	if len(reqs) < 3 || len(reqs[0].OIDs) != 2 || len(reqs[1].OIDs) != 2 || len(reqs[2].OIDs) != 1 {
		t.Fatalf("finished column must drop out: %+v", reqs)
	}
}

func TestTruncationVersusEndOfTable(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 100)
	a.MaxRows = 12
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 50, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := res.PDUs[0]
	if !first.Truncated || first.Requested != 50 || first.Rows != 12 {
		t.Fatalf("first PDU must be truncated 50 -> 12, got %+v", first)
	}
	last := res.PDUs[len(res.PDUs)-1]
	if last.Truncated {
		t.Fatalf("end-of-table PDU must not count as truncated: %+v", last)
	}
	if res.Truncations == 0 || !strings.Contains(res.Failure, "truncat") {
		t.Fatalf("truncation must fail the trial: %+v", res.Failure)
	}
	if len(res.OIDs) != 100 {
		t.Fatalf("walk must still complete, got %d oids", len(res.OIDs))
	}
}

func TestLoopingAgentAbortsColumn(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 10)
	a.LoopOID = "1.3.6.1.2.1.2.2.1.1.4"
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Failure, "not advancing") || len(a.Requests()) > 4 {
		t.Fatalf("looping agent must abort the column: failure=%q requests=%d", res.Failure, len(a.Requests()))
	}
}

func TestScalarsPackedByV(t *testing.T) {
	a := sim.New()
	a.Set("1.3.6.1.2.1.6.5.0", 1)
	a.Set("1.3.6.1.2.1.6.6.0", 2)
	a.Set("1.3.6.1.2.1.6.7.0", 3)
	w := workload.Workload{Scalars: []string{"1.3.6.1.2.1.6.5.0", "1.3.6.1.2.1.6.6.0", "1.3.6.1.2.1.6.7.0"}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 2, MaxVarsPerPDU: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reqs := a.Requests()
	if len(reqs) != 2 || reqs[0].Kind != "get" || len(reqs[0].OIDs) != 2 || len(reqs[1].OIDs) != 1 {
		t.Fatalf("want two Get PDUs of 2 and 1, got %+v", reqs)
	}
	if res.Varbinds != 3 {
		t.Fatalf("want 3 scalar varbinds, got %d", res.Varbinds)
	}
}

func TestTimeoutFailsTrialAndStops(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 100)
	a.Timeout = 10 * time.Millisecond
	a.RTT = func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 20, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Timeouts != 1 || !strings.Contains(res.Failure, "timeout") || len(a.Requests()) != 1 {
		t.Fatalf("first timeout must fail and stop the trial: %+v", res)
	}
}

func TestMissingFraction(t *testing.T) {
	ref := []string{"a", "b", "c", "d"}
	if m := Missing(ref, []string{"a", "b", "c", "d", "extra"}); m != 0 {
		t.Fatalf("extras must be ignored, got %d missing", m)
	}
	if m := Missing(ref, []string{"a", "d"}); m != 2 {
		t.Fatalf("want 2 missing, got %d", m)
	}
}

func TestSequentialAndTelemetry(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 2, 5)
	a.RTT = func(int) time.Duration { return 3 * time.Millisecond }
	var buf bytes.Buffer
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1, 2)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, NewJSONLines(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PDUs) != len(a.Requests()) {
		t.Fatalf("every request must produce one telemetry record")
	}
	for i := 1; i < len(res.PDUs); i++ {
		if res.PDUs[i].Sent.Before(res.PDUs[i-1].Sent) {
			t.Fatal("PDUs must be sent in order")
		}
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(res.PDUs) {
		t.Fatalf("want %d log lines, got %d", len(res.PDUs), len(lines))
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"seq", "sent", "rtt_ms", "bytes", "requested", "rows", "retries", "error"} {
		if _, ok := rec[k]; !ok {
			t.Fatalf("log line missing %q: %s", k, lines[0])
		}
	}
	if res.PDUs[0].RTT != 3*time.Millisecond || res.PDUs[0].Bytes == 0 {
		t.Fatalf("telemetry not carried: %+v", res.PDUs[0])
	}
}

func TestLogLineCarriesRowsAndTruncation(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 100)
	a.MaxRows = 12
	var buf bytes.Buffer
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	if _, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 50, MaxVarsPerPDU: 10}, NewJSONLines(&buf)); err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Rows      int  `json:"rows"`
		Truncated bool `json:"truncated"`
	}
	first := strings.SplitN(buf.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Rows != 12 || !rec.Truncated {
		t.Fatalf("log line must be written after rows are known: %s", first)
	}
}

func TestThroughputUsesRoundTripTime(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 10)
	a.RTT = func(int) time.Duration { return 100 * time.Millisecond }
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 5, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 10 rows at R=5: rows 1-5, rows 6-10, then one PDU that leaves the table = 3 PDUs of 100ms.
	if got := res.VarbindsPerSecond(); got < 33 || got > 34 {
		t.Fatalf("want 10 varbinds / 0.3s of agent time = 33.3/s, got %.1f", got)
	}
}

func TestAgentErrorFailsTrialInsteadOfAborting(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 50)
	a.ErrorAbove = 10
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 20, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatalf("agent error must not be a transport error: %v", err)
	}
	if res.AgentErrors != 1 || !strings.Contains(res.Failure, "agent error") {
		t.Fatalf("want failed trial with agent error, got %+v", res)
	}
}

func TestTruncationDetectedEvenWhenAColumnEnds(t *testing.T) {
	a := sim.New()
	base := "1.3.6.1.2.1.2.2"
	for r := 1; r <= 20; r++ {
		a.Set(fmt.Sprintf("%s.1.1.%d", base, r), r)
	}
	for r := 1; r <= 3; r++ {
		a.Set(fmt.Sprintf("%s.1.2.%d", base, r), r)
	}
	a.Set("1.3.6.1.2.1.3.1.1.1.1", "after") // so the walk past column 2 sees real OIDs, not end of MIB
	a.MaxRows = 12
	w := workload.Workload{Tables: []workload.Table{{Base: base, Columns: cols(base, 1, 2)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 50, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncations == 0 {
		t.Fatalf("12 rows for R=50 is a cap even though column 2 ended: %+v", res.PDUs[0])
	}
}

func TestPartialTrailingRowIsCounted(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 3, 4)
	a.MaxBytes = 50 // two varbinds fit: fewer than the three repeaters
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1, 2, 3)}}}
	res, err := Run(context.Background(), a, w, Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OIDs) != 12 {
		t.Fatalf("walk must still collect every OID, got %d; failure %q", len(res.OIDs), res.Failure)
	}
	if strings.Contains(res.Failure, "empty") {
		t.Fatalf("partial row is truncation, not an empty response: %q", res.Failure)
	}
}

func TestCancelledContextStopsWalk(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: cols("1.3.6.1.2.1.2.2", 1)}}}
	_, err := Run(ctx, a, w, Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled surfaced, got %v", err)
	}
}
