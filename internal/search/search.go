/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package search finds the largest settings an agent sustains: escalate by
// doubling, bisect after the first failure, then split the product between
// repetitions and vars per PDU with measured trials.
package search

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/oid"
	"github.com/no42-org/snmptune/internal/safety"
	"github.com/no42-org/snmptune/internal/walk"
	"github.com/no42-org/snmptune/internal/workload"
)

// Start is the OpenNMS default, where every run begins.
var Start = walk.Settings{MaxRepetitions: 2, MaxVarsPerPDU: 10}

// SplitVars are the max-vars-per-pdu values tried at the final product.
var SplitVars = []int{5, 10, 20, 50}

// Options tune the search itself.
type Options struct {
	Repeats     int
	Cooldown    time.Duration
	Tolerance   float64 // percent of reference OIDs that may be missing
	PlateauGain float64 // minimum relative gain to keep escalating
}

// DefaultOptions returns the documented defaults.
func DefaultOptions() Options {
	return Options{Repeats: 3, Cooldown: 2 * time.Second, Tolerance: 1, PlateauGain: 0.10}
}

// Trial is one setting measured over several repeats.
type Trial struct {
	Settings walk.Settings
	Phase    string
	Runs     []walk.Result
	Score    float64 // median varbinds per second; 0 when failed
	Failure  string
}

// OK reports whether every repeat was clean.
func (t Trial) OK() bool { return t.Failure == "" }

// Outcome is the whole search.
type Outcome struct {
	Trials        []Trial
	Aborted       string // set when the run stopped for the agent's sake
	BudgetLimited string // set when a budget ended the run
	Stressed      bool
}

// Peak is the error-free trial with the best throughput, or nil.
func (o Outcome) Peak() *Trial {
	var best *Trial
	for i := range o.Trials {
		t := &o.Trials[i]
		if t.OK() && (best == nil || t.Score > best.Score) {
			best = t
		}
	}
	return best
}

