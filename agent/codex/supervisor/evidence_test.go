package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, v any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestStartedEvidence(t *testing.T) {
	m := decode(t, startedEvidence{
		Event: "started", SupervisorPid: 7, ChildPid: 8,
		Subreaper: true, Nondumpable: true, UID: 10002, LiveCapsZero: true,
		CapBoundingSet: "0xc0", NoNewPrivs: true,
	})
	if m["event"] != "started" {
		t.Errorf("event = %v", m["event"])
	}
	for _, k := range []string{"supervisorPid", "childPid", "subreaper", "nondumpable", "uid", "liveCapsZero", "capBoundingSet", "noNewPrivs"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if m["subreaper"] != true || m["nondumpable"] != true || m["liveCapsZero"] != true || m["capBoundingSet"] != "0xc0" || m["noNewPrivs"] != true {
		t.Errorf("posture flags not all true: %v", m)
	}
}

func TestSnapshotEvidenceCommOnly(t *testing.T) {
	m := decode(t, snapshotEvidence{
		Event: "snapshot", ID: 3,
		Processes: []procRow{{Pid: 11, PPid: 7, Pgid: 11, Comm: "codex"}},
	})
	if m["event"] != "snapshot" {
		t.Errorf("event = %v", m["event"])
	}
	procs, ok := m["processes"].([]any)
	if !ok || len(procs) != 1 {
		t.Fatalf("processes = %v", m["processes"])
	}
	row := procs[0].(map[string]any)
	for _, k := range []string{"pid", "ppid", "pgid", "comm"} {
		if _, ok := row[k]; !ok {
			t.Errorf("row missing %q", k)
		}
	}
	// The row must carry NO cmdline/argv key.
	for _, forbidden := range []string{"cmdline", "command", "argv", "exe"} {
		if _, ok := row[forbidden]; ok {
			t.Errorf("row must not carry %q", forbidden)
		}
	}
}

func TestDisposeEvidenceDrained(t *testing.T) {
	d := drainResult{State: stateDrained, Authority: authorityECHILD, Killed: []int{11}, Reaped: []int{11}}
	m := decode(t, disposeEvidence(4, d))
	if m["event"] != "dispose" || m["id"].(float64) != 4 {
		t.Errorf("event/id = %v/%v", m["event"], m["id"])
	}
	if m["state"] != stateDrained || m["authority"] != authorityECHILD {
		t.Errorf("state/authority = %v/%v", m["state"], m["authority"])
	}
	// A drained result carries NO children and NO reason key.
	if _, ok := m["children"]; ok {
		t.Errorf("drained must not carry a children key")
	}
	if _, ok := m["reason"]; ok {
		t.Errorf("drained must not carry a reason key")
	}
}

func TestDisposeEvidenceUnconfirmed(t *testing.T) {
	d := drainResult{State: stateUnconfirmed, Reason: reasonDeadline, Killed: []int{11}, Reaped: nil, Children: []int{11}}
	m := decode(t, disposeEvidence(5, d))
	if m["state"] != stateUnconfirmed || m["reason"] != reasonDeadline {
		t.Errorf("state/reason = %v/%v", m["state"], m["reason"])
	}
	children, ok := m["children"].([]any)
	if !ok || len(children) != 1 {
		t.Errorf("children = %v", m["children"])
	}
	// An unconfirmed result carries NO authority key.
	if _, ok := m["authority"]; ok {
		t.Errorf("unconfirmed must not carry an authority key")
	}
	// Empty reaped must render as [] not null.
	if reaped, ok := m["reaped"].([]any); !ok || len(reaped) != 0 {
		t.Errorf("reaped = %v, want empty array", m["reaped"])
	}
}

func TestAbnormalEvidence(t *testing.T) {
	// Pre-fork abnormal: no cleanup key.
	m := decode(t, abnormalEvidence("profile:uid", nil))
	if m["event"] != "abnormal" || m["reason"] != "profile:uid" {
		t.Errorf("got %v", m)
	}
	if _, ok := m["cleanup"]; ok {
		t.Errorf("pre-fork abnormal must carry no cleanup")
	}
	// Post-fork abnormal: cleanup carries the drain result.
	cleanup := drainResult{State: stateDrained, Authority: authorityECHILD}
	m2 := decode(t, abnormalEvidence("control EOF", &cleanup))
	c, ok := m2["cleanup"].(map[string]any)
	if !ok {
		t.Fatalf("cleanup = %v", m2["cleanup"])
	}
	if c["state"] != stateDrained {
		t.Errorf("cleanup.state = %v", c["state"])
	}
}

func TestEvidenceWriterLineCap(t *testing.T) {
	var buf bytes.Buffer
	ev := &evidence{w: &buf}
	for i := 0; i < maxEvidenceLines+50; i++ {
		if err := ev.writeJSON(map[string]any{"n": i}); err != nil {
			t.Fatalf("writeJSON: %v", err)
		}
	}
	lines := strings.Count(buf.String(), "\n")
	if lines != maxEvidenceLines {
		t.Errorf("wrote %d lines, want %d", lines, maxEvidenceLines)
	}
}

func TestEvidenceWriterSizeCap(t *testing.T) {
	var buf bytes.Buffer
	ev := &evidence{w: &buf}
	big := strings.Repeat("a", maxEvidenceBytes+10)
	if err := ev.writeJSON(map[string]any{"x": big}); err == nil {
		t.Errorf("expected an over-length line to error")
	}
	if buf.Len() != 0 {
		t.Errorf("nothing should have been written, got %d bytes", buf.Len())
	}
}
