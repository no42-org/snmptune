/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package search

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/agent/sim"
	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/safety"
	"github.com/no42-org/snmptune/internal/walk"
	"github.com/no42-org/snmptune/internal/workload"
)

// fixture builds a runner over a simulated table. rtt gets the varbind count
// of each response, so tests can shape throughput and stress.
func fixture(t *testing.T, cols, rows int, budget safety.Budget, configure func(*sim.Agent)) (*Runner, *sim.Agent) {
	t.Helper()
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", cols, rows)
	a.RTT = func(n int) time.Duration { return time.Millisecond + time.Duration(n)*10*time.Microsecond }
	if configure != nil {
		configure(a)
	}
	inv, err := inventory.Walk(context.Background(), a, []string{"1.3.6.1.2.1.2.2"}, inventory.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	canary, err := safety.NewCanary(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	tr := safety.NewTracker(budget)
	tr.Start()
	opts := DefaultOptions()
	opts.Repeats = 2
	return &Runner{
		Transport: a, Workload: workload.FromInventory(inv), Reference: inv,
		Canary: canary, Budget: tr, Sleep: func(context.Context, time.Duration) {}, Opts: opts,
	}, a
}

func settingsOf(o Outcome) string {
	var parts []string
	for _, tr := range o.Trials {
		parts = append(parts, fmt.Sprintf("%s/%s/%v", tr.Settings, tr.Phase, tr.OK()))
	}
	return strings.Join(parts, " ")
}

func TestFirstTrialIsOpenNMSDefault(t *testing.T) {
	r, _ := fixture(t, 12, 4, safety.Defaults(), nil)
	o := r.Run(context.Background())
	if len(o.Trials) == 0 || o.Trials[0].Settings != (walk.Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}) {
		t.Fatalf("first trial must be R2 V10: %s", settingsOf(o))
	}
}

func TestEscalationStopsAtPlateau(t *testing.T) {
	// 20 rows, one column: above R=32 the walk is one PDU either way.
	r, _ := fixture(t, 1, 20, safety.Defaults(), nil)
	o := r.Run(context.Background())
	var last Trial
	for _, tr := range o.Trials {
		if tr.Phase == "escalate" {
			last = tr
		}
	}
	if last.Settings.MaxRepetitions != 64 || !last.OK() {
		t.Fatalf("escalation should stop at R64 by plateau: %s", settingsOf(o))
	}
	for _, tr := range o.Trials {
		if tr.Phase == "bisect" {
			t.Fatalf("plateau needs no bisect: %s", settingsOf(o))
		}
	}
	if o.Aborted != "" || o.BudgetLimited != "" {
		t.Fatalf("clean run expected: %+v", o)
	}
}

func TestFailureThenBisect(t *testing.T) {
	r, _ := fixture(t, 1, 100, safety.Defaults(), func(a *sim.Agent) { a.MaxRows = 12 })
	o := r.Run(context.Background())
	got := map[uint32]bool{}
	for _, tr := range o.Trials {
		got[tr.Settings.MaxRepetitions] = tr.OK()
	}
	if got[16] || !got[8] || !got[12] || got[13] {
		t.Fatalf("expected 8 ok, 12 ok, 13 and 16 truncated: %s", settingsOf(o))
	}
	if p := o.Peak(); p == nil || p.Settings.MaxRepetitions != 12 {
		t.Fatalf("peak must be R12: %s", settingsOf(o))
	}
	minFailed := 0
	for _, tr := range o.Trials {
		if !tr.OK() && (minFailed == 0 || tr.Settings.Product() < minFailed) {
			minFailed = tr.Settings.Product()
		}
	}
	for i, tr := range o.Trials {
		for _, prev := range o.Trials[:i] {
			if !prev.OK() && tr.Settings.Product() >= prev.Settings.Product() {
				t.Fatalf("trial %s ran after %s had failed: %s", tr.Settings, prev.Settings, settingsOf(o))
			}
		}
	}
}

func TestSplitTriesOtherVAtSameProductBelowCeiling(t *testing.T) {
	// 20 columns, byte cap around 300 varbinds per response.
	r, _ := fixture(t, 20, 50, safety.Defaults(), func(a *sim.Agent) { a.MaxBytes = 300 * 22 })
	o := r.Run(context.Background())
	var split []Trial
	for _, tr := range o.Trials {
		if tr.Phase == "split" {
			split = append(split, tr)
		}
	}
	if len(split) < 2 {
		t.Fatalf("want split trials for V5 and V20: %s", settingsOf(o))
	}
	vs := map[int]bool{}
	for _, tr := range split {
		vs[tr.Settings.MaxVarsPerPDU] = true
		if tr.Settings.MaxVarsPerPDU == 10 || tr.Settings.MaxVarsPerPDU > 20 {
			t.Fatalf("split must skip V10 (measured) and V50 (wider than table): %s", settingsOf(o))
		}
	}
	if !vs[5] || !vs[20] {
		t.Fatalf("want V5 and V20: %s", settingsOf(o))
	}
	for i, tr := range o.Trials {
		for _, prev := range o.Trials[:i] {
			if !prev.OK() && tr.Settings.Product() >= prev.Settings.Product() {
				t.Fatalf("split exceeded ceiling: %s after failed %s", tr.Settings, prev.Settings)
			}
		}
	}
}

func TestOneBadRepeatFailsTrial(t *testing.T) {
	runs := []walk.Result{
		{Varbinds: 100, PDUs: []walk.PDU{{RTT: time.Second}}},
		{Varbinds: 100, PDUs: []walk.PDU{{RTT: time.Second}}, Failure: "timeout"},
		{Varbinds: 100, PDUs: []walk.PDU{{RTT: time.Second}}},
	}
	tr := score(walk.Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, "escalate", runs)
	if tr.OK() || tr.Score != 0 {
		t.Fatalf("one failed repeat must fail the trial: %+v", tr)
	}
}

func TestScoreIsMedianThroughput(t *testing.T) {
	mk := func(rtt time.Duration) walk.Result {
		return walk.Result{Varbinds: 100, PDUs: []walk.PDU{{RTT: rtt}}}
	}
	tr := score(walk.Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}, "x", []walk.Result{mk(time.Second), mk(4 * time.Second), mk(2 * time.Second)})
	if tr.Score != 50 {
		t.Fatalf("median of 100, 25, 50 is 50, got %v", tr.Score)
	}
}

