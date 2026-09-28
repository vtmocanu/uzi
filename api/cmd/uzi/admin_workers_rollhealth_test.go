package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1484 M3: `uzi admin workers` gains VERSION, UPGRADE and BLOCKING, so an admin can see
// which owner's fleet is stuck rolling and why — the roll-health columns the owner-scoped
// `uzi worker list` already had. UPGRADE reuses upgradeCell, so the two consumers agree.
func TestAdminWorkersShowsRollHealth(t *testing.T) {
	version := "0.83.0"
	container, reason := "seed-nix", "ImagePullBackOff"
	fc := &uzicli.FakeClient{AdminWorkers: []apitypes.AdminWorkerDTO{
		{
			WorkerDTO: apitypes.WorkerDTO{
				ID: "w1", Name: "prod-1", Status: "online", Version: &version,
				UpgradeStatus:            "upgrade_failed",
				UpgradeBlockingContainer: &container,
				UpgradeBlockingReason:    &reason,
			},
			OwnerEmail: "owner@uzi.test",
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "workers")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"VERSION", "UPGRADE", "BLOCKING", // the new headers
		"0.83.0",                     // VERSION cell
		"FAILED",                     // upgradeCell maps upgrade_failed → FAILED
		"seed-nix: ImagePullBackOff", // BLOCKING cell, container: reason
		"owner@uzi.test", "prod-1",   // the row's other columns still render
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin workers missing %q:\n%s", want, out)
		}
	}
}

// blockingCell renders the compact container/reason behind an upgrade_failed roll, "-" when
// neither is set, and either single field when only one is present.
func TestBlockingCell(t *testing.T) {
	container, reason := "seed-nix", "CrashLoopBackOff"
	cases := []struct {
		name string
		w    apitypes.WorkerDTO
		want string
	}{
		{"none", apitypes.WorkerDTO{}, "-"},
		{"both", apitypes.WorkerDTO{UpgradeBlockingContainer: &container, UpgradeBlockingReason: &reason}, "seed-nix: CrashLoopBackOff"},
		{"reason-only", apitypes.WorkerDTO{UpgradeBlockingReason: &reason}, "CrashLoopBackOff"},
		{"container-only", apitypes.WorkerDTO{UpgradeBlockingContainer: &container}, "seed-nix"},
	}
	for _, tc := range cases {
		if got := blockingCell(tc.w); got != tc.want {
			t.Errorf("%s: blockingCell = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// PRD #1809 M6 (D8): `uzi admin workers` carries the LARGEST RUN column `uzi worker list` has,
// through the same largestRunCell, so the fleet.rundisk health check's action (which points at
// this command) lands on a fleet-wide view rather than the caller's own workers. It sits before
// OUTBOX, "-" when a worker reports no run sizes, "+" on a truncated (lower-bound) walk.
func TestAdminWorkersShowsLargestRunColumn(t *testing.T) {
	const gib = int64(1) << 30
	fc := &uzicli.FakeClient{AdminWorkers: []apitypes.AdminWorkerDTO{
		{WorkerDTO: apitypes.WorkerDTO{ID: "w1", Name: "idle", Status: "online"}, OwnerEmail: "a@uzi.test"},
		{WorkerDTO: apitypes.WorkerDTO{ID: "w2", Name: "busy", Status: "online", RunDisk: []apitypes.WorkerRunDiskDTO{
			{RunID: "r1", HomeBytes: 5 * gib, CacheBytes: 3 * gib},
			{RunID: "r2", HomeBytes: gib},
		}}, OwnerEmail: "b@uzi.test"},
		{WorkerDTO: apitypes.WorkerDTO{ID: "w3", Name: "partial", Status: "online", RunDisk: []apitypes.WorkerRunDiskDTO{
			{RunID: "r3", HomeBytes: 512 * 1024 * 1024, Truncated: true},
		}}, OwnerEmail: "c@uzi.test"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "workers")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "LARGEST RUN") || strings.Index(lines[0], "LARGEST RUN") > strings.Index(lines[0], "OUTBOX") {
		t.Fatalf("header lacks LARGEST RUN before OUTBOX:\n%s", out)
	}
	// Row fields: ID OWNER NAME STATUS VERSION UPGRADE BLOCKING RUNS LARGEST-RUN OUTBOX, every
	// cell non-empty ("-" placeholders), so LARGEST RUN is the second-to-last field.
	cellOf := func(name string) string {
		t.Helper()
		for _, line := range lines {
			if f := strings.Fields(line); len(f) > 3 && f[2] == name {
				return f[len(f)-2]
			}
		}
		t.Fatalf("no row for %s in %q", name, out)
		return ""
	}
	for name, want := range map[string]string{"idle": "-", "busy": "5.0GiB", "partial": "512.0MiB+"} {
		if got := cellOf(name); got != want {
			t.Errorf("%s LARGEST RUN = %q, want %q\n%s", name, got, want, out)
		}
	}
}
