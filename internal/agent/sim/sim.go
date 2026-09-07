/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package sim is an in-memory SNMP agent model for tests.
// It answers Get, GetNext and GetBulk from a sorted OID table, can truncate
// GetBulk responses by rows or bytes, simulates round-trip time and timeouts
// without sleeping, and can restart itself after a number of requests.
package sim

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/oid"
)

// Request is one logged request.
type Request struct {
	Kind     string // get, getnext, getbulk
	OIDs     []string
	MaxReps  uint32
	Varbinds int // varbinds in the response
}

// Agent is the simulated agent. Configure the exported fields before use.
type Agent struct {
	// MaxRows caps the rows of a GetBulk response. Zero means no cap.
	MaxRows int
	// MaxBytes caps the encoded size of a GetBulk response. Zero means no cap.
	MaxBytes int
	// RTT returns the simulated round-trip time for a response with n varbinds.
	RTT func(n int) time.Duration
	// Timeout is the client's timeout; a response slower than this is lost.
	Timeout time.Duration
	// RestartAfter restarts the agent (sysUpTime resets) after this many
	// requests. Zero disables.
	RestartAfter int
	// LoopOID, when set, makes the agent answer a GetNext or GetBulk step
	// from that OID with the same OID again, like a buggy agent.
	LoopOID string
	// Misorder maps a cursor to the OID the agent answers with instead of
	// the true successor, to simulate an agent that sorts indices as text.
	Misorder map[string]string
	// ErrorAbove makes GetBulk answer with an agent error (tooBig) when more
	// than this many varbinds are requested. Zero disables.
	ErrorAbove int
	// Retries is reported on every response, simulating retransmissions.
	Retries int

	mu       sync.Mutex
	keys     []string
	arcs     [][]uint32
	values   map[string]any
	requests []Request
	uptime   time.Duration
}

// New returns an empty agent that answers instantly and never truncates.
func New() *Agent {
	return &Agent{
		RTT:     func(int) time.Duration { return 0 },
		Timeout: time.Hour,
		values:  map[string]any{},
		uptime:  24 * time.Hour, // a restart drops this to zero, like a real reboot
	}
}

// Set stores a value. The OID table is re-sorted on the next request.
func (a *Agent) Set(o string, v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o = oid.Canonical(o)
	if _, ok := a.values[o]; !ok {
		a.keys = append(a.keys, o)
		a.arcs = nil
	}
	a.values[o] = v
}

// AddTable adds a table under base with the given number of columns and rows,
// using the MIB layout base.1.<column>.<index>.
func (a *Agent) AddTable(base string, cols, rows int) {
	for c := 1; c <= cols; c++ {
		for r := 1; r <= rows; r++ {
			a.Set(fmt.Sprintf("%s.1.%d.%d", oid.Canonical(base), c, r), fmt.Sprintf("c%dr%d", c, r))
		}
	}
}

// Requests returns a copy of the request log.
func (a *Agent) Requests() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Request(nil), a.requests...)
}

// Get implements agent.Transport.
func (a *Agent) Get(ctx context.Context, oids []string) (agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.beginLocked()
	var vbs []agent.Varbind
	for _, o := range oids {
		o = oid.Canonical(o)
		if o == agent.SysUpTimeOID {
			vbs = append(vbs, agent.Varbind{OID: o, Value: uint32(a.uptime / (10 * time.Millisecond))})
			continue
		}
		v, ok := a.values[o]
		if !ok {
			v = agent.NoSuchInstance{}
		}
		vbs = append(vbs, agent.Varbind{OID: o, Value: v})
	}
	return a.finishLocked("get", oids, 0, vbs)
}

// GetNext implements agent.Transport.
func (a *Agent) GetNext(ctx context.Context, oids []string) (agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.beginLocked()
	var vbs []agent.Varbind
	for _, o := range oids {
		vbs = append(vbs, a.nextLocked(oid.Parse(o)))
	}
	return a.finishLocked("getnext", oids, 0, vbs)
}

