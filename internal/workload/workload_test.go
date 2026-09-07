/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package workload

import (
	"strings"
	"testing"

	"github.com/no42-org/snmptune/internal/inventory"
)

func TestFromInventoryTableGetsInferredColumns(t *testing.T) {
	inv := inventory.Inventory{Subtrees: []inventory.Subtree{
		{Root: "1.3.6.1.2.1.2.2", Columns: []uint32{1, 2, 10}, OIDs: []string{"1.3.6.1.2.1.2.2.1.1.1"}},
		{Root: "1.3.6.1.2.1.1", OIDs: []string{"1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.3.0"}},
	}}
	w := FromInventory(inv)
	if len(w.Tables) != 2 {
		t.Fatalf("want 2 tables, got %+v", w.Tables)
	}
	if got := w.Tables[0].Columns; len(got) != 3 || got[2] != "1.3.6.1.2.1.2.2.1.10" {
		t.Fatalf("want column oids, got %v", got)
	}
	if got := w.Tables[1].Columns; len(got) != 1 || got[0] != "1.3.6.1.2.1.1" {
		t.Fatalf("non-table must be a single-column walk of its root, got %v", got)
	}
}

func TestDatacollectionGroupsColumnsIntoTables(t *testing.T) {
	w, err := FromDatacollection("testdata/mib2.xml", "mib2-interfaces")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Tables) != 2 || len(w.Scalars) != 0 {
		t.Fatalf("want ifXTable and ifTable, got %+v", w)
	}
	ifx := w.Tables[0]
	if ifx.Base != "1.3.6.1.2.1.31.1.1" || len(ifx.Columns) != 2 || ifx.Columns[1] != "1.3.6.1.2.1.31.1.1.1.10" {
		t.Fatalf("unexpected ifXTable %+v", ifx)
	}
	if w.Tables[1].Base != "1.3.6.1.2.1.2.2" {
		t.Fatalf("unexpected second table %+v", w.Tables[1])
	}
}

func TestDatacollectionScalars(t *testing.T) {
	w, err := FromDatacollection("testdata/mib2.xml", "mib2-tcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Tables) != 0 || len(w.Scalars) != 2 || w.Scalars[0] != "1.3.6.1.2.1.6.5.0" {
		t.Fatalf("want two scalar instance oids, got %+v", w)
	}
}

func TestDatacollectionAllGroupsWhenNameEmpty(t *testing.T) {
	w, err := FromDatacollection("testdata/mib2.xml", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Tables) != 3 || len(w.Scalars) != 2 {
		t.Fatalf("want every group, got %d tables %d scalars", len(w.Tables), len(w.Scalars))
	}
}

func TestDatacollectionUnknownGroupListsAvailable(t *testing.T) {
	_, err := FromDatacollection("testdata/mib2.xml", "nope")
	if err == nil || !strings.Contains(err.Error(), "mib2-interfaces") || !strings.Contains(err.Error(), "mib2-tcp") {
		t.Fatalf("want error listing group names, got %v", err)
	}
}

func TestSplitGroupArg(t *testing.T) {
	f, g := SplitGroupArg("/etc/opennms/datacollection/mib2.xml:mib2-interfaces")
	if f != "/etc/opennms/datacollection/mib2.xml" || g != "mib2-interfaces" {
		t.Fatalf("got %q %q", f, g)
	}
	f, g = SplitGroupArg("mib2.xml")
	if f != "mib2.xml" || g != "" {
		t.Fatalf("got %q %q", f, g)
	}
}

func TestWholeTree(t *testing.T) {
	if !IsWholeTree([]string{"1.3.6.1.2.1.2.2", ".1.3.6.1"}) {
		t.Fatal("1.3.6.1 is whole-tree opt-in")
	}
	if IsWholeTree([]string{"1.3.6.1.2.1"}) {
		t.Fatal("mib-2 is not the whole tree")
	}
}

func TestWidestTable(t *testing.T) {
	w := Workload{Tables: []Table{{Columns: []string{"a"}}, {Columns: []string{"a", "b", "c"}}}}
	if w.WidestTable() != 3 {
		t.Fatalf("got %d", w.WidestTable())
	}
}

func TestDuplicateObjectsAcrossGroupsAreMergedOnce(t *testing.T) {
	w, err := FromDatacollection("testdata/dup.xml", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Tables) != 1 || len(w.Tables[0].Columns) != 1 || len(w.Scalars) != 1 {
		t.Fatalf("want one column and one scalar, got %+v", w)
	}
}
