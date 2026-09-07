/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package report scores a search outcome into a recommendation an operator
// can paste, and renders it as text, JSON or an OpenNMS snmp-config snippet.
package report

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/search"
	"github.com/no42-org/snmptune/internal/walk"
)

const (
	timeoutFactor  = 3
	timeoutRoundMs = 100
	timeoutFloorMs = 500
)

// Recommendation is what goes into snmp-config.xml, with the evidence.
type Recommendation struct {
	Settings  walk.Settings `json:"settings"`
	Peak      walk.Settings `json:"peak"`
	Reason    string        `json:"reason"`
	P50       time.Duration `json:"-"`
	P99       time.Duration `json:"-"`
	P50ms     float64       `json:"p50_ms"`
	P99ms     float64       `json:"p99_ms"`
	TimeoutMs int           `json:"timeout_ms"`
	Retry     int           `json:"retry"`
}

// Recommend picks the largest error-free product strictly below the peak,
// or the default when the peak is the default. ok is false when no trial
// passed at all.
func Recommend(o search.Outcome) (rec Recommendation, ok bool) {
	peak := o.Peak()
	if peak == nil {
		return rec, false
	}
	chosen := peak
	if allRequested(o) {
		rec.Reason = fmt.Sprintf("best of the requested settings (%.0f varbinds/s); no headroom step in try mode", peak.Score)
	} else if peak.Settings == search.Start {
		rec.Reason = "the agent did not sustain more than the OpenNMS default without errors"
	} else {
		var below *search.Trial
		for i := range o.Trials {
			t := &o.Trials[i]
			if !t.OK() || t.Settings.Product() >= peak.Settings.Product() {
				continue
			}
			if below == nil || t.Settings.Product() > below.Settings.Product() ||
				t.Settings.Product() == below.Settings.Product() && t.Score > below.Score {
				below = t
			}
		}
		if below != nil {
			chosen = below
			rec.Reason = fmt.Sprintf("one notch of headroom below the observed peak %s (%.0f varbinds/s); production shares the agent with other pollers", peak.Settings, peak.Score)
		} else {
			rec.Reason = "no error-free setting below the peak was measured"
		}
	}
	rec.Settings, rec.Peak = chosen.Settings, peak.Settings
	rtts, retried := rtts(*chosen)
	rec.P50, rec.P99 = percentile(rtts, 50), percentile(rtts, 99)
	rec.P50ms, rec.P99ms = ms(rec.P50), ms(rec.P99)
	rec.TimeoutMs = SuggestTimeout(rec.P99)
	rec.Retry = 1
	if retried {
		rec.Retry = 2
	}
	return rec, true
}

// allRequested reports whether every trial came from --try.
func allRequested(o search.Outcome) bool {
	for _, t := range o.Trials {
		if t.Phase != "try" {
			return false
		}
	}
	return len(o.Trials) > 0
}

// SuggestTimeout is three times the p99 round-trip time, rounded up to
// 100 ms, never below 500 ms.
func SuggestTimeout(p99 time.Duration) int {
	v := int(math.Ceil(float64(p99.Milliseconds())*timeoutFactor/timeoutRoundMs)) * timeoutRoundMs
	return max(v, timeoutFloorMs)
}

func rtts(t search.Trial) (out []time.Duration, retried bool) {
	for _, run := range t.Runs {
		for _, p := range run.PDUs {
			if p.Retries > 0 {
				retried = true // its RTT includes the timeout, so it is not an agent measurement
				continue
			}
			if p.Error == "" {
				out = append(out, p.RTT)
			}
		}
	}
	return out, retried
}

