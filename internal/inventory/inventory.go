/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package inventory performs the conservative reference walk and derives the
// ground truth every trial is checked against.
package inventory

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/oid"
)

// ReferenceRepetitions is the max-repetitions of the reference walk.
// It matches the net-snmp snmpbulkwalk default and is safe on any agent
// that speaks GetBulk.
const ReferenceRepetitions = 10

// Budget bounds the reference walk. Zero means unlimited. The time budget
// is the context deadline, shared with the rest of the run.
type Budget struct {
	MaxOIDs int
}

// Subtree is the inventory of one walked root.
type Subtree struct {
	Root    string
	OIDs    []string // sorted, as returned by the agent
	Bytes   int      // sum of response bytes
	Columns []uint32 // column arcs when the subtree is a table
	Partial bool
}

// Count is the number of OIDs collected.
func (s Subtree) Count() int { return len(s.OIDs) }

// IsTable reports whether every OID has the form root.1.column.index.
func (s Subtree) IsTable() bool { return len(s.Columns) > 0 }

// MeanBytesPerVarbind is the average encoded size of one varbind.
func (s Subtree) MeanBytesPerVarbind() float64 {
	if len(s.OIDs) == 0 {
		return 0
	}
	return float64(s.Bytes) / float64(len(s.OIDs))
}

// Inventory is the result of the reference walk.
type Inventory struct {
	Subtrees []Subtree
	Partial  bool
	PDUs     int
	Duration time.Duration
}

// Total is the number of OIDs across all subtrees.
func (inv Inventory) Total() int {
	n := 0
	for _, s := range inv.Subtrees {
		n += len(s.OIDs)
	}
	return n
}

var now = time.Now

// Walk runs a single-repeater GetBulk walk over every root within the budget.
// It returns a partial inventory when a budget is exhausted; only transport
// or agent misbehaviour is an error.
func Walk(ctx context.Context, tr agent.Transport, roots []string, b Budget) (Inventory, error) {
	start := now()
	inv := Inventory{}
	total := 0
	for _, root := range roots {
		root = oid.Canonical(root)
		st := Subtree{Root: root}
		cursor := root
		for {
			if b.MaxOIDs > 0 && total >= b.MaxOIDs || ctx.Err() != nil {
				st.Partial, inv.Partial = true, true
				break
			}
			resp, err := tr.GetBulk(ctx, []string{cursor}, ReferenceRepetitions)
			if err != nil {
				if ctx.Err() != nil {
					st.Partial, inv.Partial = true, true
					break
				}
				return inv, fmt.Errorf("reference walk of %s at %s: %w", root, cursor, err)
			}
			inv.PDUs++
			st.Bytes += resp.Bytes
			done := len(resp.Varbinds) == 0
			for _, vb := range resp.Varbinds {
				if _, end := vb.Value.(agent.EndOfMibView); end || !oid.HasPrefix(vb.OID, root) {
					done = true
					break
				}
				if oid.Compare(vb.OID, cursor) <= 0 {
					return inv, fmt.Errorf("reference walk of %s: agent returned %s after %s, not advancing", root, vb.OID, cursor)
				}
				st.OIDs = append(st.OIDs, vb.OID)
				cursor = vb.OID
				total++
			}
			if done {
				break
			}
		}
		st.Columns = inferColumns(root, st.OIDs)
		inv.Subtrees = append(inv.Subtrees, st)
	}
	inv.Duration = now().Sub(start)
	return inv, nil
}

// inferColumns returns the distinct column arcs when every OID under root has
// the MIB table shape root.1.column.index, and nil otherwise.
func inferColumns(root string, oids []string) []uint32 {
	if len(oids) == 0 {
		return nil
	}
	depth := len(oid.Parse(root))
	var cols []uint32
	for _, o := range oids {
		arcs := oid.Parse(o)
		if len(arcs) < depth+3 || arcs[depth] != 1 {
			return nil
		}
		if c := arcs[depth+1]; !slices.Contains(cols, c) {
			cols = append(cols, c)
		}
	}
	slices.Sort(cols)
	return cols
}
