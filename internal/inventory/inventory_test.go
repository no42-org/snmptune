/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package inventory

import (
	"context"
	"testing"

	"github.com/no42-org/snmptune/internal/agent/sim"
)

func TestWalkCollectsEverySubtreeOIDOnce(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 3, 7)
	a.Set("1.3.6.1.2.1.1.1.0", "sys")
	a.Set("1.3.6.1.2.1.3.1.1.1.1", "after the table")

	inv, err := Walk(context.Background(), a, []string{"1.3.6.1.2.1.2.2"}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	st := inv.Subtrees[0]
	if st.Count() != 21 {
		t.Fatalf("got %d oids, want 21", st.Count())
	}
	if st.OIDs[0] != "1.3.6.1.2.1.2.2.1.1.1" || st.OIDs[20] != "1.3.6.1.2.1.2.2.1.3.7" {
		t.Fatalf("unexpected bounds %s .. %s", st.OIDs[0], st.OIDs[20])
	}
	if st.Bytes == 0 || st.MeanBytesPerVarbind() <= 0 {
		t.Fatalf("bytes not accounted: %+v", st)
	}
	for _, r := range a.Requests() {
		if r.Kind != "getbulk" || len(r.OIDs) != 1 || r.MaxReps != ReferenceRepetitions {
			t.Fatalf("reference walk must be single-repeater GetBulk at R=%d, got %+v", ReferenceRepetitions, r)
		}
	}
}

func TestTableDetected(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 3, 2)
	inv, err := Walk(context.Background(), a, []string{"1.3.6.1.2.1.2.2"}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	st := inv.Subtrees[0]
	if !st.IsTable() || len(st.Columns) != 3 || st.Columns[2] != 3 {
		t.Fatalf("want table with columns 1,2,3, got %+v", st.Columns)
	}
}

func TestNotATable(t *testing.T) {
	a := sim.New()
	a.Set("1.3.6.1.2.1.1.1.0", "descr")
	a.Set("1.3.6.1.2.1.1.3.0", "uptime")
	inv, err := Walk(context.Background(), a, []string{"1.3.6.1.2.1.1"}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Subtrees[0].IsTable() {
		t.Fatal("system group must not be classified as a table")
	}
}

func TestOIDBudgetMarksPartial(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 100)
	inv, err := Walk(context.Background(), a, []string{"1.3.6.1.2.1.2.2"}, Budget{MaxOIDs: 25})
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Partial || inv.Subtrees[0].Count() > 30 {
		t.Fatalf("want partial inventory around 25 oids, got partial=%v count=%d", inv.Partial, inv.Subtrees[0].Count())
	}
}

func TestEmptySubtreeIsRecordedEmpty(t *testing.T) {
	a := sim.New()
	a.Set("1.3.6.1.2.1.1.1.0", "descr")
	inv, err := Walk(context.Background(), a, []string{"1.3.6.1.2.1.99"}, Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Subtrees[0].Count() != 0 {
		t.Fatalf("want empty subtree, got %d", inv.Subtrees[0].Count())
	}
}

func TestCancelledContextGivesPartialInventory(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inv, err := Walk(ctx, a, []string{"1.3.6.1.2.1.2.2"}, Budget{})
	if err != nil {
		t.Fatalf("deadline or cancel is a budget, not an error: %v", err)
	}
	if !inv.Partial {
		t.Fatal("want partial inventory")
	}
}

func TestMisorderedColumnIsSkippedAndRecorded(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 2, 3)
	a.Misorder = map[string]string{"1.3.6.1.2.1.2.2.1.1.2": "1.3.6.1.2.1.2.2.1.1.1"}
	inv, err := Walk(context.Background(), a, []string{"1.3.6.1.2.1.2.2"}, Budget{})
	if err != nil {
		t.Fatalf("misordering must not be an error: %v", err)
	}
	st := inv.Subtrees[0]
	want := []string{"1.3.6.1.2.1.2.2.1.1.1", "1.3.6.1.2.1.2.2.1.1.2", "1.3.6.1.2.1.2.2.1.2.1", "1.3.6.1.2.1.2.2.1.2.2", "1.3.6.1.2.1.2.2.1.2.3"}
	if len(st.OIDs) != len(want) {
		t.Fatalf("want %v, got %v", want, st.OIDs)
	}
	for i := range want {
		if st.OIDs[i] != want[i] {
			t.Fatalf("want %v, got %v", want, st.OIDs)
		}
	}
	if len(st.Skipped) != 1 || st.Skipped[0] != "1.3.6.1.2.1.2.2.1.1" {
		t.Fatalf("want skipped column 1, got %v", st.Skipped)
	}
}