func TestStressedCanaryStopsEscalation(t *testing.T) {
	stressed := false
	r, a := fixture(t, 1, 200, safety.Defaults(), nil)
	base := a.RTT
	a.RTT = func(n int) time.Duration {
		if stressed && n == 1 {
			return 50 * time.Millisecond
		}
		return base(n)
	}
	var seen int
	r.Progress = func(string) {
		seen++
		if seen == 3 {
			stressed = true
		}
	}
	o := r.Run(context.Background())
	if !o.Stressed {
		t.Fatalf("stress must be recorded: %s", settingsOf(o))
	}
	last := o.Trials[len(o.Trials)-1].Settings.Product()
	best := 0
	for _, tr := range o.Trials {
		if tr.OK() {
			best = max(best, tr.Settings.Product())
		}
	}
	if last > best {
		t.Fatalf("no trial above the last good product after stress: %s", settingsOf(o))
	}
}

func TestRestartAbortsAndNamesPrecedingTrial(t *testing.T) {
	r, a := fixture(t, 1, 30, safety.Defaults(), nil)
	a.RestartAfter = len(a.Requests()) + 40
	o := r.Run(context.Background())
	if !strings.Contains(o.Aborted, "restart") {
		t.Fatalf("want restart abort, got %q (%s)", o.Aborted, settingsOf(o))
	}
	last := o.Trials[len(o.Trials)-1]
	if !strings.Contains(o.Aborted, last.Settings.String()) {
		t.Fatalf("abort must name the preceding setting %s: %q", last.Settings, o.Aborted)
	}
}

