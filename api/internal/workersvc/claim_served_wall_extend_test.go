package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestClaimServedWallIncludesExtend pins PRD #1189 M1's worker-serve crux (D6): the wall a
// worker arms at claim (Config.RunTimeoutSeconds) is COALESCE(budget_wall_seconds, RUN_TIMEOUT)
// PLUS budget_extension_seconds, so a run extended while queued/parked arms the already-larger
// hard wall when it starts. Without the extension term the worker would trip REASON_WALL at the
// frozen budget before the server's extended deadline and the extend would be a silent no-op.
//
// Driven through the full svc.Claim (a self_improve run, which takes the ordinary assembleClaim
// path) rather than a re-derived formula, so the assertion moves with the real assembly. The
// active-runs table is empty (no in-flight targets), keeping the payload minimal.
func TestClaimServedWallIncludesExtend(t *testing.T) {
	const wallSeconds = 28800 // frozen 8h budget

	cases := []struct {
		name string
		ext  int32
		want int // Config.RunTimeoutSeconds
	}{
		// A 2h extension on top of the frozen 8h wall arms a 10h served wall.
		{"8h wall + 2h extension arms 10h", 7200, wallSeconds + 7200},
		// No extension is byte-identical to the pre-#1189 served wall (the coalesced budget).
		{"8h wall + no extension arms the frozen wall", 0, wallSeconds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selfRunID, repoID := uuid.New(), uuid.New()
			fs, svc, wkr := selfImproveClaimFixture(t, selfRunID, repoID)
			// selfImproveClaimFixture already stamps claimRun.RepoID = repoID; only the
			// budget columns need setting for this test.
			fs.claimRun.BudgetWallSeconds = pgtype.Int4{Int32: wallSeconds, Valid: true}
			fs.claimRun.BudgetExtensionSeconds = tc.ext
			fs.activeRunsAll = nil

			payload, err := svc.Claim(context.Background(), wkr)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if payload == nil {
				t.Fatal("expected a self_improve payload, got idle")
			}
			if got := payload.Config.RunTimeoutSeconds; got != tc.want {
				t.Fatalf("served RunTimeoutSeconds = %d, want %d (wall %d + ext %d)", got, tc.want, wallSeconds, tc.ext)
			}
		})
	}
}
