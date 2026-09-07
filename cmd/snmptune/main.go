/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Command snmptune measures one SNMP v2c agent and recommends the
// max-repetitions, max-vars-per-pdu, timeout and retry values for OpenNMS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/oid"
	"github.com/no42-org/snmptune/internal/report"
	"github.com/no42-org/snmptune/internal/safety"
	"github.com/no42-org/snmptune/internal/search"
	"github.com/no42-org/snmptune/internal/walk"
	"github.com/no42-org/snmptune/internal/workload"
)

// isTerminal reports whether w is a character device such as a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// version is set at build time from the git tag (see the Makefile).
var version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitAborted = 1
	exitUsage   = 2
)

type dialFunc func(agent.Config) (agent.Transport, func(), error)

func dialGoSNMP(cfg agent.Config) (agent.Transport, func(), error) {
	tr, err := agent.NewGoSNMP(cfg)
	if err != nil {
		return nil, nil, err
	}
	return tr, tr.Close, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type options struct {
	target, community, group, referenceTree, format, pduLog, color string
	port, retries, mtu                                             int
	timeout                                                        time.Duration
	oids, tries                                                    multiFlag
	settings                                                       []walk.Settings
	budget                                                         safety.Budget
	search                                                         search.Options
	dryRun, jsonOut, showVersion                                   bool
}

func parse(args []string, errw io.Writer) (options, error) {
	var o options
	o.budget, o.search = safety.Defaults(), search.DefaultOptions()
	fs := flag.NewFlagSet("snmptune", flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.StringVar(&o.target, "target", "", "agent address (required)")
	fs.IntVar(&o.port, "port", 161, "agent UDP port")
	fs.StringVar(&o.community, "community", "public", "SNMP v2c community")
	fs.DurationVar(&o.timeout, "timeout", 3*time.Second, "per-request timeout used while measuring")
	fs.IntVar(&o.retries, "retries", 1, "per-request retries used while measuring")
	fs.Var(&o.oids, "oid", "subtree to query (repeatable); 1.3.6.1 opts in to the whole tree")
	fs.StringVar(&o.group, "group", "", "OpenNMS datacollection XML file, optionally :group-name")
	fs.StringVar(&o.referenceTree, "reference-tree", "", "additional subtree for the reference inventory only")
	fs.Var(&o.tries, "try", "measure this R:V setting instead of searching (repeatable, e.g. --try 4:10)")
	fs.DurationVar(&o.budget.MaxDuration, "max-duration", o.budget.MaxDuration, "stop the run after this long")
	fs.IntVar(&o.budget.MaxTrials, "max-trials", o.budget.MaxTrials, "stop after this many trials")
	fs.IntVar(&o.budget.MaxProduct, "max-product", o.budget.MaxProduct, "never request more than R x V varbinds per PDU")
	fs.IntVar(&o.budget.MaxOIDs, "max-oids", o.budget.MaxOIDs, "stop the reference walk after this many OIDs")
	fs.IntVar(&o.search.Repeats, "repeats", o.search.Repeats, "repeats per trial")
	fs.DurationVar(&o.search.Cooldown, "cooldown", o.search.Cooldown, "minimum pause between runs")
	fs.Float64Var(&o.search.Tolerance, "tolerance", o.search.Tolerance, "percent of reference OIDs a trial may miss")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the plan and send nothing")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&o.jsonOut, "json", false, "print the report as JSON")
	fs.StringVar(&o.format, "format", "text", "text or opennms (snmp-config.xml definition)")
	fs.StringVar(&o.pduLog, "pdu-log", "", "append per-PDU telemetry as JSON lines to this file")
	fs.IntVar(&o.mtu, "mtu", report.DefaultMTU, "path MTU; responses above MTU-28 bytes are marked as IP-fragmented")
	fs.StringVar(&o.color, "color", "auto", "colour the text report: auto, always or never")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.showVersion {
		return o, nil
	}
	if o.target == "" {
		return o, errors.New("--target is required")
	}
	if len(o.oids) == 0 && o.group == "" {
		return o, errors.New("give at least one --oid or a --group file")
	}
	if o.format != "text" && o.format != "opennms" {
		return o, fmt.Errorf("unknown --format %q", o.format)
	}
	if o.search.Repeats < 1 {
		return o, errors.New("--repeats must be at least 1")
	}
	if o.color != "auto" && o.color != "always" && o.color != "never" {
		return o, fmt.Errorf("--color %q: want auto, always or never", o.color)
	}
	if o.mtu < 576 {
		return o, fmt.Errorf("--mtu %d is below the IPv4 minimum of 576", o.mtu)
	}
	if o.port < 1 || o.port > 65535 {
		return o, fmt.Errorf("--port %d is out of range 1-65535", o.port)
	}
	for _, t := range o.tries {
		var s walk.Settings
		if n, err := fmt.Sscanf(t, "%d:%d", &s.MaxRepetitions, &s.MaxVarsPerPDU); n != 2 || err != nil || s.MaxRepetitions < 1 || s.MaxVarsPerPDU < 1 {
			return o, fmt.Errorf("--try %q: want R:V with both at least 1, e.g. 4:10", t)
		}
		o.settings = append(o.settings, s)
	}
	return o, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, dialGoSNMP))
}

