package workersvc

import (
	"context"
	"errors"
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

// TestCheckerClaimDoesNotReadTheOwnerLane pins the checker half of the conditional lane read in
// claim assembly: the lane is not read for a checker claim and the claim does not fail on it (a
// lead claim does, see TestClaimFailsOnDefaultModelLookupError). The Codex review half needs a
// bound Codex credential and is pinned by
// TestAssembleCodexReviewClaimDoesNotReadOwnerLaneLiveDB.
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
	// The fake store has no transaction, so a checker claim that assembles fully stops at the final
	// run-claim check with exactly this refusal, after the lane-read decision. Requiring it (and
	// not just any error) proves assembly got past the seam rather than failing on something
	// earlier; the lane read must never have been attempted either.
	_, err := svc.Claim(context.Background(), w, nil)
	if !errors.Is(err, errClaimRecoveryNoTx) {
		t.Fatalf("checker claim = %v, want assembly to reach the final run-claim check", err)
	}
	if reads != 0 {
		t.Fatalf("checker claim read the owner lane %d times, want 0", reads)
	}
}
