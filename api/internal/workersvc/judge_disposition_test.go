package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ownedRecFixture wires a fakeStore whose owner has one judged run carrying a single
// improve_uzi/” recommendation, so the disposition write path resolves it under strict
// caller-ownership. Returns the store, run id, and the rec id the tests address.
func ownedRecFixture() (*fakeStore, uuid.UUID, uuid.UUID) {
	ownerID := uuid.New()
	runID := uuid.New()
	reviewID := uuid.New()
	recID := uuid.New()
	fs := &fakeStore{
		runByID:        store.Run{ID: runID, UserID: ownerID, Status: "completed", Kind: RunKindIssue},
		reviewByTarget: store.RunReview{ID: reviewID, TargetRunID: runID, UserID: ownerID},
		recsByReview: []store.ReviewRecommendation{
			{ID: recID, ReviewID: reviewID, Category: "improve_uzi", Target: "", RationaleMd: "tidy the thing"},
		},
		dispositionDeleteRows: 1,
	}
	return fs, runID, recID
}

// TestServiceSetDispositionResolvesOwnerAndReStampsHash: the owner's PUT resolves the
// recommendation under strict caller-ownership and upserts the coordinate with the current
// rationale's hash re-stamped (PRD #94 Decisions 3/6) and the reason mapped ("" → NULL).
func TestServiceSetDispositionResolvesOwnerAndReStampsHash(t *testing.T) {
	fs, runID, recID := ownedRecFixture()
	owner := fs.runByID.UserID
	svc := New(fs, newBox(t), testParams())

	if err := svc.SetDisposition(context.Background(), owner, runID, recID, "dismissed", "not_an_issue"); err != nil {
		t.Fatalf("SetDisposition: %v", err)
	}
	if fs.upsertedDisposition == nil {
		t.Fatal("the disposition upsert did not run")
	}
	got := *fs.upsertedDisposition
	if got.ReviewID != fs.reviewByTarget.ID || got.Category != "improve_uzi" || got.Target != "" {
		t.Fatalf("upsert coordinate wrong: %+v", got)
	}
	if got.Status != "dismissed" || got.DismissReason.String != "not_an_issue" || !got.DismissReason.Valid {
		t.Fatalf("status/reason wrong: %+v", got)
	}
	if got.RationaleHash != RationaleHash("tidy the thing") {
		t.Fatalf("rationale_hash = %q, want sha256 of the current rationale_md", got.RationaleHash)
	}
	// A 'done' carries no reason → the reason column is written NULL.
	fs.upsertedDisposition = nil
	if err := svc.SetDisposition(context.Background(), owner, runID, recID, "done", ""); err != nil {
		t.Fatalf("SetDisposition done: %v", err)
	}
	if fs.upsertedDisposition.DismissReason.Valid {
		t.Fatalf("a 'done' must write a NULL reason, got %+v", fs.upsertedDisposition.DismissReason)
	}
}

// TestServiceSetDispositionNonOwnerIsRunNotFound: when the owner-scoped run lookup returns
// no row (a non-owner, incl. a uza_ admin_ro token — the write path passes isAdmin=false),
// SetDisposition returns ErrRunNotFound and never upserts (PRD #94 Decision 5).
func TestServiceSetDispositionNonOwnerIsRunNotFound(t *testing.T) {
	fs, runID, recID := ownedRecFixture()
	fs.runByIDErr = pgx.ErrNoRows // the owner-scoped GetRunByIDForUser found nothing for this caller
	svc := New(fs, newBox(t), testParams())

	err := svc.SetDisposition(context.Background(), uuid.New(), runID, recID, "done", "")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
	if fs.upsertedDisposition != nil {
		t.Fatal("a non-owner must not reach the upsert")
	}
}

