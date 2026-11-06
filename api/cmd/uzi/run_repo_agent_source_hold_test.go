package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestHoldRowRepoAgentSource(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 12, 0, 0, time.UTC)
	run := heldRun("r1", holdRepoAgentSource, now.Add(-70*time.Minute))
	want := "repo agents unreadable · parked 14:02 (1h10m) · 4/7 milestones · resume once the repository can be fetched: uzi run resume r1"
	if row := holdRow(run, now); row == nil || row[1] != want {
		t.Errorf("holdRow = %v, want [HOLD %q]", row, want)
	}
}

func TestRunGetRepoAgentSourceHold(t *testing.T) {
	run := heldRun("r1", holdRepoAgentSource, time.Now().Add(-5*time.Minute))
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HOLD") || !strings.Contains(out, "repo agents unreadable") {
		t.Errorf("run get lacks the repo_agent_source_unavailable HOLD row:\n%s", out)
	}
}