func run(ctx context.Context, args []string, out, errw io.Writer, dial dialFunc) int {
	o, err := parse(args, errw)
	if err != nil {
		fmt.Fprintln(errw, "snmptune:", err)
		return exitUsage
	}
	if o.showVersion {
		fmt.Fprintln(out, "snmptune", version)
		return exitOK
	}
	if dial == nil {
		dial = dialGoSNMP
	}

	// Workload before any packet: the plan needs it and errors here are usage errors.
	var w workload.Workload
	for i, r := range o.oids {
		o.oids[i] = oid.Canonical(r) // accept the net-snmp spelling with a leading dot
	}
	o.referenceTree = oid.Canonical(o.referenceTree)
	roots := slices.Clone([]string(o.oids))
	if o.group != "" {
		file, name := workload.SplitGroupArg(o.group)
		w, err = workload.FromDatacollection(file, name)
		if err != nil {
			fmt.Fprintln(errw, "snmptune:", err)
			return exitUsage
		}
		roots = append(roots, w.Roots()...)
	}
	if workload.IsWholeTree(roots) {
		fmt.Fprintln(errw, "warning: walking the whole tree (1.3.6.1); on large devices this takes long and loads the agent. Budgets apply.")
	}
	if o.dryRun {
		plan := w
		for _, r := range o.oids {
			plan.Tables = append(plan.Tables, workload.Table{Base: r, Columns: []string{r}})
		}
		fmt.Fprint(out, search.Plan(plan, o.budget, o.search))
		if len(o.oids) > 0 {
			fmt.Fprintln(out, "Column counts of --oid subtrees are known only after the reference walk; V starts at min(10, widest table).")
		}
		return exitOK
	}

	var logger walk.Logger
	if o.pduLog != "" {
		f, err := os.OpenFile(o.pduLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(errw, "snmptune:", err)
			return exitUsage
		}
		defer func() { _ = f.Close() }()
		logger = walk.NewJSONLines(f)
	}
	if o.budget.MaxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.budget.MaxDuration)
		defer cancel()
	}

	tr, closeTr, err := dial(agent.Config{Target: o.target, Port: o.port, Community: o.community, Timeout: o.timeout, Retries: o.retries})
	if err != nil {
		fmt.Fprintln(errw, "snmptune:", err)
		return exitAborted
	}
	defer closeTr()

	canary, err := safety.NewCanary(ctx, tr)
	if err != nil {
		fmt.Fprintf(errw, "snmptune: preflight failed for %s: %v\n", o.target, err)
		return exitAborted
	}
	fmt.Fprintf(errw, "canary baseline %.1f ms\n", float64(canary.Baseline.Microseconds())/1000)

	tracker := safety.NewTracker(o.budget)
	tracker.Start()
	refBudget := inventory.Budget{MaxOIDs: o.budget.MaxOIDs}
	ref, err := inventory.Walk(ctx, tr, roots, refBudget)
	if err != nil {
		fmt.Fprintln(errw, "snmptune: reference walk:", err)
		return exitAborted
	}
	fmt.Fprintf(errw, "reference walk: %d OIDs, %d PDUs, %s\n", ref.Total(), ref.PDUs, ref.Duration.Round(time.Millisecond))

	// Trials only run against subtrees the reference walk finished; a partial
	// subtree has no ground truth and no bound.
	complete := inventory.Inventory{PDUs: ref.PDUs, Duration: ref.Duration}
	partial := map[string]bool{}
	for _, st := range ref.Subtrees {
		for _, sk := range st.Skipped {
			fmt.Fprintf(errw, "warning: agent misordered %s (OID not increasing), rest of it skipped\n", sk)
		}
		if st.Partial {
			partial[st.Root] = true
			fmt.Fprintf(errw, "warning: dropped %s from the trials, the reference walk hit --max-oids or --max-duration before finishing it\n", st.Root)
			continue
		}
		complete.Subtrees = append(complete.Subtrees, st)
	}
	if len(o.oids) > 0 {
		var fromOIDs inventory.Inventory
		for _, st := range complete.Subtrees {
			if slices.Contains(roots[:len(o.oids)], st.Root) {
				fromOIDs.Subtrees = append(fromOIDs.Subtrees, st)
			}
		}
		w.Tables = append(workload.FromInventory(fromOIDs).Tables, w.Tables...)
	}
	w.Tables = slices.DeleteFunc(w.Tables, func(t workload.Table) bool { return partial[t.Base] })
	if len(w.Tables) == 0 && len(w.Scalars) == 0 {
		fmt.Fprintln(errw, "snmptune: the reference walk hit --max-oids or --max-duration before completing any subtree; raise the budget or narrow --oid")
		return exitAborted
	}
	shown := ref
	if o.referenceTree != "" {
		wide, err := inventory.Walk(ctx, tr, []string{o.referenceTree}, refBudget)
		if err != nil {
			fmt.Fprintln(errw, "snmptune: reference tree walk:", err)
			return exitAborted
		}
		shown.Subtrees = append(shown.Subtrees, wide.Subtrees...)
		shown.PDUs += wide.PDUs
		shown.Duration += wide.Duration
		shown.Partial = shown.Partial || wide.Partial
	}

	runner := &search.Runner{
		Transport: tr, Workload: w, Reference: complete, Canary: canary, Budget: tracker,
		Logger: logger, Opts: o.search,
		Progress: func(line string) { fmt.Fprintln(errw, line) },
	}
	var outcome search.Outcome
	if len(o.settings) > 0 {
		outcome = runner.Measure(ctx, o.settings)
	} else {
		outcome = runner.Run(ctx)
	}
	rep := report.Build(o.target, shown, outcome, report.WithMTU(o.mtu))
	rep.Color = o.color == "always" || o.color == "auto" && isTerminal(out)

	switch {
	case o.jsonOut:
		raw, err := rep.JSON()
		if err != nil {
			fmt.Fprintln(errw, "snmptune:", err)
			return exitAborted
		}
		fmt.Fprintln(out, string(raw))
	case o.format == "opennms":
		fmt.Fprint(out, rep.OpenNMS())
	default:
		fmt.Fprint(out, rep.Text())
	}
	if outcome.Aborted != "" || rep.Recommendation == nil {
		return exitAborted
	}
	return exitOK
}
