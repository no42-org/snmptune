/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package agent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// unusedPort reserves a UDP port that nothing answers on.
func unusedPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestGoSNMPTimeoutAfterRetries(t *testing.T) {
	tr, err := NewGoSNMP(Config{
		Target: "127.0.0.1", Port: unusedPort(t), Community: "public",
		Timeout: 50 * time.Millisecond, Retries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	start := time.Now()
	_, err = tr.Get(context.Background(), []string{SysUpTimeOID})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if el := time.Since(start); el < 90*time.Millisecond || el > 2*time.Second {
		t.Fatalf("timeout with one retry should take about 100ms, took %v", el)
	}
}
