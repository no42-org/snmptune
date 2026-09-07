/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package oid compares and inspects dotted object identifiers numerically.
package oid

import (
	"strconv"
	"strings"
)

// Parse splits a dotted OID into its arcs. A leading dot is ignored.
// Arcs that do not parse are treated as zero; callers pass agent output,
// which is always numeric.
func Parse(s string) []uint32 {
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ".")
	arcs := make([]uint32, len(parts))
	for i, p := range parts {
		v, _ := strconv.ParseUint(p, 10, 32)
		arcs[i] = uint32(v)
	}
	return arcs
}

// Compare orders two OIDs lexicographically by arc value.
// A proper prefix sorts before its extensions.
func Compare(a, b string) int {
	return CompareArcs(Parse(a), Parse(b))
}

// CompareArcs is Compare on already parsed arcs.
func CompareArcs(a, b []uint32) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// HasPrefix reports whether o lies at or under prefix.
func HasPrefix(o, prefix string) bool {
	a, p := Parse(o), Parse(prefix)
	if len(a) < len(p) {
		return false
	}
	return CompareArcs(a[:len(p)], p) == 0
}

// Canonical returns the OID without a leading dot.
func Canonical(s string) string {
	return strings.TrimPrefix(s, ".")
}
