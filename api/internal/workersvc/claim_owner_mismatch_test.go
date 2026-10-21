package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimContextNoRowsDistinguishesOwnerMismatch pins issue #1691. Since #1688
// GetRunClaimContext returns no rows when the run's repo is not owned by the run's user.
// assembleClaim used to read that as a vanished run, so the claim was dropped silently and
// the run cycled claimed -> queued. It now probes the run row: a surviving row is failed
// terminally as credential_unavailable, a missing row is still a silent drop, and a failing
// probe surfaces as an error without failing the run.
//
// The Claude cross-check child lane (lead-first via GetCrossCheckByChild) needs a live
// transaction and is not drivable through the fake store.
func TestClaimContextNoRowsDistinguishesOwnerMismatch(t *testing.T) {
	cases := []struct {
		name       string
		probeErr   error
		wantErr    bool
		wantFailed bool
	}{
		{name: "owner mismatch fails the run", wantFailed: true},
		{name: "vanished run is dropped silently", probeErr: pgx.ErrNoRows},
		{name: "probe error is surfaced", probeErr: errors.New("boom"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaimFixture(t)
			f.fs.claimCtxErr = pgx.ErrNoRows
			f.fs.runByIDPlain = f.fs.claimRun
			f.fs.runByIDPlainErr = tc.probeErr

			payload, err := f.svc.Claim(context.Background(), store.Worker{ID: f.fs.claimRun.ID, UserID: f.owner}, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Claim err = %v, wantErr %v", err, tc.wantErr)
			}
			if payload != nil {
				t.Fatalf("no payload may be handed over, got %+v", payload)
			}
			if (f.fs.claimFailed != nil) != tc.wantFailed {
				t.Fatalf("claimFailed = %+v, want failed=%v", f.fs.claimFailed, tc.wantFailed)
			}
			if !tc.wantFailed {
				return
			}
			p := f.fs.claimFailed
			if p.FailOrigin.String != "credential_unavailable" {
				t.Fatalf("FailOrigin = %q, want credential_unavailable", p.FailOrigin.String)
			}
			reason := p.FailureReason.String
			if !strings.HasPrefix(reason, "credential unavailable") || !strings.Contains(reason, "not owned by the run's user") {
				t.Fatalf("FailureReason = %q", reason)
			}
		})
	}
}
