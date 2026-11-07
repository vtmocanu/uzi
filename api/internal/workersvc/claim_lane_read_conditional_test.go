package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkerPinFake adds the cross-check pin snapshot a checker claim reads to the shared fake.
type checkerPinFake struct {
	*fakeStore
	row       store.GetCrossCheckPinSnapshotRow
	laneReads *int
}

func (f checkerPinFake) GetUserHarnessModelDefaults(ctx context.Context, id uuid.UUID) (store.GetUserHarnessModelDefaultsRow, error) {
	*f.laneReads++
	return f.fakeStore.GetUserHarnessModelDefaults(ctx, id)
}

func (f checkerPinFake) GetCrossCheckPinSnapshot(context.Context, store.GetCrossCheckPinSnapshotParams) (store.GetCrossCheckPinSnapshotRow, error) {
	return f.row, nil
}

// TestCheckerClaimDoesNotReadTheOwnerLane pins the conditional lane read in claim assembly: a
// checker child resolves its model from the cross-check pin snapshot, so a failing
// GetUserHarnessModelDefaults must not fail its claim (it does fail a lead claim, see
// TestClaimFailsOnDefaultModelLookupError). A Codex review claim skips the same read by the
// harness != codex || !isReviewRun condition; that branch needs a bound Codex credential and is
// covered on the real path by per_harness_model_assembly_livedb_test.go.
func TestCheckerClaimDoesNotReadTheOwnerLane(t *testing.T) {
	w := worker()
	fs := leadClaimFake(t, runkind.CrossCheck, func(r *store.Run) {
		r.UserID = w.UserID
		r.IssueIid = pgtype.Int8{}
		r.IssueTitle, r.IssueDescription = "check", "plan"
	})
	fs.defaultModelErr = errors.New("lane read must not happen for a checker")
	reads := 0
	fake := checkerPinFake{fakeStore: fs, laneReads: &reads, row: store.GetCrossCheckPinSnapshotRow{
		Stage: "plan", CheckerHarness: pgconv.TextOrNull("claude"), TemplateModel: pgconv.TextOrNull("opus"),
	}}
	svc := newOffModeService(fake, newBox(t), testParams())
	// The fake store has no transaction, so assembly stops at the final run-claim check, after the
	// lane-read decision. The lane read itself must never have been attempted, and its error must
	// not be what the claim fails with.
	_, err := svc.Claim(context.Background(), w, nil)
	if err != nil && strings.Contains(err.Error(), "harness model defaults lookup") {
		t.Fatalf("checker claim failed on the owner lane read: %v", err)
	}
	if reads != 0 {
		t.Fatalf("checker claim read the owner lane %d times, want 0", reads)
	}
}
