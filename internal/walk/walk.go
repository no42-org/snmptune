/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package walk queries a workload the way the OpenNMS collector does and
// records telemetry for every PDU. It sends one request at a time; there is
// no concurrency toward the agent and none is planned.
package walk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/oid"
	"github.com/no42-org/snmptune/internal/workload"
)

// Settings are the two knobs under test.
type Settings struct {
	MaxRepetitions uint32
	MaxVarsPerPDU  int
}

// Product is the varbinds requested per GetBulk, the quantity that limits an agent.
func (s Settings) Product() int { return int(s.MaxRepetitions) * s.MaxVarsPerPDU }

func (s Settings) String() string {
	return fmt.Sprintf("R=%d V=%d", s.MaxRepetitions, s.MaxVarsPerPDU)
}

// PDU is the telemetry of one request.
type PDU struct {
	Seq       int           `json:"seq"`
	Sent      time.Time     `json:"sent"`
	Kind      string        `json:"kind"`
	Repeaters int           `json:"repeaters"`
	MaxReps   uint32        `json:"max_repetitions"`
	Requested int           `json:"requested"` // varbinds requested
	Rows      int           `json:"rows"`      // complete rows returned
	Varbinds  int           `json:"varbinds"`  // varbinds returned
	Bytes     int           `json:"bytes"`
	RTT       time.Duration `json:"-"`
	RTTms     float64       `json:"rtt_ms"`
	Retries   int           `json:"retries"`
	Truncated bool          `json:"truncated"`
	Error     string        `json:"error"`
}

// Result is one trial run of a workload at fixed settings.
type Result struct {
	Settings    Settings
	PDUs        []PDU
	OIDs        []string // in-scope OIDs collected from tables
	Varbinds    int      // in-scope varbinds, tables plus scalars
	Duration    time.Duration
	Skips       []string // prefixes skipped because the agent misordered them
	Timeouts    int
	AgentErrors int
	Truncations int
	Retries     int
	Failure     string // empty when the walk was clean
}

// AgentTime is the sum of all round-trip times: the time the agent and the
// network spent on this walk, independent of client-side processing.
func (r Result) AgentTime() time.Duration {
	var d time.Duration
	for _, p := range r.PDUs {
		d += p.RTT
	}
	return d
}

// VarbindsPerSecond is the trial's throughput over agent time.
func (r Result) VarbindsPerSecond() float64 {
	d := r.AgentTime()
	if d <= 0 {
		return 0
	}
	return float64(r.Varbinds) / d.Seconds()
}

// Logger receives every PDU as it completes.
type Logger interface{ Log(PDU) }

// JSONLines writes one JSON object per PDU.
type JSONLines struct{ w io.Writer }

// NewJSONLines returns a Logger writing to w.
func NewJSONLines(w io.Writer) *JSONLines { return &JSONLines{w: w} }

// Log implements Logger.
func (l *JSONLines) Log(p PDU) {
	b, _ := json.Marshal(p)
	_, _ = l.w.Write(append(b, '\n'))
}

type column struct {
	oid    string
	cursor string
	jumped bool // skipped within the current response; ignore its remaining rows
}

type walker struct {
	ctx context.Context
	tr  agent.Transport
	log Logger
	res Result
}

// Run walks the workload once at the given settings. A transport error other
// than a timeout aborts with an error; timeouts, truncation and agent
// misbehaviour are recorded in Result.Failure instead.
func Run(ctx context.Context, tr agent.Transport, w workload.Workload, s Settings, log Logger) (Result, error) {
	if s.MaxRepetitions == 0 || s.MaxVarsPerPDU <= 0 {
		return Result{}, fmt.Errorf("invalid settings %s", s)
	}
	wk := &walker{ctx: ctx, tr: tr, log: log, res: Result{Settings: s}}
	start := time.Now()
	var failures []string
	for _, t := range w.Tables {
		for chunk := range slices.Chunk(t.Columns, s.MaxVarsPerPDU) {
			if err := wk.table(chunk, s.MaxRepetitions); err != nil {
				if errors.Is(err, agent.ErrTimeout) || errors.Is(err, agent.ErrAgent) {
					failures = append(failures, err.Error())
					wk.res.Duration = time.Since(start)
					wk.res.Failure = strings.Join(failures, "; ")
					return wk.res, nil
				}
				if errors.Is(err, errWalk) {
					failures = append(failures, err.Error())
					continue
				}
				return wk.res, err
			}
		}
	}
	for chunk := range slices.Chunk(w.Scalars, s.MaxVarsPerPDU) {
		if err := wk.scalars(chunk); err != nil {
			if errors.Is(err, agent.ErrTimeout) || errors.Is(err, agent.ErrAgent) {
				failures = append(failures, err.Error())
				break
			}
			return wk.res, err
		}
	}
	wk.res.Duration = time.Since(start)
	if wk.res.Truncations > 0 {
		failures = append(failures, fmt.Sprintf("%d truncated responses", wk.res.Truncations))
	}
	wk.res.Failure = strings.Join(failures, "; ")
	return wk.res, nil
}

