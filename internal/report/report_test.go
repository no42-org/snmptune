/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/inventory"
	"github.com/no42-org/snmptune/internal/search"
	"github.com/no42-org/snmptune/internal/walk"
)

func trial(r uint32, v int, score float64, failure string, rtts ...time.Duration) search.Trial {
	t := search.Trial{Settings: walk.Settings{MaxRepetitions: r, MaxVarsPerPDU: v}, Phase: "escalate", Score: score, Failure: failure}
	var pdus []walk.PDU
	for _, d := range rtts {
		pdus = append(pdus, walk.PDU{RTT: d})
	}
	t.Runs = []walk.Result{{PDUs: pdus, Varbinds: 10}}
	return t
}

func TestRecommendationStepsBackFromPeak(t *testing.T) {
	o := search.Outcome{Trials: []search.Trial{
		trial(2, 10, 100, "", time.Millisecond),
		trial(32, 10, 900, "", time.Millisecond),
		trial(48, 10, 950, "", time.Millisecond),
		trial(64, 10, 0, "2 truncated responses"),
	}}
	rec, ok := Recommend(o)
	if !ok || rec.Settings != (walk.Settings{MaxRepetitions: 32, MaxVarsPerPDU: 10}) || rec.Peak != (walk.Settings{MaxRepetitions: 48, MaxVarsPerPDU: 10}) {
		t.Fatalf("want R32 recommended below peak R48, got %+v", rec)
	}
}

func TestRecommendationWhenPeakIsDefault(t *testing.T) {
	o := search.Outcome{Trials: []search.Trial{
		trial(2, 10, 100, "", time.Millisecond),
		trial(4, 10, 0, "timeout"),
	}}
	rec, ok := Recommend(o)
	if !ok || rec.Settings != search.Start || !strings.Contains(rec.Reason, "did not sustain") {
		t.Fatalf("want default with reason, got %+v", rec)
	}
}

func TestNoRecommendationWithoutCleanTrial(t *testing.T) {
	o := search.Outcome{Trials: []search.Trial{trial(2, 10, 0, "timeout")}}
	if _, ok := Recommend(o); ok {
		t.Fatal("no clean trial means no recommendation")
	}
}

func TestTimeoutSuggestion(t *testing.T) {
	if got := SuggestTimeout(40 * time.Millisecond); got != 500 {
		t.Fatalf("40ms p99 -> 500ms floor, got %d", got)
	}
	if got := SuggestTimeout(730 * time.Millisecond); got != 2200 {
		t.Fatalf("730ms p99 -> 2200ms, got %d", got)
	}
}

func TestP99AndRetryFromRecommendedTrial(t *testing.T) {
	var rtts []time.Duration
	for i := 1; i <= 100; i++ {
		rtts = append(rtts, time.Duration(i)*time.Millisecond)
	}
	good := trial(2, 10, 100, "", rtts...)
	good.Runs[0].PDUs[3].Retries = 1
	o := search.Outcome{Trials: []search.Trial{good, trial(4, 10, 0, "timeout")}}
	rec, _ := Recommend(o)
	// PDU 4 (4 ms) was retried and is excluded, so p99 of the remaining 99 is 100 ms.
	if rec.P99 != 100*time.Millisecond || rec.TimeoutMs != 500 || rec.Retry != 2 {
		t.Fatalf("want p99 100ms timeout 500 (floor) retry 2, got %+v", rec)
	}
}

func fullReport() Report {
	o := search.Outcome{Trials: []search.Trial{
		trial(2, 10, 100, "", time.Millisecond, 2*time.Millisecond),
		trial(32, 10, 900, "", 3*time.Millisecond, 4*time.Millisecond),
		trial(64, 10, 0, "2 truncated responses"),
	}}
	inv := inventory.Inventory{Subtrees: []inventory.Subtree{{Root: "1.3.6.1.2.1.2.2", OIDs: []string{"a", "b"}, Bytes: 100, Columns: []uint32{1}}}}
	return Build("192.0.2.1", inv, o)
}

