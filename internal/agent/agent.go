/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package agent defines the request contract toward an SNMP agent.
// Everything above this package talks to a Transport, so the search and
// walker can be tested against a simulated agent.
package agent

import (
	"context"
	"errors"
	"time"
)

// SysUpTimeOID is sysUpTime.0, used by canary probes.
const SysUpTimeOID = "1.3.6.1.2.1.1.3.0"

// ErrTimeout is returned when the agent did not answer within the timeout,
// after all retries.
var ErrTimeout = errors.New("snmp: timeout")

// ErrAgent is returned when the agent answered with an error status such
// as tooBig or genErr. It is the agent's own limit signal, not a transport
// problem.
var ErrAgent = errors.New("snmp: agent error")

// Exception values that an agent can return instead of a value.
type (
	EndOfMibView   struct{}
	NoSuchObject   struct{}
	NoSuchInstance struct{}
)

// Varbind is one OID with its value or exception.
type Varbind struct {
	OID   string
	Value any
}

// Response carries the varbinds plus the telemetry of one request.
type Response struct {
	Varbinds []Varbind
	RTT      time.Duration
	Bytes    int // encoded response size
	Retries  int // retransmissions before this response arrived
}

// Transport sends read-only requests to one agent.
// Implementations are used strictly sequentially: callers never issue a
// request before the previous one returned.
type Transport interface {
	Get(ctx context.Context, oids []string) (Response, error)
	GetNext(ctx context.Context, oids []string) (Response, error)
	GetBulk(ctx context.Context, oids []string, maxRepetitions uint32) (Response, error)
}