func TestTrialBudgetEndsRunWithResult(t *testing.T) {
	r, _ := fixture(t, 1, 200, safety.Budget{MaxTrials: 2, MaxProduct: 1000}, nil)
	o := r.Run(context.Background())
	if len(o.Trials) != 2 || o.BudgetLimited == "" || o.Peak() == nil {
		t.Fatalf("want 2 trials, budget flag and a peak: %s %q", settingsOf(o), o.BudgetLimited)
	}
}

func TestPlanSendsNothing(t *testing.T) {
	w := workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: []string{"1.3.6.1.2.1.2.2.1.10"}}}, Scalars: []string{"1.3.6.1.2.1.1.3.0"}}
	text := Plan(w, safety.Defaults(), DefaultOptions())
	for _, want := range []string{"1.3.6.1.2.1.2.2", "R=2 V=1", "R=512 V=1", "max-product", "worst case"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "R=1024") {
		t.Fatalf("ladder must stop at max-product 1000:\n%s", text)
	}
}

func TestGroupWorkloadChecksOnlyItsColumns(t *testing.T) {
	// Reference covers the whole 14-column table; the trial walks 3 columns.
	r, _ := fixture(t, 14, 4, safety.Defaults(), nil)
	r.Workload = workload.Workload{Tables: []workload.Table{{Base: "1.3.6.1.2.1.2.2", Columns: []string{
		"1.3.6.1.2.1.2.2.1.10", "1.3.6.1.2.1.2.2.1.14", "1.3.6.1.2.1.2.2.1.2"}}}}
	o := r.Run(context.Background())
	if p := o.Peak(); p == nil {
		t.Fatalf("group workload must pass correctness: %s", settingsOf(o))
	}
}

func TestStressBeforeFirstTrialStillMeasuresDefault(t *testing.T) {
	r, a := fixture(t, 1, 20, safety.Defaults(), nil)
	base := a.RTT
	a.RTT = func(n int) time.Duration {
		if n == 1 {
			return 100 * time.Millisecond // every canary from now on is slow
		}
		return base(n)
	}
	o := r.Run(context.Background())
	if len(o.Trials) != 1 || o.Trials[0].Settings.MaxRepetitions != Start.MaxRepetitions || !o.Trials[0].OK() || !o.Stressed {
		t.Fatalf("want exactly the default trial measured under stress: %s stressed=%v", settingsOf(o), o.Stressed)
	}
}

func TestStressMidTrialRecordsPartialTrial(t *testing.T) {
	r, a := fixture(t, 1, 20, safety.Defaults(), nil)
	base := a.RTT
	stressed := false
	a.RTT = func(n int) time.Duration {
		if stressed && n == 1 {
			return 100 * time.Millisecond
		}
		return base(n)
	}
	r.Opts.Repeats = 2
	walks := 0
	r.Logger = countingLogger{n: &walks, onWalk: func(n int) {
		if n == 3 { // after repeat 1 of trial 2 (R=4)
			stressed = true
		}
	}}
	o := r.Run(context.Background())
	var found *Trial
	for i := range o.Trials {
		if o.Trials[i].Settings.MaxRepetitions == 4 {
			found = &o.Trials[i]
		}
	}
	if found == nil || found.OK() || !strings.Contains(found.Failure, "stress") {
		t.Fatalf("interrupted trial must be recorded as failed: %s", settingsOf(o))
	}
	if r.Budget.Trials != len(o.Trials) {
		t.Fatalf("every recorded trial counts against the budget: %d vs %d", r.Budget.Trials, len(o.Trials))
	}
}

// countingLogger counts walks by their first PDU.
type countingLogger struct {
	n      *int
	onWalk func(int)
}

func (c countingLogger) Log(p walk.PDU) {
	if p.Seq == 1 {
		*c.n++
		c.onWalk(*c.n)
	}
}