// TestServiceSetDispositionUnknownRecIsRecNotFound: a recID absent from the current review
// (re-judged away or never present) is ErrRecommendationNotFound — the handler maps it to
// the same 404 as a foreign run, so no existence oracle leaks (PRD #94 Decision 5).
func TestServiceSetDispositionUnknownRecIsRecNotFound(t *testing.T) {
	fs, runID, _ := ownedRecFixture()
	svc := New(fs, newBox(t), testParams())

	err := svc.SetDisposition(context.Background(), fs.runByID.UserID, runID, uuid.New(), "done", "")
	if !errors.Is(err, ErrRecommendationNotFound) {
		t.Fatalf("err = %v, want ErrRecommendationNotFound", err)
	}
	if fs.upsertedDisposition != nil {
		t.Fatal("an unknown recID must not reach the upsert")
	}
}

// TestServiceDeleteDispositionUndoAndDoubleUndo: an existing disposition deletes cleanly
// (1 row); a second delete affects 0 rows and surfaces ErrRecommendationNotFound so the
// handler 404s a double-undo (PRD #94 Decision 6).
func TestServiceDeleteDispositionUndoAndDoubleUndo(t *testing.T) {
	fs, runID, recID := ownedRecFixture()
	owner := fs.runByID.UserID
	svc := New(fs, newBox(t), testParams())

	fs.dispositionDeleteRows = 1
	if err := svc.DeleteDisposition(context.Background(), owner, runID, recID); err != nil {
		t.Fatalf("first undo: %v", err)
	}
	if fs.deletedDisposition == nil || fs.deletedDisposition.Category != "improve_uzi" {
		t.Fatalf("delete did not target the coordinate: %+v", fs.deletedDisposition)
	}

	fs.dispositionDeleteRows = 0
	err := svc.DeleteDisposition(context.Background(), owner, runID, recID)
	if !errors.Is(err, ErrRecommendationNotFound) {
		t.Fatalf("double undo err = %v, want ErrRecommendationNotFound (0 rows)", err)
	}
}

// TestServiceDeleteDispositionNonOwnerIsRunNotFound: undo is owner-only too — a non-owner's
// run lookup misses, so DeleteDisposition returns ErrRunNotFound and never deletes.
func TestServiceDeleteDispositionNonOwnerIsRunNotFound(t *testing.T) {
	fs, runID, recID := ownedRecFixture()
	fs.runByIDErr = pgx.ErrNoRows
	svc := New(fs, newBox(t), testParams())

	err := svc.DeleteDisposition(context.Background(), uuid.New(), runID, recID)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
	if fs.deletedDisposition != nil {
		t.Fatal("a non-owner must not reach the delete")
	}
}

// TestServiceJudgeTriageStatsAggregate proves the single ladder (PRD #94 Decisions 2/8) at
// the service seam: a mix of flat rows buckets by dismissed > done > filed(settled) > todo,
// an UNSETTLED claim (filed_settled=false) counts as todo not filed, and false_positives
// counts only not_an_issue. The store query is owner-scoped by the caller's id.
func TestServiceJudgeTriageStatsAggregate(t *testing.T) {
	text := func(s string) pgtype.Text {
		if s == "" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	fs := &fakeStore{
		judgeTriageRows: []store.ListJudgeTriageRowsForUserRow{
			{DispositionStatus: text("dismissed"), DismissReason: text("not_an_issue"), FiledSettled: true}, // dismissed beats filed
			{DispositionStatus: text("dismissed"), DismissReason: text("wont_do"), FiledSettled: false},
			{DispositionStatus: text("done"), FiledSettled: true}, // done beats filed
			{DispositionStatus: text(""), FiledSettled: true},     // filed (settled)
			{DispositionStatus: text(""), FiledSettled: false},    // unsettled claim → todo
			{DispositionStatus: text(""), FiledSettled: false},    // open → todo
		},
	}
	svc := New(fs, newBox(t), testParams())

	got, err := svc.JudgeTriageStats(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("JudgeTriageStats: %v", err)
	}
	want := apitypes.TriageDTO{Total: 6, Todo: 2, Filed: 1, Done: 1, Dismissed: 2, FalsePositives: 1}
	if got != want {
		t.Fatalf("triage = %+v, want %+v", got, want)
	}
}