func percentile(d []time.Duration, pct float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(math.Ceil(pct/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Report is everything the operator sees.
type Report struct {
	Target         string
	Reference      inventory.Inventory
	Outcome        search.Outcome
	Recommendation *Recommendation
}

// Build assembles the report for one run.
func Build(target string, inv inventory.Inventory, o search.Outcome) Report {
	r := Report{Target: target, Reference: inv, Outcome: o}
	if rec, ok := Recommend(o); ok {
		r.Recommendation = &rec
	}
	return r
}

// Text renders the human-readable report.
func (r Report) Text() string {
	var sb strings.Builder
	switch {
	case r.Outcome.Aborted != "":
		fmt.Fprintf(&sb, "ABORTED: %s\n\n", r.Outcome.Aborted)
	case r.Outcome.BudgetLimited != "":
		fmt.Fprintf(&sb, "BUDGET LIMITED: %s\n\n", r.Outcome.BudgetLimited)
	}
	fmt.Fprintf(&sb, "Target %s\n\nReference walk: %d OIDs in %d subtrees, %d PDUs, %s", r.Target,
		r.Reference.Total(), len(r.Reference.Subtrees), r.Reference.PDUs, r.Reference.Duration.Round(time.Millisecond))
	if r.Reference.Partial {
		sb.WriteString(" (partial, budget hit)")
	}
	sb.WriteString("\n")
	for _, st := range r.Reference.Subtrees {
		kind := "walk"
		if st.IsTable() {
			kind = fmt.Sprintf("table, %d columns", len(st.Columns))
		}
		fmt.Fprintf(&sb, "  %s: %d OIDs, %.0f B/varbind (%s)\n", st.Root, st.Count(), st.MeanBytesPerVarbind(), kind)
		for _, sk := range st.Skipped {
			fmt.Fprintf(&sb, "    warning: agent misordered %s, rest of it skipped\n", sk)
		}
	}
	sb.WriteString("\nTrials\n")
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  phase\tR\tV\tvarbinds/s\tp50 ms\tp99 ms\tresult")
	for _, t := range r.Outcome.Trials {
		if !t.OK() {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t-\t-\t-\tFAILED: %s\n", t.Phase, t.Settings.MaxRepetitions, t.Settings.MaxVarsPerPDU, t.Failure)
			continue
		}
		d, _ := rtts(t)
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%.0f\t%.1f\t%.1f\tok\n", t.Phase, t.Settings.MaxRepetitions, t.Settings.MaxVarsPerPDU,
			t.Score, ms(percentile(d, 50)), ms(percentile(d, 99)))
	}
	_ = tw.Flush()
	if r.Outcome.Note != "" {
		fmt.Fprintf(&sb, "\n%s\n", r.Outcome.Note)
	}
	switch {
	case r.Outcome.Stressed && allRequested(r.Outcome):
		sb.WriteString("\nCanary showed agent stress between repeats; requested settings were still measured.\n")
	case r.Outcome.Stressed:
		sb.WriteString("\nCanary showed agent stress; escalation was stopped early.\n")
	}
	sb.WriteString("\nRecommendation\n")
	if r.Recommendation == nil {
		sb.WriteString("  none: no trial completed without errors\n")
		return sb.String()
	}
	rec := r.Recommendation
	fmt.Fprintf(&sb, "  max-repetitions  %d\n  max-vars-per-pdu %d\n  timeout          %d ms  (3 x p99 %.1f ms, rounded up, floor 500 ms)\n  retry            %d\n",
		rec.Settings.MaxRepetitions, rec.Settings.MaxVarsPerPDU, rec.TimeoutMs, rec.P99ms, rec.Retry)
	fmt.Fprintf(&sb, "  observed peak    %s\n  why              %s\n", rec.Peak, rec.Reason)
	return sb.String()
}

// OpenNMS renders a definition element for snmp-config.xml.
func (r Report) OpenNMS() string {
	if r.Recommendation == nil {
		return "<!-- snmptune: no error-free trial, nothing to recommend -->\n"
	}
	rec := r.Recommendation
	return fmt.Sprintf("<definition version=\"v2c\" max-repetitions=\"%d\" max-vars-per-pdu=\"%d\" timeout=\"%d\" retry=\"%d\">\n    <specific>%s</specific>\n</definition>\n",
		rec.Settings.MaxRepetitions, rec.Settings.MaxVarsPerPDU, rec.TimeoutMs, rec.Retry, r.Target)
}

type trialView struct {
	Phase   string  `json:"phase"`
	R       uint32  `json:"max_repetitions"`
	V       int     `json:"max_vars_per_pdu"`
	Score   float64 `json:"varbinds_per_second"`
	P50ms   float64 `json:"p50_ms"`
	P99ms   float64 `json:"p99_ms"`
	Repeats int     `json:"repeats"`
	Failure string  `json:"failure,omitempty"`
}

type subtreeView struct {
	Root    string   `json:"root"`
	OIDs    int      `json:"oids"`
	Bytes   int      `json:"bytes"`
	PerVB   float64  `json:"bytes_per_varbind"`
	Columns int      `json:"columns"`
	Partial bool     `json:"partial"`
	Skipped []string `json:"skipped"`
}

type view struct {
	Target         string          `json:"target"`
	Reference      []subtreeView   `json:"reference"`
	ReferencePDUs  int             `json:"reference_pdus"`
	Trials         []trialView     `json:"trials"`
	Recommendation *Recommendation `json:"recommendation"`
	Aborted        string          `json:"aborted"`
	BudgetLimited  string          `json:"budget_limited"`
	Stressed       bool            `json:"stressed"`
	Note           string          `json:"note"`
}

// JSON renders the report as one JSON document without per-PDU detail.
func (r Report) JSON() ([]byte, error) {
	v := view{Target: r.Target, ReferencePDUs: r.Reference.PDUs, Recommendation: r.Recommendation,
		Aborted: r.Outcome.Aborted, BudgetLimited: r.Outcome.BudgetLimited, Stressed: r.Outcome.Stressed, Note: r.Outcome.Note,
		Reference: []subtreeView{}, Trials: []trialView{}}
	for _, st := range r.Reference.Subtrees {
		v.Reference = append(v.Reference, subtreeView{Root: st.Root, OIDs: st.Count(), Bytes: st.Bytes, PerVB: st.MeanBytesPerVarbind(), Columns: len(st.Columns), Partial: st.Partial, Skipped: append([]string{}, st.Skipped...)})
	}
	for _, t := range r.Outcome.Trials {
		d, _ := rtts(t)
		v.Trials = append(v.Trials, trialView{Phase: t.Phase, R: t.Settings.MaxRepetitions, V: t.Settings.MaxVarsPerPDU,
			Score: t.Score, P50ms: ms(percentile(d, 50)), P99ms: ms(percentile(d, 99)), Repeats: len(t.Runs), Failure: t.Failure})
	}
	return json.MarshalIndent(v, "", "  ")
}