func TestTextTrialTableShowsFailureWithoutScore(t *testing.T) {
	text := fullReport().Text()
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "64") && strings.Contains(l, "truncated") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("failed trial row missing:\n%s", text)
	}
	if strings.Contains(line, "varbinds/s") || strings.Contains(line, " 0.0 ") {
		t.Fatalf("failed row must not carry a score: %q", line)
	}
	if !strings.Contains(text, "max-repetitions") || !strings.Contains(text, "peak") {
		t.Fatalf("recommendation section missing:\n%s", text)
	}
}

func TestAbortIsFirstLine(t *testing.T) {
	r := fullReport()
	r.Outcome.Aborted = "agent restarted after trial R=64 V=10"
	first := strings.SplitN(r.Text(), "\n", 2)[0]
	if !strings.Contains(first, "restarted") || !strings.Contains(first, "R=64 V=10") {
		t.Fatalf("abort must lead the report: %q", first)
	}
	r.Outcome.Aborted = ""
	r.Outcome.BudgetLimited = "max-trials 40 reached"
	if first := strings.SplitN(r.Text(), "\n", 2)[0]; !strings.Contains(first, "max-trials") {
		t.Fatalf("budget limit must lead the report: %q", first)
	}
}

func TestOpenNMSSnippet(t *testing.T) {
	got := fullReport().OpenNMS()
	for _, want := range []string{`<definition`, `version="v2c"`, `max-repetitions="2"`, `max-vars-per-pdu="10"`, `timeout="500"`, `retry="1"`, `<specific>192.0.2.1</specific>`, `</definition>`} {
		if !strings.Contains(got, want) {
			t.Fatalf("snippet missing %s:\n%s", want, got)
		}
	}
}

func TestJSONShape(t *testing.T) {
	raw, err := fullReport().JSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"target", "reference", "trials", "recommendation", "aborted", "budget_limited"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("json missing %q: %s", k, raw)
		}
	}
	trials := m["trials"].([]any)
	if len(trials) != 3 {
		t.Fatalf("want 3 trials, got %d", len(trials))
	}
	if _, ok := trials[0].(map[string]any)["pdus"]; ok {
		t.Fatal("json must not dump every PDU; that is the pdu log's job")
	}
}

func TestRetriedPDUsExcludedFromPercentiles(t *testing.T) {
	good := trial(2, 10, 100, "", 10*time.Millisecond, 12*time.Millisecond, 3*time.Second)
	good.Runs[0].PDUs[2].Retries = 1
	rec, _ := Recommend(search.Outcome{Trials: []search.Trial{good}})
	if rec.P99 != 12*time.Millisecond || rec.TimeoutMs != 500 {
		t.Fatalf("retried PDU must not drive p99: %+v", rec)
	}
}

func TestSkippedColumnsReported(t *testing.T) {
	r := fullReport()
	r.Reference.Subtrees[0].Skipped = []string{"1.3.6.1.4.1.52642.1.1.10.2.5.1.2.4.1.1"}
	text := r.Text()
	if !strings.Contains(text, "misorder") || !strings.Contains(text, "52642.1.1.10.2.5.1.2.4.1.1") {
		t.Fatalf("text must name the skipped column:\n%s", text)
	}
	raw, _ := r.JSON()
	if !strings.Contains(string(raw), `"skipped"`) || !strings.Contains(string(raw), "52642.1.1.10.2.5.1.2.4.1.1") {
		t.Fatalf("json must carry skipped columns: %s", raw)
	}
}

func TestTryModeRecommendsTheBestRequestedSetting(t *testing.T) {
	a := trial(4, 10, 2253, "", time.Millisecond)
	b := trial(2, 20, 1900, "", time.Millisecond)
	a.Phase, b.Phase = "try", "try"
	rec, ok := Recommend(search.Outcome{Trials: []search.Trial{a, b}})
	if !ok || rec.Settings != a.Settings || !strings.Contains(rec.Reason, "requested") {
		t.Fatalf("try mode compares what was asked for, no headroom step: %+v", rec)
	}
}