// GetBulk implements agent.Transport with non-repeaters fixed at zero.
func (a *Agent) GetBulk(ctx context.Context, oids []string, maxRepetitions uint32) (agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.beginLocked()
	if a.ErrorAbove > 0 && len(oids)*int(maxRepetitions) > a.ErrorAbove {
		a.requests = append(a.requests, Request{Kind: "getbulk", OIDs: append([]string(nil), oids...), MaxReps: maxRepetitions})
		return agent.Response{RTT: a.RTT(0), Retries: a.Retries}, fmt.Errorf("%w tooBig at index 0", agent.ErrAgent)
	}
	cursors := make([][]uint32, len(oids))
	for i, o := range oids {
		cursors[i] = oid.Parse(o)
	}
	var vbs []agent.Varbind
	bytes := 0
rows:
	for r := 0; r < int(maxRepetitions); r++ {
		if a.MaxRows > 0 && r >= a.MaxRows {
			break
		}
		for i := range cursors {
			vb := a.nextLocked(cursors[i])
			if a.MaxBytes > 0 && bytes+encodedSize(vb) > a.MaxBytes {
				break rows // a byte-limited agent stops mid-row
			}
			cursors[i] = oid.Parse(vb.OID)
			bytes += encodedSize(vb)
			vbs = append(vbs, vb)
		}
	}
	return a.finishLocked("getbulk", oids, maxRepetitions, vbs)
}

// beginLocked applies a pending restart and sorts the OID table.
func (a *Agent) beginLocked() {
	if a.RestartAfter > 0 && len(a.requests) >= a.RestartAfter {
		a.uptime = 0
		a.RestartAfter = 0
	}
	if a.arcs != nil || len(a.keys) == 0 {
		return
	}
	sort.Slice(a.keys, func(i, j int) bool { return oid.Compare(a.keys[i], a.keys[j]) < 0 })
	a.arcs = make([][]uint32, len(a.keys))
	for i, k := range a.keys {
		a.arcs[i] = oid.Parse(k)
	}
}

// nextLocked returns the first varbind strictly after arcs, or EndOfMibView.
func (a *Agent) nextLocked(arcs []uint32) agent.Varbind {
	if a.LoopOID != "" && oid.CompareArcs(arcs, oid.Parse(a.LoopOID)) == 0 {
		return agent.Varbind{OID: oid.Canonical(a.LoopOID), Value: a.values[oid.Canonical(a.LoopOID)]}
	}
	if wrong, ok := a.Misorder[oid.Format(arcs)]; ok {
		return agent.Varbind{OID: wrong, Value: a.values[wrong]}
	}
	i := sort.Search(len(a.arcs), func(i int) bool { return oid.CompareArcs(a.arcs[i], arcs) > 0 })
	if i == len(a.keys) {
		return agent.Varbind{OID: fmtArcs(arcs), Value: agent.EndOfMibView{}}
	}
	return agent.Varbind{OID: a.keys[i], Value: a.values[a.keys[i]]}
}

func (a *Agent) finishLocked(kind string, oids []string, reps uint32, vbs []agent.Varbind) (agent.Response, error) {
	a.requests = append(a.requests, Request{Kind: kind, OIDs: append([]string(nil), oids...), MaxReps: reps, Varbinds: len(vbs)})
	rtt := a.RTT(len(vbs))
	a.uptime += rtt + time.Millisecond
	if rtt > a.Timeout {
		return agent.Response{Retries: a.Retries}, agent.ErrTimeout
	}
	size := 0
	for _, vb := range vbs {
		size += encodedSize(vb)
	}
	return agent.Response{Varbinds: vbs, RTT: rtt, Bytes: size, Retries: a.Retries}, nil
}

// encodedSize approximates the BER size of one varbind.
func encodedSize(vb agent.Varbind) int {
	n := 4 + len(oid.Parse(vb.OID)) // sequence and OID headers plus one byte per arc
	switch v := vb.Value.(type) {
	case string:
		n += 2 + len(v)
	case []byte:
		n += 2 + len(v)
	default:
		n += 6
	}
	return n
}

func fmtArcs(arcs []uint32) string {
	s := ""
	for i, v := range arcs {
		if i > 0 {
			s += "."
		}
		s += fmt.Sprint(v)
	}
	return s
}
