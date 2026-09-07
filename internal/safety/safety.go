/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package safety holds the guardrails that keep a run from harming the
// agent: canary probes, budgets and cooldown.
package safety

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
)

// Verdict is the outcome of a canary probe.
type Verdict int

const (
	// OK means the agent answered normally.
	OK Verdict = iota
	// Stressed means the answer was far slower than the baseline.
	Stressed
	// Restarted means sysUpTime went backwards.
	Restarted
	// Lost means two consecutive probes timed out.
	Lost
)

func (v Verdict) String() string {
	return [...]string{"ok", "stressed", "restarted", "lost"}[v]
}

const (
	baselineProbes = 5
	stressFactor   = 3
	// stressFloor keeps sub-millisecond LAN jitter from counting as stress.
	stressFloor = 10 * time.Millisecond
)

// Canary probes sysUpTime with a single-OID Get.
type Canary struct {
	tr         agent.Transport
	Baseline   time.Duration
	lastUptime uint32
	seen       bool
	lost       int
}

// NewCanary sends the baseline probes and fails when none is answered.
func NewCanary(ctx context.Context, tr agent.Transport) (*Canary, error) {
	c := &Canary{tr: tr}
	var rtts []time.Duration
	for range baselineProbes {
		resp, err := tr.Get(ctx, []string{agent.SysUpTimeOID})
		if err != nil {
			continue
		}
		rtts = append(rtts, resp.RTT)
		c.noteUptime(resp)
	}
	if len(rtts) == 0 {
		return nil, fmt.Errorf("agent did not answer any of %d sysUpTime probes", baselineProbes)
	}
	slices.Sort(rtts)
	c.Baseline = rtts[len(rtts)/2]
	return c, nil
}

// Probe sends one canary and classifies the answer.
func (c *Canary) Probe(ctx context.Context) Verdict {
	resp, err := c.tr.Get(ctx, []string{agent.SysUpTimeOID})
	if err != nil {
		c.lost++
		if c.lost >= 2 {
			return Lost
		}
		return OK
	}
	c.lost = 0
	if c.seen && uptime(resp) < c.lastUptime {
		c.noteUptime(resp)
		return Restarted
	}
	c.noteUptime(resp)
	if resp.Retries > 0 {
		return OK // the RTT includes a retransmission; that is loss, not agent stress
	}
	if resp.RTT > stressFactor*c.Baseline && resp.RTT > stressFloor {
		return Stressed
	}
	return OK
}

func (c *Canary) noteUptime(resp agent.Response) {
	c.lastUptime, c.seen = uptime(resp), true
}

func uptime(resp agent.Response) uint32 {
	if len(resp.Varbinds) == 0 {
		return 0
	}
	switch v := resp.Varbinds[0].Value.(type) {
	case uint32:
		return v
	case uint:
		return uint32(v)
	case int:
		return uint32(v)
	}
	return 0
}

// Budget bounds a run. Zero values mean unlimited.
type Budget struct {
	MaxDuration time.Duration
	MaxTrials   int
	MaxProduct  int
	MaxOIDs     int
}

// Defaults are conservative enough for production gear.
func Defaults() Budget {
	return Budget{MaxDuration: 15 * time.Minute, MaxTrials: 40, MaxProduct: 1000, MaxOIDs: 200_000}
}

// Tracker counts trials and time against a Budget.
type Tracker struct {
	Budget
	Now    func() time.Time
	Trials int
	start  time.Time
}

// NewTracker returns an unstarted tracker.
func NewTracker(b Budget) *Tracker { return &Tracker{Budget: b, Now: time.Now} }

// Start marks the beginning of the run.
func (t *Tracker) Start() { t.start = t.Now() }

// NoteTrial counts one completed trial.
func (t *Tracker) NoteTrial() { t.Trials++ }

// Elapsed is the time since Start.
func (t *Tracker) Elapsed() time.Duration { return t.Now().Sub(t.start) }

// Exhausted returns the reason when a budget is used up, else "".
func (t *Tracker) Exhausted() string {
	if t.MaxTrials > 0 && t.Trials >= t.MaxTrials {
		return fmt.Sprintf("max-trials %d reached", t.MaxTrials)
	}
	if t.MaxDuration > 0 && t.Elapsed() >= t.MaxDuration {
		return fmt.Sprintf("max-duration %s reached", t.MaxDuration)
	}
	return ""
}

// Cooldown is the pause before the next request burst: at least minimum,
// and never shorter than the run that just finished.
func Cooldown(minimum, previous time.Duration) time.Duration {
	return max(minimum, previous)
}
