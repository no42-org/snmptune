/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package oid

import "testing"

func TestCompareIsNumericNotLexical(t *testing.T) {
	// ".1.3.6.1.2.1.2.2.1.10.9" sorts before ".1.3.6.1.2.1.2.2.1.10.10" numerically,
	// but a string compare would put "10" before "9".
	if Compare("1.3.6.1.2.1.2.2.1.10.9", "1.3.6.1.2.1.2.2.1.10.10") >= 0 {
		t.Fatal("expected .9 < .10")
	}
	if Compare("1.3.6.1", "1.3.6.1") != 0 {
		t.Fatal("expected equal")
	}
	if Compare("1.3.6.1", "1.3.6.1.2") >= 0 {
		t.Fatal("expected prefix to sort first")
	}
}

func TestCompareAcceptsLeadingDot(t *testing.T) {
	if Compare(".1.3.6", "1.3.6") != 0 {
		t.Fatal("leading dot must not matter")
	}
}

func TestHasPrefix(t *testing.T) {
	if !HasPrefix("1.3.6.1.2.1.2.2.1.10.9", "1.3.6.1.2.1.2.2") {
		t.Fatal("expected prefix match")
	}
	if HasPrefix("1.3.6.1.2.1.22", "1.3.6.1.2.1.2") {
		t.Fatal("arc 22 must not match prefix arc 2")
	}
	if !HasPrefix("1.3.6.1", "1.3.6.1") {
		t.Fatal("an oid is its own prefix")
	}
}
