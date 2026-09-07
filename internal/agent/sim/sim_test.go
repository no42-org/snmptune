/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package sim

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
)

func newTable(t *testing.T, rows, cols int) *Agent {
	t.Helper()
	a := New()
	a.AddTable("1.3.6.1.2.1.2.2", cols, rows)
	return a
}

func TestGetNextReturnsNumericallyNextOID(t *testing.T) {
	a := New()
	a.Set("1.3.6.1.2.1.1.1.0", "sys")
	a.Set("1.3.6.1.2.1.1.10.0", "ten")
	a.Set("1.3.6.1.2.1.1.9.0", "nine")

	r, err := a.GetNext(context.Background(), []string{"1.3.6.1.2.1.1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Varbinds[0].OID; got != "1.3.6.1.2.1.1.9.0" {
		t.Fatalf("got %s, want .9.0 before .10.0", got)
	}
}

func TestGetNextPastEndReturnsEndOfMibView(t *testing.T) {
	a := New()
	a.Set("1.3.6.1.2.1.1.1.0", "sys")
	r, err := a.GetNext(context.Background(), []string{"1.3.6.1.2.1.1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Varbinds[0].Value.(agent.EndOfMibView); !ok {
		t.Fatalf("want EndOfMibView, got %#v", r.Varbinds[0].Value)
	}
}

func TestGetBulkReturnsRowsPerColumnInOrder(t *testing.T) {
	a := newTable(t, 5, 3) // ifTable with 3 columns and 5 rows
	r, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1", "1.3.6.1.2.1.2.2.1.2"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"1.3.6.1.2.1.2.2.1.1.1", "1.3.6.1.2.1.2.2.1.2.1",
		"1.3.6.1.2.1.2.2.1.1.2", "1.3.6.1.2.1.2.2.1.2.2",
	}
	if len(r.Varbinds) != len(want) {
		t.Fatalf("got %d varbinds, want %d", len(r.Varbinds), len(want))
	}
	for i, w := range want {
		if r.Varbinds[i].OID != w {
			t.Fatalf("varbind %d: got %s want %s", i, r.Varbinds[i].OID, w)
		}
	}
}

func TestGetBulkTruncatesToMaxRows(t *testing.T) {
	a := newTable(t, 100, 1)
	a.MaxRows = 12
	r, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Varbinds) != 12 {
		t.Fatalf("got %d rows, want 12", len(r.Varbinds))
	}
}

func TestGetBulkTruncatesToMaxBytes(t *testing.T) {
	a := newTable(t, 100, 1)
	a.MaxBytes = 200
	r, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Varbinds) >= 50 || r.Bytes > 200 {
		t.Fatalf("got %d rows / %d bytes, want fewer than 50 rows within 200 bytes", len(r.Varbinds), r.Bytes)
	}
}

func TestRTTAboveTimeoutIsTimeout(t *testing.T) {
	a := newTable(t, 10, 1)
	a.Timeout = 100 * time.Millisecond
	a.RTT = func(varbinds int) time.Duration { return time.Duration(varbinds) * 20 * time.Millisecond }
	_, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1"}, 10)
	if !errors.Is(err, agent.ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	r, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1"}, 2)
	if err != nil || r.RTT != 40*time.Millisecond {
		t.Fatalf("want 40ms rtt, got %v %v", r.RTT, err)
	}
}

func TestSysUpTimeAdvancesAndResetsOnRestart(t *testing.T) {
	a := New()
	a.RTT = func(int) time.Duration { return 10 * time.Millisecond }
	a.RestartAfter = 3
	up := func() uint32 {
		r, err := a.Get(context.Background(), []string{agent.SysUpTimeOID})
		if err != nil {
			t.Fatal(err)
		}
		return r.Varbinds[0].Value.(uint32)
	}
	first := up()
	second := up()
	if second <= first {
		t.Fatalf("uptime must advance: %d then %d", first, second)
	}
	third := up()
	fourth := up() // request 4 is after the restart
	if fourth >= third {
		t.Fatalf("uptime must reset after restart: %d then %d", third, fourth)
	}
}

func TestRequestsAreLogged(t *testing.T) {
	a := newTable(t, 3, 1)
	_, _ = a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1"}, 3)
	_, _ = a.Get(context.Background(), []string{agent.SysUpTimeOID})
	if got := a.Requests(); len(got) != 2 || got[0].Kind != "getbulk" || got[1].Kind != "get" {
		t.Fatalf("unexpected request log %+v", got)
	}
}

func TestContextCancelledIsReturned(t *testing.T) {
	a := New()
	a.Set("1.3.6.1.2.1.1.1.0", "sys")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Get(ctx, []string{"1.3.6.1.2.1.1.1.0"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestErrorAboveReturnsAgentError(t *testing.T) {
	a := newTable(t, 10, 2)
	a.ErrorAbove = 5
	_, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1", "1.3.6.1.2.1.2.2.1.2"}, 3)
	if !errors.Is(err, agent.ErrAgent) {
		t.Fatalf("want ErrAgent for 6 requested varbinds, got %v", err)
	}
	if _, err := a.GetBulk(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1"}, 5); err != nil {
		t.Fatalf("5 requested must be fine: %v", err)
	}
}

func TestRetriesReported(t *testing.T) {
	a := newTable(t, 1, 2)
	a.Retries = 1
	r, err := a.Get(context.Background(), []string{agent.SysUpTimeOID})
	if err != nil || r.Retries != 1 {
		t.Fatalf("want Retries 1, got %+v %v", r, err)
	}
}

func TestMisorderAnswersWithGivenOID(t *testing.T) {
	a := newTable(t, 3, 1)
	a.Misorder = map[string]string{"1.3.6.1.2.1.2.2.1.1.2": "1.3.6.1.2.1.2.2.1.1.1"}
	r, err := a.GetNext(context.Background(), []string{"1.3.6.1.2.1.2.2.1.1.2"})
	if err != nil || r.Varbinds[0].OID != "1.3.6.1.2.1.2.2.1.1.1" {
		t.Fatalf("want the configured smaller OID, got %+v %v", r, err)
	}
}
