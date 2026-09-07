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
	"time"

	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/search"
	"github.com/no42-org/snmptune/internal/walk"
)

const (
	timeoutFactor  = 3
	timeoutRoundMs = 100
	timeoutFloorMs = 500

	// DefaultMTU is the Ethernet MTU; a UDP payload above MTU minus the IP
	// and UDP headers is sent as IP fragments.
	DefaultMTU  = 1500
	udpOverhead = 28

	red   = "\x1b[31m"
	green = "\x1b[32m"
	reset = "\x1b[0m"
)

// DatagramLimit is the largest SNMP message that fits one datagram at mtu.
func DatagramLimit(mtu int) int { return mtu - udpOverhead }

// Fragments is the number of IP fragments a UDP payload of the given size
// needs at mtu: the payload plus the 8-byte UDP header, split into pieces
// of at most mtu minus the 20-byte IP header.
func Fragments(payload, mtu int) int {
	per := mtu - 20
	return (payload + 8 + per - 1) / per
}

// fillPct is the payload as a percentage of the datagram limit.
func fillPct(payload, limit int) int { return (payload*100 + limit/2) / limit }

// fragmentDetail describes an oversized response.
func fragmentDetail(payload, mtu int) string {
	return fmt.Sprintf("%d fragments, %d B over", Fragments(payload, mtu), payload-DatagramLimit(mtu))
}

// Recommendation is what goes into snmp-config.xml, with the evidence.
type Recommendation struct {
	Settings   walk.Settings `json:"settings"`
	Peak       walk.Settings `json:"peak"`
	Reason     string        `json:"reason"`
	P50        time.Duration `json:"-"`
	P99        time.Duration `json:"-"`
	P50ms      float64       `json:"p50_ms"`
	P99ms      float64       `json:"p99_ms"`
	TimeoutMs  int           `json:"timeout_ms"`
	Retry      int           `json:"retry"`
	MaxBytes   int           `json:"max_bytes"`
	Fragmented bool          `json:"fragmented"`
}

// Recommend is recommend at the default MTU.
func Recommend(o search.Outcome) (Recommendation, bool) {
	return recommend(o, DatagramLimit(DefaultMTU))
}

// maxBytes is the largest response seen in a trial.
func maxBytes(t search.Trial) int {
	n := 0
	for _, run := range t.Runs {
		for _, p := range run.PDUs {
			n = max(n, p.Bytes)
		}
	}
	return n
}

// bestWithin is the clean trial with the best throughput whose responses
// all fit one datagram, or nil.
func bestWithin(o search.Outcome, limit int) *search.Trial {
	var best *search.Trial
	for i := range o.Trials {
		t := &o.Trials[i]
		if t.OK() && maxBytes(*t) <= limit && (best == nil || t.Score > best.Score) {
			best = t
		}
	}
	return best
}

