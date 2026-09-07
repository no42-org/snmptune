/*
 * Copyright 2026 Ronny Trommer <ronny@no42.org>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/no42-org/snmptune/internal/agent"
	"github.com/no42-org/snmptune/internal/inventory"
)

// startSnmpd runs net-snmp on a free high port with the repository config.
func startSnmpd(t *testing.T) (host string, port int) {
	t.Helper()
	if os.Getenv("SNMPTUNE_INTEGRATION") == "" {
		t.Skip("set SNMPTUNE_INTEGRATION=1 (make integration) to run against snmpd")
	}
	bin, err := exec.LookPath("snmpd")
	if err != nil {
		t.Skip("snmpd not on PATH")
	}
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = l.LocalAddr().(*net.UDPAddr).Port
	_ = l.Close()
	root, _ := filepath.Abs("../..")
	conf := filepath.Join(root, "testdata", "snmpd.conf")
	runDir := filepath.Join(root, "testdata", "run")
	_ = os.MkdirAll(runDir, 0o755)
	cmd := exec.Command(bin, "-f", "-Lo", "-C", "-c", conf, "-I", "-smux", "-p", filepath.Join(runDir, "snmpd.pid"),
		fmt.Sprintf("udp:127.0.0.1:%d", port))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SNMP_PERSISTENT_DIR="+runDir)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("snmpd output:\n%s", output.String())
		}
	})
	tr, err := agent.NewGoSNMP(agent.Config{Target: "127.0.0.1", Port: port, Community: "public", Timeout: 300 * time.Millisecond, Retries: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := tr.Get(context.Background(), []string{agent.SysUpTimeOID}); err == nil {
			return "127.0.0.1", port
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("snmpd did not answer within 10s:\n%s", output.String())
	return "", 0
}

func TestIntegrationReferenceWalkSystemAndIfTable(t *testing.T) {
	host, port := startSnmpd(t)
	tr, err := agent.NewGoSNMP(agent.Config{Target: host, Port: port, Community: "public", Timeout: 2 * time.Second, Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	inv, err := inventory.Walk(context.Background(), tr, []string{"1.3.6.1.2.1.1", "1.3.6.1.2.1.2.2"}, inventory.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Subtrees[0].Count() < 5 || inv.Subtrees[0].IsTable() {
		t.Fatalf("system group: %+v", inv.Subtrees[0])
	}
	if !inv.Subtrees[1].IsTable() || inv.Subtrees[1].Count() == 0 {
		t.Fatalf("ifTable should be a table with rows: %d oids, columns %v", inv.Subtrees[1].Count(), inv.Subtrees[1].Columns)
	}
}

func TestIntegrationFullRunProducesRecommendation(t *testing.T) {
	host, port := startSnmpd(t)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", host, "--port", fmt.Sprint(port), "--oid", "1.3.6.1.2.1.1", "--oid", "1.3.6.1.2.1.2.2",
		"--repeats", "1", "--cooldown", "0", "--max-product", "200"}, &out, &errw, nil)
	if code != 0 || !strings.Contains(out.String(), "max-repetitions") {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errw.String())
	}
	t.Log(out.String())
}

func TestIntegrationTimeoutReflectsSlowSubtree(t *testing.T) {
	host, port := startSnmpd(t)
	var out, errw bytes.Buffer
	code := run(context.Background(), []string{"--target", host, "--port", fmt.Sprint(port), "--oid", "1.3.6.1.4.1.8072.9999",
		"--repeats", "1", "--cooldown", "0", "--max-product", "8", "--timeout", "5s", "--json"}, &out, &errw, nil)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), errw.String())
	}
	var v struct {
		Recommendation *struct {
			TimeoutMs int     `json:"timeout_ms"`
			P99ms     float64 `json:"p99_ms"`
		} `json:"recommendation"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// Every OID of the pass subtree costs about 100 ms, so a PDU takes well over 200 ms
	// and the suggestion must leave the 500 ms floor behind.
	if v.Recommendation == nil || v.Recommendation.TimeoutMs <= 500 {
		t.Fatalf("slow subtree must raise the timeout suggestion: %s", out.String())
	}
}
