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