var errWalk = errors.New("walk error")

// table walks one chunk of columns with multi-repeater GetBulk requests until
// every column has left its subtree.
func (wk *walker) table(chunk []string, reps uint32) error {
	active := make([]*column, len(chunk))
	for i, c := range chunk {
		c = oid.Canonical(c)
		active[i] = &column{oid: c, cursor: c}
	}
	for len(active) > 0 {
		if err := wk.ctx.Err(); err != nil {
			return err
		}
		oids := make([]string, len(active))
		for i, c := range active {
			oids[i] = c.cursor
		}
		resp, p, err := wk.send("getbulk", oids, reps, func() (agent.Response, error) {
			return wk.tr.GetBulk(wk.ctx, oids, reps)
		})
		if err != nil {
			return err
		}
		width := len(active)
		n := len(resp.Varbinds)
		rows := (n + width - 1) / width // a partial trailing row counts
		finished := make([]bool, width)
		anyFinished := false
		lastRowAllEnd := n > 0
		for _, c := range active {
			c.jumped = false
		}
		for idx, vb := range resp.Varbinds {
			i, r := idx%width, idx/width
			_, end := vb.Value.(agent.EndOfMibView)
			if r == rows-1 && !end {
				lastRowAllEnd = false
			}
			c := active[i]
			if finished[i] || c.jumped {
				continue
			}
			if end || !oid.HasPrefix(vb.OID, c.oid) {
				finished[i], anyFinished = true, true
				continue
			}
			if oid.Compare(vb.OID, c.cursor) <= 0 {
				// Misordered agent: continue past the shared prefix and
				// ignore the rest of this column in this response.
				target := oid.SkipTarget(c.cursor, vb.OID)
				wk.res.Skips = append(wk.res.Skips, c.oid)
				p.Error = "misordered, skipped"
				c.cursor, c.jumped = target, true
				if !oid.HasPrefix(target, c.oid) {
					finished[i], anyFinished = true, true
				}
				continue
			}
			c.cursor = vb.OID
			wk.res.OIDs = append(wk.res.OIDs, vb.OID)
			wk.res.Varbinds++
		}
		p.Rows = rows
		// An agent may only stop short of R rows once every repeater has
		// reached the end of the MIB view (RFC 3416). Anything else is a cap.
		if (rows < int(reps) || n%width != 0) && !lastRowAllEnd {
			p.Truncated = true
			wk.res.Truncations++
		}
		wk.emit(p)
		if n == 0 && !anyFinished {
			return fmt.Errorf("%w: empty GetBulk response for %v", errWalk, oids)
		}
		next := active[:0]
		for i, c := range active {
			if !finished[i] {
				next = append(next, c)
			}
		}
		active = next
	}
	return nil
}

func (wk *walker) scalars(chunk []string) error {
	resp, p, err := wk.send("get", chunk, 0, func() (agent.Response, error) {
		return wk.tr.Get(wk.ctx, chunk)
	})
	if err != nil {
		return err
	}
	wk.emit(p)
	for _, vb := range resp.Varbinds {
		switch vb.Value.(type) {
		case agent.NoSuchObject, agent.NoSuchInstance, agent.EndOfMibView:
		default:
			wk.res.Varbinds++
		}
	}
	return nil
}

// emit logs a completed PDU record.
func (wk *walker) emit(p *PDU) {
	if wk.log != nil {
		wk.log.Log(*p)
	}
}

// send issues one request and records its telemetry. The returned *PDU
// points into the result so callers can fill in rows and truncation before
// emitting it; failed requests are emitted here.
func (wk *walker) send(kind string, oids []string, reps uint32, do func() (agent.Response, error)) (agent.Response, *PDU, error) {
	p := PDU{Seq: len(wk.res.PDUs) + 1, Sent: time.Now(), Kind: kind, Repeaters: len(oids), MaxReps: reps, Requested: len(oids)}
	if kind == "getbulk" {
		p.Requested = len(oids) * int(reps)
	}
	resp, err := do()
	p.RTT, p.RTTms, p.Bytes, p.Retries, p.Varbinds = resp.RTT, float64(resp.RTT.Microseconds())/1000, resp.Bytes, resp.Retries, len(resp.Varbinds)
	wk.res.Retries += resp.Retries
	if err != nil {
		p.Error = err.Error()
		switch {
		case errors.Is(err, agent.ErrTimeout):
			wk.res.Timeouts++
		case errors.Is(err, agent.ErrAgent):
			wk.res.AgentErrors++
		}
	}
	wk.res.PDUs = append(wk.res.PDUs, p)
	if err != nil {
		wk.emit(&p)
		return resp, nil, err
	}
	return resp, &wk.res.PDUs[len(wk.res.PDUs)-1], nil
}

// Missing counts reference OIDs absent from got. Extra OIDs in got are ignored.
func Missing(ref, got []string) int {
	seen := make(map[string]struct{}, len(got))
	for _, o := range got {
		seen[o] = struct{}{}
	}
	n := 0
	for _, o := range ref {
		if _, ok := seen[o]; !ok {
			n++
		}
	}
	return n
}
