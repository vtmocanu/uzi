package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func blockedWorker(reason string) apitypes.WorkerDTO {
	since := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	at := time.Date(2026, 3, 4, 9, 30, 0, 0, time.FixedZone("x", 3600)) // 08:30Z
	return apitypes.WorkerDTO{
		ID: "w1", Name: "prod-1", Status: "online", Kind: "hosted",
		HibernationBlockedReason: &reason, HibernationBlockedSince: &since, HibernationBlockedAt: &at,
	}
}

// The detail line names the blocker, since when and the last observation, in RFC3339 UTC, on
// every worker listing surface.
func TestWorkerHibernationBlockedDetail(t *testing.T) {
	const want = "Hibernation blocker (last recorded) worker w1: recovery custody hold (since 2026-03-01T08:00:00Z, last observed 2026-03-04T08:30:00Z)"
	w := blockedWorker("custody_hold")

	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{w}}), "worker", "list")
	if code != uzicli.ExitOK || !strings.Contains(out, want) {
		t.Fatalf("worker list exit %d, missing %q:\n%s", code, want, out)
	}
	out, _, code = runCLI(t, fakeEnv(&uzicli.FakeClient{AdminWorkers: []apitypes.AdminWorkerDTO{{WorkerDTO: w, OwnerEmail: "o@uzi.test"}}}), "admin", "workers")
	if code != uzicli.ExitOK || !strings.Contains(out, want) {
		t.Fatalf("admin workers exit %d, missing %q:\n%s", code, want, out)
	}

	none := apitypes.WorkerDTO{ID: "w2", Name: "idle", Status: "online", Kind: "hosted"}
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{none}}), "worker", "list")
	if strings.Contains(out, "Hibernation blocker") {
		t.Fatalf("unset reason printed a detail line:\n%s", out)
	}
}

// Every closed code has a human label, and an unknown or hostile code is sanitized.
func TestWorkerHibernationBlockedLabels(t *testing.T) {
	for _, code := range workersvc.HibernationBlockedReasons() {
		if hibernationBlockedLabels[code] == "" {
			t.Errorf("code %q has no CLI label", code)
		}
	}
	if len(hibernationBlockedLabels) != len(workersvc.HibernationBlockedReasons()) {
		t.Errorf("CLI labels %d != workersvc closed set %d", len(hibernationBlockedLabels), len(workersvc.HibernationBlockedReasons()))
	}
	out, _, _ := runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{blockedWorker("weird\x1b[31m_code\nFORGED ROW")}}), "worker", "list")
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\nFORGED ROW") {
		t.Fatalf("unsanitized reason:\n%q", out)
	}
	if !strings.Contains(out, "Hibernation blocker (last recorded) worker w1: weird") {
		t.Fatalf("unknown code not shown:\n%s", out)
	}
}

// --json passes the three fields through, null when unset.
func TestWorkerHibernationBlockedJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{blockedWorker("roll"), {ID: "w2"}}}), "worker", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d", code)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 2 {
		t.Fatalf("decode %v (%d rows):\n%s", err, len(got), out)
	}
	if got[0]["hibernation_blocked_reason"] != "roll" ||
		got[0]["hibernation_blocked_since"] != "2026-03-01T08:00:00Z" ||
		got[0]["hibernation_blocked_at"] != "2026-03-04T09:30:00+01:00" {
		t.Fatalf("set row: %v", got[0])
	}
	for _, k := range []string{"hibernation_blocked_reason", "hibernation_blocked_since", "hibernation_blocked_at"} {
		if v, ok := got[1][k]; !ok || v != nil {
			t.Fatalf("unset %s = %v (present %v), want null", k, v, ok)
		}
	}
}

func rollMarkerWorker(m *apitypes.RollMarkerDTO) apitypes.WorkerDTO {
	return apitypes.WorkerDTO{ID: "w1", Name: "prod-1", Status: "online", Kind: "hosted", RollMarker: m}
}

// The roll-marker line shows every populated field and is sanitized.
func TestWorkerRollMarkerLine(t *testing.T) {
	yes := true
	ver, grp, blocked := "0.11.7", "0123456789abcdef", "waiting\x1b[31m_for\nFORGED ROW"
	at := time.Date(2026, 3, 4, 9, 30, 0, 0, time.FixedZone("x", 3600))
	m := &apitypes.RollMarkerDTO{State: "present", Canary: &yes, Completed: &yes, TargetVersion: &ver,
		CoverageGroup: &grp, Reservation: &yes, Blocked: &blocked, ObservedAt: &at}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{rollMarkerWorker(m)}}), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "\nFORGED ROW") {
		t.Fatalf("unsanitized marker:\n%q", out)
	}
	const want = "Roll marker worker w1: present · canary · completed · target 0.11.7 · group 0123456789abcdef · reservation · blocked: waiting_for FORGED ROW · observed 2026-03-04T08:30:00Z"
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in:\n%s", want, out)
	}

	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{rollMarkerWorker(&apitypes.RollMarkerDTO{State: "unknown"})}}), "worker", "list")
	if !strings.Contains(out, "Roll marker worker w1: unknown\n") {
		t.Fatalf("unknown marker line missing:\n%s", out)
	}
	// With several hosted workers each marker line names its own worker.
	w2 := rollMarkerWorker(&apitypes.RollMarkerDTO{State: "absent"})
	w2.ID, w2.Name = "w2", "prod-2"
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{rollMarkerWorker(m), w2}}), "worker", "list")
	if !strings.Contains(out, "Roll marker worker w1: present") || !strings.Contains(out, "Roll marker worker w2: absent\n") {
		t.Fatalf("marker lines not attributed per worker:\n%s", out)
	}
	ext := rollMarkerWorker(m)
	ext.Kind = "external"
	out, _, _ = runCLI(t, fakeEnv(&uzicli.FakeClient{Workers: []apitypes.WorkerDTO{ext}}), "worker", "list")
	if strings.Contains(out, "Roll marker") {
		t.Fatalf("external worker printed a marker:\n%s", out)
	}
}
