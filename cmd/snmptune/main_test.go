/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/agent/sim"
)

func simDial(a *sim.Agent) dialFunc {
	return func(agent.Config) (agent.Transport, func(), error) { return a, func() {}, nil }
}

func TestUsageErrorWithoutTarget(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--oid", "1.3.6.1.2.1.1"}, &out, &errw, nil)
	if code != 2 || !strings.Contains(errw.String(), "--target") {
		t.Fatalf("want exit 2 mentioning --target, got %d %q", code, errw.String())
	}
}

func TestUsageErrorWithoutWorkload(t *testing.T) {
	var out, errw bytes.Buffer
	if code := run(context.Background(), []string{"--target", "192.0.2.1"}, &out, &errw, nil); code != 2 {
		t.Fatalf("want exit 2, got %d %q", code, errw.String())
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	var out, errw bytes.Buffer
	dial := func(agent.Config) (agent.Transport, func(), error) {
		t.Fatal("dry run must not open a transport")
		return nil, nil, nil
	}
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--dry-run"}, &out, &errw, dial)
	if code != 0 || !strings.Contains(out.String(), "no packets sent") || !strings.Contains(out.String(), "1.3.6.1.2.1.2.2") {
		t.Fatalf("want plan, got %d\n%s%s", code, out.String(), errw.String())
	}
}

func TestWholeTreeWarns(t *testing.T) {
	var out, errw bytes.Buffer
	run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1", "--dry-run"}, &out, &errw, nil)
	if !strings.Contains(strings.ToLower(errw.String()), "whole tree") {
		t.Fatalf("want whole-tree warning on stderr, got %q", errw.String())
	}
}

func TestFullRunAgainstSimulatedAgent(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 5, 30)
	a.RTT = func(n int) time.Duration { return time.Millisecond + time.Duration(n)*20*time.Microsecond }
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--repeats", "1", "--cooldown", "0", "--json"}, &out, &errw, simDial(a))
	if code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, errw.String())
	}
	var v struct {
		Recommendation *struct {
			Settings  struct{ MaxRepetitions uint32 }
			TimeoutMs int `json:"timeout_ms"`
		} `json:"recommendation"`
		Trials []any `json:"trials"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not json: %v\n%s", err, out.String())
	}
	if v.Recommendation == nil || v.Recommendation.TimeoutMs < 500 || len(v.Trials) < 3 {
		t.Fatalf("unexpected result: %s", out.String())
	}
	if !strings.Contains(errw.String(), "escalate") {
		t.Fatalf("progress lines expected on stderr, got %q", errw.String())
	}
}

func TestOpenNMSFormat(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 2, 10)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--repeats", "1", "--cooldown", "0", "--format", "opennms"}, &out, &errw, simDial(a))
	if code != 0 || !strings.Contains(out.String(), "<specific>192.0.2.1</specific>") {
		t.Fatalf("want definition snippet, got %d\n%s%s", code, out.String(), errw.String())
	}
}

func TestAbortedRunExitsOne(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 200)
	a.RestartAfter = 40
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 1 || !strings.Contains(out.String(), "ABORTED") {
		t.Fatalf("want exit 1 with ABORTED report, got %d\n%s", code, out.String())
	}
}

func TestGroupWorkloadAndPDULog(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.31.1.1", 10, 4)
	a.AddTable("1.3.6.1.2.1.2.2", 14, 4)
	a.Set("1.3.6.1.2.1.6.5.0", 1)
	a.Set("1.3.6.1.2.1.6.6.0", 2)
	logFile := t.TempDir() + "/pdu.jsonl"
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--group", "../../internal/workload/testdata/mib2.xml", "--repeats", "1", "--cooldown", "0", "--pdu-log", logFile}, &out, &errw, simDial(a))
	if code != 0 {
		t.Fatalf("want exit 0, got %d\n%s", code, errw.String())
	}
	if !strings.Contains(out.String(), "1.3.6.1.2.1.31.1.1") {
		t.Fatalf("report must list the group's tables:\n%s", out.String())
	}
	if !fileHasLines(t, logFile) {
		t.Fatal("pdu log must have lines")
	}
}

func fileHasLines(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n") > 0
}

func TestGroupRunProducesRecommendation(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.31.1.1", 10, 4)
	a.AddTable("1.3.6.1.2.1.2.2", 14, 4)
	a.Set("1.3.6.1.2.1.6.5.0", 1)
	a.Set("1.3.6.1.2.1.6.6.0", 2)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--group", "../../internal/workload/testdata/mib2.xml", "--repeats", "1", "--cooldown", "0", "--json"}, &out, &errw, simDial(a))
	if code != 0 || !strings.Contains(out.String(), `"settings"`) || strings.Contains(out.String(), `"recommendation": null`) {
		t.Fatalf("group run must recommend: exit %d\n%s\n%s", code, out.String(), errw.String())
	}
}

func TestPartialSubtreesAreDroppedFromTrials(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 100)
	a.AddTable("1.3.6.1.2.1.4.22", 1, 10)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--oid", "1.3.6.1.2.1.4.22", "--max-oids", "50", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 1 || !strings.Contains(errw.String(), "max-oids") {
		t.Fatalf("no complete subtree means no trial and a clear error: exit %d\n%s", code, errw.String())
	}
	out.Reset()
	errw.Reset()
	code = run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.4.22", "--oid", "1.3.6.1.2.1.2.2", "--max-oids", "50", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 0 || !strings.Contains(errw.String(), "1.3.6.1.2.1.2.2") || !strings.Contains(errw.String(), "dropped") {
		t.Fatalf("partial ifTable must be dropped with a warning, exit %d\n%s", code, errw.String())
	}
}

func TestNoRecommendationExitsOne(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 1, 20)
	a.ErrorAbove = 1
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 1 {
		t.Fatalf("no clean trial is not success: exit %d\n%s", code, out.String())
	}
}

func TestBadPortAndPDULogAreUsageErrorsBeforeAnyPacket(t *testing.T) {
	var out, errw bytes.Buffer
	dial := func(agent.Config) (agent.Transport, func(), error) {
		t.Fatal("must fail before dialing")
		return nil, nil, nil
	}
	if code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.1", "--port", "70000"}, &out, &errw, dial); code != 2 {
		t.Fatalf("port out of range: exit %d %s", code, errw.String())
	}
	if code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.1", "--pdu-log", "/nonexistent/dir/x.jsonl"}, &out, &errw, dial); code != 2 {
		t.Fatalf("unwritable pdu log: exit %d %s", code, errw.String())
	}
}

func TestSkippedColumnWarnsOnStderr(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 2, 3)
	a.Misorder = map[string]string{"1.3.6.1.2.1.2.2.1.1.2": "1.3.6.1.2.1.2.2.1.1.1"}
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.2.2", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 0 || !strings.Contains(errw.String(), "misorder") || !strings.Contains(errw.String(), "1.3.6.1.2.1.2.2.1.1") {
		t.Fatalf("want exit 0 and a warning naming the column, got %d\n%s", code, errw.String())
	}
}

func TestLeadingDotOIDsAreAccepted(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.2.2", 2, 5)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", ".1.3.6.1.2.1.2.2", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 0 || !strings.Contains(out.String(), "max-repetitions") {
		t.Fatalf("a leading dot is the net-snmp spelling and must work: exit %d\n%s", code, errw.String())
	}
}

func TestTryFlagMeasuresGivenSettings(t *testing.T) {
	a := sim.New()
	a.AddTable("1.3.6.1.2.1.31.1.1", 19, 4)
	a.RTT = func(n int) time.Duration { return time.Millisecond + time.Duration(n)*20*time.Microsecond }
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.31.1.1", "--try", "4:10", "--try", "2:20", "--repeats", "1", "--cooldown", "0"}, &out, &errw, simDial(a))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errw.String())
	}
	text := out.String()
	if !strings.Contains(text, "try") || strings.Contains(text, "escalate") {
		t.Fatalf("try mode must skip the search:\n%s", text)
	}
	for _, want := range []string{`try\s+4\s+10\s`, `try\s+2\s+20\s`} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Fatalf("trial table missing %q:\n%s", want, text)
		}
	}
	if code := run(context.Background(), []string{"--target", "192.0.2.1", "--oid", "1.3.6.1.2.1.31.1.1", "--try", "4x10"}, &out, &errw, nil); code != 2 {
		t.Fatalf("malformed --try is a usage error, got %d", code)
	}
}