func TestRestartOnLastRequestIsDetected(t *testing.T) {
	r, a := fixture(t, 12, 4, safety.Defaults(), nil)
	clean := r.Run(context.Background())
	if clean.Aborted != "" {
		t.Fatalf("baseline run must be clean: %q", clean.Aborted)
	}
	total := len(a.Requests())

	r2, a2 := fixture(t, 12, 4, safety.Defaults(), nil)
	a2.RestartAfter = total - 1 // the reset lands on the very last request
	o := r2.Run(context.Background())
	if !strings.Contains(o.Aborted, "restart") {
		t.Fatalf("restart after the last trial must be caught: aborted=%q", o.Aborted)
	}
	last := o.Trials[len(o.Trials)-1]
	if last.OK() {
		t.Fatalf("the trial that preceded the restart must not stay ok: %s", settingsOf(o))
	}
}

func TestInterruptIsReportedAsSuch(t *testing.T) {
	r, _ := fixture(t, 1, 200, safety.Defaults(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	walks := 0
	r.Logger = countingLogger{n: &walks, onWalk: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	o := r.Run(ctx)
	if !strings.Contains(o.Aborted, "interrupted") {
		t.Fatalf("cancel must be reported as an interrupt, got %q", o.Aborted)
	}
}

func TestDeadlineIsBudget(t *testing.T) {
	r, _ := fixture(t, 1, 200, safety.Defaults(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dctx, dcancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer dcancel()
	o := r.Run(dctx)
	if o.BudgetLimited == "" || o.Aborted != "" {
		t.Fatalf("an expired deadline is the duration budget: %+v", o)
	}
}

func TestSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	sleep(ctx, time.Minute)
	if time.Since(start) > time.Second {
		t.Fatal("sleep must return on cancel")
	}
}

func TestMeasureRunsExactlyTheGivenSettings(t *testing.T) {
	r, _ := fixture(t, 19, 4, safety.Defaults(), nil)
	want := []walk.Settings{{MaxRepetitions: 4, MaxVarsPerPDU: 10}, {MaxRepetitions: 2, MaxVarsPerPDU: 20}}
	o := r.Measure(context.Background(), want)
	if len(o.Trials) != 2 || o.Trials[0].Settings != want[0] || o.Trials[1].Settings != want[1] {
		t.Fatalf("want exactly the two requested trials in order: %s", settingsOf(o))
	}
	for _, tr := range o.Trials {
		if tr.Phase != "try" || !tr.OK() || len(tr.Runs) != r.Opts.Repeats {
			t.Fatalf("each requested setting is measured with all repeats: %+v", tr)
		}
	}
}

func TestSplitTriesTheTableWidth(t *testing.T) {
	// 19 columns: V=20 and V=50 are wider than the table, so V=19 (all columns
	// in one PDU) must be tried instead.
	r, _ := fixture(t, 19, 4, safety.Budget{MaxProduct: 40, MaxTrials: 40}, nil)
	o := r.Run(context.Background())
	seen := map[int]bool{}
	for _, tr := range o.Trials {
		if tr.Phase == "split" {
			seen[tr.Settings.MaxVarsPerPDU] = true
		}
	}
	if !seen[19] || !seen[5] || seen[20] {
		t.Fatalf("want split V in {5, 19}: %s", settingsOf(o))
	}
}

func TestMeasureIsNotVetoedByStress(t *testing.T) {
	r, a := fixture(t, 19, 4, safety.Defaults(), nil)
	base := a.RTT
	a.RTT = func(n int) time.Duration {
		if n == 1 {
			return 100 * time.Millisecond // every canary is slow
		}
		return base(n)
	}
	o := r.Measure(context.Background(), []walk.Settings{{MaxRepetitions: 4, MaxVarsPerPDU: 10}})
	if len(o.Trials) != 1 || !o.Trials[0].OK() || len(o.Trials[0].Runs) != r.Opts.Repeats || !o.Stressed {
		t.Fatalf("a requested setting is measured fully and stress is only noted: %s stressed=%v", settingsOf(o), o.Stressed)
	}
}
