/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Config describes the SNMP v2c target.
type Config struct {
	Target    string
	Port      int
	Community string
	Timeout   time.Duration
	Retries   int
}

// GoSNMP is the gosnmp-backed Transport for SNMP v2c.
type GoSNMP struct {
	c       *gosnmp.GoSNMP
	retries int
}

// NewGoSNMP connects the UDP socket for cfg.
func NewGoSNMP(cfg Config) (*GoSNMP, error) {
	t := &GoSNMP{}
	t.c = &gosnmp.GoSNMP{
		Target:    cfg.Target,
		Port:      uint16(cfg.Port),
		Community: cfg.Community,
		Version:   gosnmp.Version2c,
		Timeout:   cfg.Timeout,
		Retries:   cfg.Retries,
		MaxOids:   gosnmp.MaxOids,
		OnRetry:   func(*gosnmp.GoSNMP) { t.retries++ },
	}
	if err := t.c.Connect(); err != nil {
		return nil, fmt.Errorf("connect %s:%d: %w", cfg.Target, cfg.Port, err)
	}
	return t, nil
}

// Close releases the socket.
func (t *GoSNMP) Close() {
	if t.c.Conn != nil {
		_ = t.c.Conn.Close()
	}
}

// Get implements Transport.
func (t *GoSNMP) Get(ctx context.Context, oids []string) (Response, error) {
	return t.do(ctx, func() (*gosnmp.SnmpPacket, error) { return t.c.Get(oids) })
}

// GetNext implements Transport.
func (t *GoSNMP) GetNext(ctx context.Context, oids []string) (Response, error) {
	return t.do(ctx, func() (*gosnmp.SnmpPacket, error) { return t.c.GetNext(oids) })
}

// GetBulk implements Transport with non-repeaters fixed at zero.
func (t *GoSNMP) GetBulk(ctx context.Context, oids []string, maxRepetitions uint32) (Response, error) {
	return t.do(ctx, func() (*gosnmp.SnmpPacket, error) { return t.c.GetBulk(oids, 0, maxRepetitions) })
}

func (t *GoSNMP) do(ctx context.Context, send func() (*gosnmp.SnmpPacket, error)) (Response, error) {
	t.c.Context = ctx
	t.retries = 0
	start := time.Now()
	pkt, err := send()
	rtt := time.Since(start)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Response{RTT: rtt, Retries: t.retries}, ctxErr
		}
		if strings.Contains(err.Error(), "request timeout") {
			return Response{RTT: rtt, Retries: t.retries}, fmt.Errorf("%w after %d retries", ErrTimeout, t.retries)
		}
		return Response{RTT: rtt, Retries: t.retries}, err
	}
	if pkt.Error != gosnmp.NoError {
		return Response{RTT: rtt, Retries: t.retries}, fmt.Errorf("%w %v at index %d", ErrAgent, pkt.Error, pkt.ErrorIndex)
	}
	resp := Response{RTT: rtt, Retries: t.retries, Varbinds: make([]Varbind, 0, len(pkt.Variables))}
	for _, v := range pkt.Variables {
		resp.Varbinds = append(resp.Varbinds, Varbind{OID: strings.TrimPrefix(v.Name, "."), Value: convert(v)})
	}
	// gosnmp does not expose the wire length; re-marshalling the packet gives
	// the same encoding the agent sent, within a few bytes of header variance.
	if raw, err := pkt.MarshalMsg(); err == nil {
		resp.Bytes = len(raw)
	}
	return resp, nil
}

func convert(v gosnmp.SnmpPDU) any {
	switch v.Type {
	case gosnmp.EndOfMibView:
		return EndOfMibView{}
	case gosnmp.NoSuchObject:
		return NoSuchObject{}
	case gosnmp.NoSuchInstance:
		return NoSuchInstance{}
	case gosnmp.TimeTicks:
		return uint32(gosnmp.ToBigInt(v.Value).Uint64())
	}
	return v.Value
}
