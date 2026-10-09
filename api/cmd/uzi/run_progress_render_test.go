package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestProgressRow(t *testing.T) {
	pct := 70
	since := time.Date(2026, 10, 9, 16, 41, 0, 0, time.UTC)
	ms := []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
	cases := []struct {
		name string
		r    apitypes.RunDTO
		want string // "" = no row
	}{
		{"nil progress", apitypes.RunDTO{Status: "completed"}, ""},
		{"active milestone position", apitypes.RunDTO{Milestones: ms,
			Progress: &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneDone: 2, MilestoneTotal: 3, ActiveMilestoneID: "m3"}},
			"≈70% · milestone 3 of 3"},
		{"no active milestone", apitypes.RunDTO{Milestones: ms,
			Progress: &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneDone: 2, MilestoneTotal: 3}},
			"≈70% · 2 of 3 done"},
		{"stalled", apitypes.RunDTO{HealthSince: &since, Progress: &apitypes.RunProgress{State: "stalled"}}, "stalled · since 16:41"},
		{"stalled no time", apitypes.RunDTO{Progress: &apitypes.RunProgress{State: "stalled"}}, "stalled"},
		{"plan gate", apitypes.RunDTO{Status: "awaiting_approval", Progress: &apitypes.RunProgress{State: "waiting"}}, "waits on you · plan gate"},
		{"question", apitypes.RunDTO{Status: "awaiting_input", Progress: &apitypes.RunProgress{State: "waiting"}}, "waits on you · question"},
		{"follow-up", apitypes.RunDTO{Status: "awaiting_followup", Progress: &apitypes.RunProgress{State: "waiting"}}, "waits on you · follow-up"},
		{"parked limit", apitypes.RunDTO{Status: "limit_wait", Progress: &apitypes.RunProgress{State: "parked"}}, "parked · limit wait"},
		{"parked pool", apitypes.RunDTO{Status: "pool_wait", Progress: &apitypes.RunProgress{State: "parked"}}, "parked · pool wait"},
		{"parked recovery", apitypes.RunDTO{Status: "recovery_wait", Progress: &apitypes.RunProgress{State: "parked"}}, "parked · recovery wait"},
		{"parked paused", apitypes.RunDTO{Status: "paused", Progress: &apitypes.RunProgress{State: "parked"}}, "parked · paused"},
		{"queued", apitypes.RunDTO{Progress: &apitypes.RunProgress{State: "queued"}}, "queued"},
		{"planning", apitypes.RunDTO{Progress: &apitypes.RunProgress{State: "planning"}}, "planning"},
		{"none", apitypes.RunDTO{Progress: &apitypes.RunProgress{State: "none"}}, ""},
		{"unknown state", apitypes.RunDTO{Progress: &apitypes.RunProgress{State: "future"}}, ""},
	}
	for _, c := range cases {
		row := progressRow(c.r)
		if c.want == "" {
			if row != nil {
				t.Errorf("%s: want no row, got %v", c.name, row)
			}
			continue
		}
		if len(row) != 2 || row[0] != "PROGRESS" || row[1] != c.want {
			t.Errorf("%s: got %v, want PROGRESS %q", c.name, row, c.want)
		}
	}
}

func TestProgressRowBlockedByHint(t *testing.T) {
	id := "fca7a801-aaaa-bbbb-cccc-dddddddddddd"
	r := apitypes.RunDTO{Status: "awaiting_input", Progress: &apitypes.RunProgress{State: "waiting", MaybeBlockedByRunID: &id}}
	row := progressRow(r)
	if len(row) != 2 || row[1] != "waits on you · question · may be blocked by fca7a801" {
		t.Errorf("got %v", row)
	}
	hostile := "\x1b[2J\u202E\x07evil"
	r.Progress.MaybeBlockedByRunID = &hostile
	if row := progressRow(r); strings.ContainsAny(row[1], "\x1b\x07\u202e") {
		t.Errorf("hostile id reached the row: %q", row[1])
	}
}

func TestRunGetPrintsProgressRowAndFieldIsUsageErrorWhileLive(t *testing.T) {
	pct := 70
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"live": {ID: "live", Kind: "issue", Status: "running", Milestones: []apitypes.Milestone{{ID: "m1"}},
			Progress: &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneTotal: 1, ActiveMilestoneID: "m1"}},
		"done": {ID: "done", Kind: "issue", Status: "completed"},
	}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "live")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "PROGRESS") || !strings.Contains(stdout, "≈70% · milestone 1 of 1") {
		t.Errorf("PROGRESS row missing:\n%s", stdout)
	}
	if _, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "live", "--field", "progress"); code != uzicli.ExitUsage || !strings.Contains(stderr, "not a scalar") {
		t.Errorf("--field progress on a live run: exit %d, stderr %q; want usage error", code, stderr)
	}
	// A terminal run carries progress null: an empty line, exit 0, and no PROGRESS row.
	stdout, _, code = runCLI(t, fakeEnv(fc), "run", "get", "done", "--field", "progress")
	if code != uzicli.ExitOK || strings.TrimSpace(stdout) != "" {
		t.Errorf("--field progress on a terminal run: exit %d stdout %q; want empty, 0", code, stdout)
	}
	if out, _, _ := runCLI(t, fakeEnv(fc), "run", "get", "done"); strings.Contains(out, "PROGRESS") {
		t.Errorf("terminal run must not print PROGRESS:\n%s", out)
	}
}
