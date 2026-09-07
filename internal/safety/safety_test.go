/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package safety

import (
	"context"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/agent/sim"
)

func TestBaselineUnreachableIsError(t *testing.T) {
	a := sim.New()
	a.Timeout = time.Millisecond
	a.RTT = func(int) time.Duration { return time.Second }
	if _, err := NewCanary(context.Background(), a); err == nil {
		t.Fatal("five lost probes must be an error")
	}
	if n := len(a.Requests()); n != 5 {
		t.Fatalf("baseline must send exactly 5 probes, sent %d", n)
	}
}

func TestBaselineIsMedianOfProbes(t *testing.T) {
	a := sim.New()
	rtts := []time.Duration{1, 9, 2, 8, 3}
	i := 0
	a.RTT = func(int) time.Duration { i++; return rtts[(i-1)%len(rtts)] * time.Millisecond }
	c, err := NewCanary(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if c.Baseline != 3*time.Millisecond {
		t.Fatalf("want median 3ms, got %v", c.Baseline)
	}
}

func TestProbeStressedAboveThreeTimesBaseline(t *testing.T) {
	a := sim.New()
	slow := false
	a.RTT = func(int) time.Duration {
		if slow {
			return 40 * time.Millisecond
		}
		return 10 * time.Millisecond
	}
	c, _ := NewCanary(context.Background(), a)
	if v := c.Probe(context.Background()); v != OK {
		t.Fatalf("want OK, got %v", v)
	}
	slow = true
	if v := c.Probe(context.Background()); v != Stressed {
		t.Fatalf("want Stressed at 4x baseline, got %v", v)
	}
}

func TestProbeIgnoresJitterBelowFloor(t *testing.T) {
	a := sim.New()
	fast := true
	a.RTT = func(int) time.Duration {
		if fast {
			return 200 * time.Microsecond
		}
		return 2 * time.Millisecond
	}
	c, _ := NewCanary(context.Background(), a)
	fast = false
	if v := c.Probe(context.Background()); v != OK {
		t.Fatalf("10x on a sub-millisecond baseline is jitter, not stress; got %v", v)
	}
}

func TestProbeDetectsRestart(t *testing.T) {
	a := sim.New()
	a.RTT = func(int) time.Duration { return time.Millisecond }
	c, _ := NewCanary(context.Background(), a)
	a.RestartAfter = len(a.Requests()) + 1
	if v := c.Probe(context.Background()); v != OK {
		t.Fatalf("probe before restart must be OK, got %v", v)
	}
	if v := c.Probe(context.Background()); v != Restarted {
		t.Fatalf("uptime went backwards, want Restarted, got %v", v)
	}
}

func TestProbeLostTwiceInARow(t *testing.T) {
	a := sim.New()
	a.RTT = func(int) time.Duration { return time.Millisecond }
	c, _ := NewCanary(context.Background(), a)
	a.Timeout = 0
	if v := c.Probe(context.Background()); v != OK {
		t.Fatalf("one lost canary is tolerated, got %v", v)
	}
	if v := c.Probe(context.Background()); v != Lost {
		t.Fatalf("second consecutive loss must be Lost, got %v", v)
	}
}

func TestTrackerExhaustion(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker(Budget{MaxDuration: time.Minute, MaxTrials: 2})
	tr.Now = func() time.Time { return now }
	tr.Start()
	if r := tr.Exhausted(); r != "" {
		t.Fatalf("fresh tracker exhausted: %s", r)
	}
	tr.NoteTrial()
	tr.NoteTrial()
	if r := tr.Exhausted(); r == "" {
		t.Fatal("two trials must exhaust MaxTrials 2")
	}
	tr = NewTracker(Budget{MaxDuration: time.Minute})
	tr.Now = func() time.Time { return now }
	tr.Start()
	now = now.Add(61 * time.Second)
	if r := tr.Exhausted(); r == "" {
		t.Fatal("duration budget must exhaust")
	}
}

func TestCooldownIsMaxOfMinimumAndPreviousRun(t *testing.T) {
	if d := Cooldown(2*time.Second, 9*time.Second); d != 9*time.Second {
		t.Fatalf("got %v", d)
	}
	if d := Cooldown(2*time.Second, 100*time.Millisecond); d != 2*time.Second {
		t.Fatalf("got %v", d)
	}
}

func TestProbeWithRetryIsNotStress(t *testing.T) {
	a := sim.New()
	a.RTT = func(int) time.Duration { return time.Millisecond }
	c, _ := NewCanary(context.Background(), a)
	a.Retries = 1
	a.RTT = func(int) time.Duration { return 3 * time.Second }
	if v := c.Probe(context.Background()); v != OK {
		t.Fatalf("a retransmitted probe measures packet loss, not stress; got %v", v)
	}
}