// Runner wires the search to one agent.
type Runner struct {
	Transport agent.Transport
	Workload  workload.Workload
	Reference inventory.Inventory
	Canary    *safety.Canary
	Budget    *safety.Tracker
	Logger    walk.Logger
	Sleep     func(context.Context, time.Duration)
	Progress  func(string) // optional, called once per completed trial
	Opts      Options

	out          Outcome
	ceiling      int // products must stay below this
	lastGood     int // largest clean product so far
	startProduct int // product of the first trial, always allowed
	prevRun      time.Duration
	lastSet      walk.Settings
	refOIDs      []string
	measured     map[walk.Settings]bool
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// init prepares a run; ceiling is the first product that must not be tried.
func (r *Runner) init(ceiling int) (v0, widest int) {
	r.out = Outcome{}
	r.ceiling = ceiling
	r.measured = map[walk.Settings]bool{}
	r.refOIDs = referenceOIDs(r.Reference, r.Workload)
	if r.Sleep == nil {
		r.Sleep = sleep
	}
	widest = r.Workload.WidestTable()
	v0 = Start.MaxVarsPerPDU
	if widest > 0 {
		v0 = min(v0, widest)
	}
	r.startProduct = int(Start.MaxRepetitions) * v0
	return v0, widest
}

// Measure runs exactly the given settings, in order, with the same repeats,
// canary and cooldown as the search. It is for verifying or comparing
// settings the operator names.
func (r *Runner) Measure(ctx context.Context, settings []walk.Settings) Outcome {
	r.init(int(^uint(0) >> 1))
	for _, s := range settings {
		if _, stop := r.trial(ctx, s, "try"); stop {
			return r.out
		}
	}
	r.finalProbe(ctx)
	return r.out
}

// Run executes the search. It never returns an error: everything that stops
// a run is recorded in the Outcome so a report can still be produced.
func (r *Runner) Run(ctx context.Context) Outcome {
	v0, widest := r.init(r.Budget.MaxProduct + 1)

	// Escalate.
	reps := Start.MaxRepetitions
	var lastGoodR, firstBadR uint32
	prevScore := 0.0
	for {
		s := walk.Settings{MaxRepetitions: reps, MaxVarsPerPDU: v0}
		if s.Product() > r.Budget.MaxProduct {
			break
		}
		t, stop := r.trial(ctx, s, "escalate")
		if stop {
			return r.out
		}
		if t == nil {
			break
		}
		if !t.OK() {
			firstBadR = reps
			break
		}
		if lastGoodR > 0 && t.Score < prevScore*(1+r.Opts.PlateauGain) {
			lastGoodR = reps
			break
		}
		lastGoodR, prevScore = reps, t.Score
		if r.out.Stressed {
			break
		}
		reps *= 2
	}

	// Bisect.
	if firstBadR > 0 {
		for firstBadR-lastGoodR > max(1, lastGoodR/10) {
			mid := (lastGoodR + firstBadR) / 2
			t, stop := r.trial(ctx, walk.Settings{MaxRepetitions: mid, MaxVarsPerPDU: v0}, "bisect")
			if stop {
				return r.out
			}
			if t == nil {
				break
			}
			if t.OK() {
				lastGoodR = mid
			} else {
				firstBadR = mid
			}
		}
	}

	// Split.
	if lastGoodR > 0 {
		product := int(lastGoodR) * v0
		for _, v := range splitCandidates(v0, widest) {
			if product/v < 1 {
				continue
			}
			if _, stop := r.trial(ctx, walk.Settings{MaxRepetitions: uint32(product / v), MaxVarsPerPDU: v}, "split"); stop {
				return r.out
			}
		}
	}
	r.finalProbe(ctx)
	return r.out
}

// splitCandidates are the max-vars-per-pdu values tried at the final
// product: the standard ladder capped at the widest table, plus the table
// width itself so "every column in one PDU" is always measured.
func splitCandidates(v0, widest int) []int {
	var out []int
	for _, v := range SplitVars {
		if v != v0 && (widest == 0 || v <= widest) {
			out = append(out, v)
		}
	}
	if widest > 0 && widest != v0 && !slices.Contains(out, widest) {
		out = append(out, widest)
	}
	slices.Sort(out)
	return out
}

// referenceOIDs keeps only the inventory OIDs the workload actually walks:
// a datacollection group lists some columns of a table, and the trial must
// not be blamed for columns it never asked for.
func referenceOIDs(inv inventory.Inventory, w workload.Workload) []string {
	columns := map[string][]string{}
	for _, t := range w.Tables {
		columns[oid.Canonical(t.Base)] = append(columns[oid.Canonical(t.Base)], t.Columns...)
	}
	var out []string
	for _, st := range inv.Subtrees {
		cols := columns[st.Root]
		for _, o := range st.OIDs {
			if slices.ContainsFunc(cols, func(c string) bool { return oid.HasPrefix(o, c) }) {
				out = append(out, o)
			}
		}
	}
	return out
}

// finalProbe catches a restart or loss caused by the very last trial, which
// the per-repeat probes cannot see.
func (r *Runner) finalProbe(ctx context.Context) {
	if ctx.Err() != nil || len(r.out.Trials) == 0 {
		return
	}
	var reason string
	switch r.Canary.Probe(ctx) {
	case safety.Restarted:
		reason = fmt.Sprintf("agent restarted after trial %s", r.lastSet)
	case safety.Lost:
		reason = fmt.Sprintf("agent stopped answering canary probes after trial %s", r.lastSet)
	default:
		return
	}
	r.out.Aborted = reason
	for i := range r.out.Trials {
		if t := &r.out.Trials[i]; t.Settings == r.lastSet {
			t.Failure, t.Score = "aborted: "+reason, 0
		}
	}
}

// interrupted classifies a done context: the deadline is the duration
// budget, a cancel is the operator.
func (r *Runner) interrupted(ctx context.Context, s walk.Settings, phase string, runs []walk.Result) bool {
	switch ctx.Err() {
	case nil:
		return false
	case context.DeadlineExceeded:
		r.out.BudgetLimited = "max-duration reached"
	default:
		r.out.Aborted = fmt.Sprintf("interrupted during trial %s", s)
	}
	if len(runs) > 0 {
		r.record(s, phase, runs, "aborted: run interrupted")
	}
	return true
}

// record stores a trial that did not complete its repeats as failed.
func (r *Runner) record(s walk.Settings, phase string, runs []walk.Result, failure string) {
	t := score(s, phase, runs)
	t.Failure, t.Score = failure, 0
	r.out.Trials = append(r.out.Trials, t)
	r.Budget.NoteTrial()
}

// trial measures one setting. It returns nil when the setting was skipped
// (above the ceiling or already measured) and stop=true when the run must end.
func (r *Runner) trial(ctx context.Context, s walk.Settings, phase string) (*Trial, bool) {
	if s.Product() >= r.ceiling || r.measured[s] {
		return nil, false
	}
	if r.interrupted(ctx, s, phase, nil) {
		return nil, true
	}
	if reason := r.Budget.Exhausted(); reason != "" {
		r.out.BudgetLimited = reason
		return nil, true
	}
	r.measured[s] = true
	var runs []walk.Result
	for i := range r.Opts.Repeats {
		r.Sleep(ctx, safety.Cooldown(r.Opts.Cooldown, r.prevRun))
		if r.interrupted(ctx, s, phase, runs) {
			return nil, true
		}
		switch r.Canary.Probe(ctx) {
		case safety.Restarted:
			return r.abort(fmt.Sprintf("agent restarted after trial %s", r.lastSet), s, phase, runs)
		case safety.Lost:
			return r.abort(fmt.Sprintf("agent stopped answering canary probes after trial %s", r.lastSet), s, phase, runs)
		case safety.Stressed:
			// Stop escalating, but always measure the first setting so the
			// report has at least the default to say something about. A
			// requested setting is never vetoed: the operator asked for it.
			r.out.Stressed = true
			if phase != "try" {
				r.ceiling = min(r.ceiling, max(r.lastGood, r.startProduct)+1)
			}
			if s.Product() >= r.ceiling {
				if len(runs) > 0 {
					r.record(s, phase, runs, fmt.Sprintf("aborted: agent stressed before repeat %d", i+1))
				}
				return nil, false
			}
		}
		res, err := walk.Run(ctx, r.Transport, r.Workload, s, r.Logger)
		r.lastSet, r.prevRun = s, res.Duration
		if err != nil {
			if r.interrupted(ctx, s, phase, runs) {
				return nil, true
			}
			r.out.Aborted = fmt.Sprintf("transport error during %s: %v", s, err)
			return nil, true
		}
		if res.Failure == "" && len(r.refOIDs) > 0 {
			missing := walk.Missing(r.refOIDs, res.OIDs)
			if pct := 100 * float64(missing) / float64(len(r.refOIDs)); pct > r.Opts.Tolerance {
				res.Failure = fmt.Sprintf("missing %d of %d reference OIDs (%.1f%%)", missing, len(r.refOIDs), pct)
			}
		}
		runs = append(runs, res)
		if res.Failure != "" {
			break
		}
	}
	t := score(s, phase, runs)
	r.out.Trials = append(r.out.Trials, t)
	r.Budget.NoteTrial()
	if t.OK() {
		r.lastGood = max(r.lastGood, s.Product())
	} else {
		r.ceiling = min(r.ceiling, s.Product())
	}
	if r.Progress != nil {
		r.Progress(fmt.Sprintf("%s %s: %s", phase, s, t.Summary()))
	}
	return &r.out.Trials[len(r.out.Trials)-1], false
}

// abort ends the run. A trial interrupted between repeats is recorded as
// failed so the report shows what ran right before the agent gave out.
func (r *Runner) abort(reason string, s walk.Settings, phase string, runs []walk.Result) (*Trial, bool) {
	r.out.Aborted = reason
	if len(runs) > 0 {
		r.record(s, phase, runs, "aborted: "+reason)
	}
	return nil, true
}

// score folds the repeats of one setting into a Trial.
func score(s walk.Settings, phase string, runs []walk.Result) Trial {
	t := Trial{Settings: s, Phase: phase, Runs: runs}
	var rates []float64
	for _, res := range runs {
		if res.Failure != "" {
			t.Failure = res.Failure
			return t
		}
		rates = append(rates, res.VarbindsPerSecond())
	}
	if len(rates) == 0 {
		t.Failure = "no runs"
		return t
	}
	slices.Sort(rates)
	t.Score = rates[len(rates)/2]
	return t
}

// Summary is a one-line description of the trial for progress output.
func (t Trial) Summary() string {
	if !t.OK() {
		return "failed: " + t.Failure
	}
	return fmt.Sprintf("%.0f varbinds/s", t.Score)
}

// Plan describes what a run would do without touching the agent.
func Plan(w workload.Workload, b safety.Budget, o Options) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Workload: %d tables, %d scalars\n", len(w.Tables), len(w.Scalars))
	for _, t := range w.Tables {
		fmt.Fprintf(&sb, "  table %s: %d columns\n", t.Base, len(t.Columns))
	}
	if len(w.Scalars) > 0 {
		fmt.Fprintf(&sb, "  scalars: %s\n", strings.Join(w.Scalars, " "))
	}
	fmt.Fprintf(&sb, "Reference walk: single-repeater GetBulk R=%d over %s, budget max-oids %d\n",
		inventory.ReferenceRepetitions, strings.Join(w.Roots(), " "), b.MaxOIDs)
	fmt.Fprintf(&sb, "Budgets: max-duration %s, max-trials %d, max-product %d\n", b.MaxDuration, b.MaxTrials, b.MaxProduct)
	fmt.Fprintf(&sb, "Trial: %d repeats, cooldown >= %s, tolerance %.1f%% missing OIDs\n", o.Repeats, o.Cooldown, o.Tolerance)
	widest := w.WidestTable()
	v0 := Start.MaxVarsPerPDU
	if widest > 0 {
		v0 = min(v0, widest)
	}
	sb.WriteString("Escalation ladder (stops early at plateau or first failure, then bisects):\n")
	n := 0
	for reps := Start.MaxRepetitions; int(reps)*v0 <= b.MaxProduct; reps *= 2 {
		fmt.Fprintf(&sb, "  R=%d V=%d\n", reps, v0)
		n++
	}
	var vs []string
	for _, v := range splitCandidates(v0, widest) {
		vs = append(vs, fmt.Sprint(v))
	}
	fmt.Fprintf(&sb, "Split phase: V in {%s} at the final product\n", strings.Join(vs, ", "))
	fmt.Fprintf(&sb, "Per trial repeat, worst case under the OID budget: about %d PDUs and %d bytes; "+
		"at most %d trials x %d repeats.\n", b.MaxOIDs/max(1, Start.MaxVarsPerPDU*int(Start.MaxRepetitions)), b.MaxOIDs*64, b.MaxTrials, o.Repeats)
	fmt.Fprintf(&sb, "Dry run: no packets sent.\n")
	return sb.String()
}
