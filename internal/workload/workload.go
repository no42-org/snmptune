/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package workload derives what the trials query: tables with their columns
// and scalars, either from the reference inventory or from an OpenNMS
// datacollection group.
package workload

import (
	"encoding/xml"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/oid"
)

// Table is one set of columns walked together, in the OpenNMS collector's
// shape. A non-table subtree is a Table whose single column is its root.
type Table struct {
	Base    string
	Columns []string
}

// Workload is everything one trial queries.
type Workload struct {
	Tables  []Table
	Scalars []string
}

// WidestTable is the largest column count, which caps max-vars-per-pdu.
func (w Workload) WidestTable() int {
	n := 0
	for _, t := range w.Tables {
		n = max(n, len(t.Columns))
	}
	return n
}

// Roots lists the table subtrees the reference walk has to cover.
// Scalars are verified by their Get responses instead.
func (w Workload) Roots() []string {
	var roots []string
	for _, t := range w.Tables {
		roots = append(roots, t.Base)
	}
	return roots
}

// FromInventory turns walked subtrees into tables: inferred columns for
// table-shaped subtrees, a single-column walk for everything else.
func FromInventory(inv inventory.Inventory) Workload {
	var w Workload
	for _, st := range inv.Subtrees {
		t := Table{Base: st.Root}
		if st.IsTable() {
			for _, c := range st.Columns {
				t.Columns = append(t.Columns, fmt.Sprintf("%s.1.%d", st.Root, c))
			}
		} else {
			t.Columns = []string{st.Root}
		}
		w.Tables = append(w.Tables, t)
	}
	return w
}

// IsWholeTree reports whether any root is 1.3.6.1, the explicit opt-in for
// walking everything.
func IsWholeTree(roots []string) bool {
	return slices.ContainsFunc(roots, func(r string) bool { return oid.Canonical(r) == "1.3.6.1" })
}

// SplitGroupArg splits "file.xml:group" into its parts; the group is optional.
func SplitGroupArg(arg string) (file, group string) {
	if i := strings.LastIndex(arg, ":"); i > 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

// node is a generic view of the datacollection XML; group and mibObj
// elements are picked out wherever they appear.
type node struct {
	XMLName  xml.Name
	Name     string `xml:"name,attr"`
	OID      string `xml:"oid,attr"`
	Instance string `xml:"instance,attr"`
	Children []node `xml:",any"`
}

// FromDatacollection reads an OpenNMS datacollection XML file and builds the
// workload from the named group, or from every group when name is empty.
func FromDatacollection(path, name string) (Workload, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Workload{}, err
	}
	var root node
	if err := xml.Unmarshal(raw, &root); err != nil {
		return Workload{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var groups []node
	collectGroups(root, &groups)
	var names []string
	var w Workload
	tables := map[string]*Table{}
	var order []string
	for _, g := range groups {
		names = append(names, g.Name)
		if name != "" && g.Name != name {
			continue
		}
		for _, obj := range g.Children {
			if obj.XMLName.Local != "mibObj" {
				continue
			}
			o := oid.Canonical(obj.OID)
			if _, isScalar := strconv.Atoi(obj.Instance); isScalar == nil {
				if s := o + "." + obj.Instance; !slices.Contains(w.Scalars, s) {
					w.Scalars = append(w.Scalars, s)
				}
				continue
			}
			base := parentOID(parentOID(o))
			t, ok := tables[base]
			if !ok {
				t = &Table{Base: base}
				tables[base] = t
				order = append(order, base)
			}
			if !slices.Contains(t.Columns, o) {
				t.Columns = append(t.Columns, o)
			}
		}
	}
	if name != "" && !slices.Contains(names, name) {
		return Workload{}, fmt.Errorf("group %q not found in %s; available: %s", name, path, strings.Join(names, ", "))
	}
	for _, base := range order {
		w.Tables = append(w.Tables, *tables[base])
	}
	return w, nil
}

func collectGroups(n node, out *[]node) {
	if n.XMLName.Local == "group" {
		*out = append(*out, n)
		return
	}
	for _, c := range n.Children {
		collectGroups(c, out)
	}
}

func parentOID(o string) string {
	if i := strings.LastIndex(o, "."); i > 0 {
		return o[:i]
	}
	return o
}