// recommend picks the largest error-free product strictly below the peak,
// or the default when the peak is the default. Settings whose responses
// exceed one datagram are only considered when nothing else passed. ok is
// false when no trial passed at all.
func recommend(o search.Outcome, limit int) (rec Recommendation, ok bool) {
	peak := bestWithin(o, limit)
	if peak == nil {
		peak = o.Peak()
		rec.Fragmented = true
	}
	if peak == nil {
		return rec, false
	}
	chosen := peak
	if allRequested(o) {
		rec.Reason = fmt.Sprintf("best of the requested settings within one datagram of %d B (%.0f varbinds/s); no headroom step in try mode", limit, peak.Score)
	} else if peak.Settings == search.Start {
		rec.Reason = "the agent did not sustain more than the OpenNMS default without errors"
	} else {
		var below *search.Trial
		for i := range o.Trials {
			t := &o.Trials[i]
			if !t.OK() || t.Settings.Product() >= peak.Settings.Product() || maxBytes(*t) > limit {
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
	if rec.Fragmented {
		rec.Reason = fmt.Sprintf("every clean setting exceeds one datagram of %d B; %s. Expect IP fragmentation on this path", limit, rec.Reason)
	}
	rec.Settings, rec.Peak, rec.MaxBytes = chosen.Settings, peak.Settings, maxBytes(*chosen)
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
	MTU            int  // path MTU the datagram limit derives from
	Color          bool // ANSI colours in Text
}

// Option adjusts Build.
type Option func(*Report)

// WithMTU sets the path MTU used for the datagram limit.
func WithMTU(mtu int) Option { return func(r *Report) { r.MTU = mtu } }

// Build assembles the report for one run.
func Build(target string, inv inventory.Inventory, o search.Outcome, opts ...Option) Report {
	r := Report{Target: target, Reference: inv, Outcome: o, MTU: DefaultMTU}
	for _, opt := range opts {
		opt(&r)
	}
	if rec, ok := recommend(o, DatagramLimit(r.MTU)); ok {
		r.Recommendation = &rec
	}
	return r
}

func (r Report) paint(color, s string) string {
	if !r.Color {
		return s
	}
	return color + s + reset
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
	limit := DatagramLimit(r.MTU)
	best := bestWithin(r.Outcome, limit)
	sb.WriteString("\nTrials\n")
	const rowFmt = "  %-8s  %4s  %3s  %11s  %7s  %7s  %6s  %5s  %s"
	fmt.Fprintf(&sb, rowFmt+"\n", "phase", "R", "V", "varbinds/s", "p50 ms", "p99 ms", "max B", "fill", "result")
	for i := range r.Outcome.Trials {
		t := &r.Outcome.Trials[i]
		mb := maxBytes(*t)
		fragmented := mb > limit
		fill := fmt.Sprintf("%d%%", fillPct(mb, limit))
		var line string
		if t.OK() {
			d, _ := rtts(*t)
			mark := "ok"
			switch {
			case fragmented:
				mark = "ok, FRAGMENTED (" + fragmentDetail(mb, r.MTU) + ")"
			case t == best:
				mark = "ok, BEST"
			}
			line = fmt.Sprintf(rowFmt, t.Phase, fmt.Sprint(t.Settings.MaxRepetitions), fmt.Sprint(t.Settings.MaxVarsPerPDU),
				fmt.Sprintf("%.0f", t.Score), fmt.Sprintf("%.1f", ms(percentile(d, 50))), fmt.Sprintf("%.1f", ms(percentile(d, 99))),
				fmt.Sprint(mb), fill, mark)
		} else {
			mark := "FAILED: " + t.Failure
			if fragmented {
				mark = "FAILED, FRAGMENTED (" + fragmentDetail(mb, r.MTU) + "): " + t.Failure
			}
			line = fmt.Sprintf(rowFmt, t.Phase, fmt.Sprint(t.Settings.MaxRepetitions), fmt.Sprint(t.Settings.MaxVarsPerPDU),
				"-", "-", "-", fmt.Sprint(mb), fill, mark)
		}
		// Colour wraps the finished line so it never disturbs the columns.
		switch {
		case fragmented:
			line = r.paint(red, line)
		case t.OK() && t == best:
			line = r.paint(green, line)
		}
		sb.WriteString(line + "\n")
	}
	fmt.Fprintf(&sb, "\n  Datagram limit %d B at MTU %d; fill = largest response as a share of it.\n", limit, r.MTU)
	sb.WriteString("  FRAGMENTED = above the limit, IP-fragmented on the wire. BEST = highest throughput within one datagram.\n")
	var notes []string
	if r.Outcome.Note != "" {
		notes = append(notes, r.Outcome.Note)
	}
	if r.Outcome.Stressed && !strings.Contains(r.Outcome.Note, "stress") {
		if allRequested(r.Outcome) {
			notes = append(notes, "canary showed agent stress between repeats; requested settings were still measured")
		} else {
			notes = append(notes, "canary showed agent stress; escalation was stopped early")
		}
	}
	if len(notes) > 0 {
		sb.WriteString("\nNotes\n")
		for _, n := range notes {
			fmt.Fprintf(&sb, "  %s\n", n)
		}
	}
	sb.WriteString("\nRecommendation\n")
	if r.Recommendation == nil {
		sb.WriteString("  none: no trial completed without errors\n")
		return sb.String()
	}
	rec := r.Recommendation
	fmt.Fprintf(&sb, "  max-repetitions  %d\n  max-vars-per-pdu %d\n  timeout          %d ms  (3 x p99 %.1f ms, rounded up, floor 500 ms)\n  retry            %d\n",
		rec.Settings.MaxRepetitions, rec.Settings.MaxVarsPerPDU, rec.TimeoutMs, rec.P99ms, rec.Retry)
	size := fmt.Sprintf("%d B (%d%% of a %d B datagram)", rec.MaxBytes, fillPct(rec.MaxBytes, limit), limit)
	if rec.MaxBytes > limit {
		size = fmt.Sprintf("%d B (%d%% of a %d B datagram, %s)", rec.MaxBytes, fillPct(rec.MaxBytes, limit), limit, fragmentDetail(rec.MaxBytes, r.MTU))
	}
	fmt.Fprintf(&sb, "  largest response %s\n  observed peak    %s\n  why              %s\n", size, rec.Peak, rec.Reason)
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
	Phase            string  `json:"phase"`
	R                uint32  `json:"max_repetitions"`
	V                int     `json:"max_vars_per_pdu"`
	Score            float64 `json:"varbinds_per_second"`
	P50ms            float64 `json:"p50_ms"`
	P99ms            float64 `json:"p99_ms"`
	Repeats          int     `json:"repeats"`
	Failure          string  `json:"failure,omitempty"`
	MaxBytes         int     `json:"max_bytes"`
	Fragmented       bool    `json:"fragmented"`
	FillPct          int     `json:"datagram_fill_pct"`
	Fragments        int     `json:"fragments"`
	BestUnfragmented bool    `json:"best_unfragmented"`
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
	DatagramLimit  int             `json:"datagram_limit"`
}

// JSON renders the report as one JSON document without per-PDU detail.
func (r Report) JSON() ([]byte, error) {
	v := view{Target: r.Target, ReferencePDUs: r.Reference.PDUs, Recommendation: r.Recommendation,
		Aborted: r.Outcome.Aborted, BudgetLimited: r.Outcome.BudgetLimited, Stressed: r.Outcome.Stressed, Note: r.Outcome.Note,
		Reference: []subtreeView{}, Trials: []trialView{}, DatagramLimit: DatagramLimit(r.MTU)}
	best := bestWithin(r.Outcome, DatagramLimit(r.MTU))
	for _, st := range r.Reference.Subtrees {
		v.Reference = append(v.Reference, subtreeView{Root: st.Root, OIDs: st.Count(), Bytes: st.Bytes, PerVB: st.MeanBytesPerVarbind(), Columns: len(st.Columns), Partial: st.Partial, Skipped: append([]string{}, st.Skipped...)})
	}
	for i := range r.Outcome.Trials {
		t := &r.Outcome.Trials[i]
		d, _ := rtts(*t)
		mb := maxBytes(*t)
		v.Trials = append(v.Trials, trialView{Phase: t.Phase, R: t.Settings.MaxRepetitions, V: t.Settings.MaxVarsPerPDU,
			Score: t.Score, P50ms: ms(percentile(d, 50)), P99ms: ms(percentile(d, 99)), Repeats: len(t.Runs), Failure: t.Failure,
			MaxBytes: mb, FillPct: fillPct(mb, DatagramLimit(r.MTU)), Fragments: Fragments(mb, r.MTU),
			Fragmented: mb > DatagramLimit(r.MTU), BestUnfragmented: t == best})
	}
	return json.MarshalIndent(v, "", "  ")
}
