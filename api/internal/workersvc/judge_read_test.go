package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestGetReviewForTargetVisibility: the run-page review read is owner-or-admin scoped
// (audit M2 / M3 carry-forward). A run the viewer can't see is ErrRunNotFound; a
// visible-but-unjudged run is (nil, nil), not an error.
func TestGetReviewForTargetVisibility(t *testing.T) {
	owner, target := uuid.New(), uuid.New()

	t.Run("not visible", func(t *testing.T) {
		fs := &fakeStore{runByIDErr: pgx.ErrNoRows} // GetRunByIDForUser finds nothing for this user
		svc := New(fs, newBox(t), testParams())
		_, err := svc.GetReviewForTarget(context.Background(), owner, false, target)
		if err != ErrRunNotFound {
			t.Fatalf("err = %v, want ErrRunNotFound", err)
		}
	})

	t.Run("visible but unjudged", func(t *testing.T) {
		fs := &fakeStore{
			runByID:           store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "completed"},
			reviewByTargetErr: pgx.ErrNoRows, // no review row yet
		}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.GetReviewForTarget(context.Background(), owner, false, target)
		if err != nil {
			t.Fatalf("GetReviewForTarget: %v", err)
		}
		if res != nil {
			t.Fatalf("res = %+v, want nil (unjudged)", res)
		}
	})

	t.Run("judged returns review + recs", func(t *testing.T) {
		reviewID := uuid.New()
		fs := &fakeStore{
			runByID:        store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "completed"},
			reviewByTarget: store.RunReview{ID: reviewID, TargetRunID: target, UserID: owner, Verdict: "issues"},
			recsByReview: []store.ReviewRecommendation{
				{ID: uuid.New(), ReviewID: reviewID, Category: "install_worker_tool", Target: "jq"},
			},
		}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.GetReviewForTarget(context.Background(), owner, false, target)
		if err != nil {
			t.Fatalf("GetReviewForTarget: %v", err)
		}
		if res == nil || res.Review.ID != reviewID || len(res.Recommendations) != 1 {
			t.Fatalf("res = %+v, want the review + one rec", res)
		}
	})

	t.Run("admin sees any owner's review", func(t *testing.T) {
		other := uuid.New()
		fs := &fakeStore{
			runByIDPlain:   store.Run{ID: target, UserID: other, Kind: RunKindIssue, Status: "completed"}, // admin GetRunByID path
			reviewByTarget: store.RunReview{ID: uuid.New(), TargetRunID: target, UserID: other, Verdict: "ok"},
		}
		svc := New(fs, newBox(t), testParams())
		res, err := svc.GetReviewForTarget(context.Background(), uuid.New(), true, target)
		if err != nil || res == nil {
			t.Fatalf("admin read: res=%+v err=%v", res, err)
		}
	})
}

// TestRerunJudgeGates walks the re-run gate matrix (Decision 8): owner-only, terminal
// + eligible kind, the global kill-switch, an owner token, and the one-active-judge
// dedupe. Each blocked path returns its typed error and never mints a judge run.
func TestRerunJudgeGates(t *testing.T) {
	owner, target := uuid.New(), uuid.New()
	terminal := store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "completed"}

	cases := []struct {
		name    string
		fs      *fakeStore
		isAdmin bool
		wire    func(*Service)
		wantErr error
		minted  bool
	}{
		{
			name:    "not visible to a non-admin non-owner",
			fs:      &fakeStore{runByIDErr: pgx.ErrNoRows},
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: true}) },
			wantErr: ErrRunNotFound,
		},
		{
			name: "visible to an admin but not theirs to spend",
			// admin path loads via GetRunByID (runByIDPlain); the run is another user's.
			fs:      &fakeStore{runByIDPlain: store.Run{ID: target, UserID: uuid.New(), Kind: RunKindIssue, Status: "completed"}},
			isAdmin: true,
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: true}) },
			wantErr: ErrNotRunOwner,
		},
		{
			name:    "not terminal",
			fs:      &fakeStore{runByID: store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "running"}},
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: true}) },
			wantErr: ErrRunNotJudgeable,
		},
		{
			name:    "ineligible kind",
			fs:      &fakeStore{runByID: store.Run{ID: target, UserID: owner, Kind: RunKindChat, Status: "completed"}},
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: true}) },
			wantErr: ErrRunNotJudgeable,
		},
		{
			name:    "no settings wired",
			fs:      &fakeStore{runByID: terminal},
			wire:    func(s *Service) {},
			wantErr: ErrJudgeDisabled,
		},
		{
			name:    "global off",
			fs:      &fakeStore{runByID: terminal},
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: false}) },
			wantErr: ErrJudgeDisabled,
		},
		{
			name:    "no anthropic token",
			fs:      &fakeStore{runByID: terminal, anthropicErr: pgx.ErrNoRows},
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: true}) },
			wantErr: ErrNoAnthropicToken,
		},
		{
			name:    "already active",
			fs:      &fakeStore{runByID: terminal, anthropic: []byte("sealed"), createJudgeRunErr: &pgconn.PgError{Code: "23505"}},
			wire:    func(s *Service) { s.SetSettings(fakeSettings{enabled: true}) },
			wantErr: ErrJudgeAlreadyActive,
			minted:  true, // CreateJudgeRun was attempted (records the arg), then 23505'd
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(tc.fs, newBox(t), testParams())
			tc.wire(svc)
			_, err := svc.RerunJudge(context.Background(), owner, tc.isAdmin, target)
			if err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !tc.minted && tc.fs.createdJudgeRun != nil {
				t.Fatalf("a blocked re-run must not attempt to mint a judge run")
			}
		})
	}
}

// TestGetRunReviewPanel walks the four independent (review, pending) combinations the
// run page must render, plus the visibility short-circuit (PRD #119 M1).
//
// The combinations are the point: before #119 the panel saw only `review`, so "never
// judged" and "a judge is already coming" were the same nil and it showed the same copy
// and the same live button for both — the button whose only outcome in the second case
// is a 409 from the one-active-judge-per-target index. The two values are orthogonal,
// so all four corners are legal and each is asserted.
//
// The not-visible case additionally asserts the pending read is never ISSUED. Ordering
// the visibility gate before it is what stops the route being an oracle for whether
// someone else's run is being judged — a 404 that took a different number of queries is
// still a leak, so "returns ErrRunNotFound" alone would be a weaker claim than intended.
func TestGetRunReviewPanel(t *testing.T) {
	owner, target := uuid.New(), uuid.New()
	enqueued := time.Now().Add(-30 * time.Second).UTC()

	// visibleRun is what GetRunByIDForUser resolves for the owner in every case below.
	visibleRun := func() store.Run {
		return store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "completed"}
	}
	activeJudge := store.GetActiveJudgeRunForTargetRow{
		ID:        uuid.New(),
		Status:    "queued",
		CreatedAt: pgtype.Timestamptz{Time: enqueued, Valid: true},
	}
	judged := func(fs *fakeStore) {
		reviewID := uuid.New()
		fs.reviewByTarget = store.RunReview{ID: reviewID, TargetRunID: target, UserID: owner, Verdict: "issues"}
		fs.recsByReview = []store.ReviewRecommendation{
			{ID: uuid.New(), ReviewID: reviewID, Category: "install_worker_tool", Target: "jq"},
		}
	}

	t.Run("not visible: no pending read at all", func(t *testing.T) {
		fs := &fakeStore{runByIDErr: pgx.ErrNoRows, pendingJudgeRow: activeJudge}
		svc := New(fs, newBox(t), testParams())
		review, pending, err := svc.GetRunReviewPanel(context.Background(), owner, false, target)
		if err != ErrRunNotFound {
			t.Fatalf("err = %v, want ErrRunNotFound", err)
		}
		if review != nil || pending != nil {
			t.Fatalf("an invisible run must yield (nil, nil): %+v / %+v", review, pending)
		}
		// The fake is STAGED with an active judge; the gate must mean it is never asked.
		if len(fs.pendingJudgeLookups) != 0 {
			t.Fatalf("the pending-judge query ran for a run the viewer cannot see (%d lookups) — "+
				"the visibility gate must precede it", len(fs.pendingJudgeLookups))
		}
	})

	t.Run("unjudged with a judge in flight", func(t *testing.T) {
		fs := &fakeStore{
			runByID:           visibleRun(),
			reviewByTargetErr: pgx.ErrNoRows, // no verdict yet
			pendingJudgeRow:   activeJudge,
		}
		svc := New(fs, newBox(t), testParams())
		review, pending, err := svc.GetRunReviewPanel(context.Background(), owner, false, target)
		if err != nil {
			t.Fatalf("GetRunReviewPanel: %v", err)
		}
		if review != nil {
			t.Fatalf("review = %+v, want nil (unjudged)", review)
		}
		if pending == nil {
			t.Fatal("pending = nil, want the active judge — this is the state #119 exists for: " +
				"a verdict IS coming, and the panel must not offer the button that would 409")
		}
		if pending.Status != "queued" {
			t.Errorf("pending.Status = %q, want the RAW run status %q (normalization is the DTO's job)",
				pending.Status, "queued")
		}
		if !pending.EnqueuedAt.Equal(enqueued) {
			t.Errorf("pending.EnqueuedAt = %v, want the judge run's created_at %v", pending.EnqueuedAt, enqueued)
		}
		if len(fs.pendingJudgeLookups) != 1 || uuid.UUID(fs.pendingJudgeLookups[0].Bytes) != target {
			t.Errorf("pending lookups = %+v, want exactly one for the target %v", fs.pendingJudgeLookups, target)
		}
	})

	t.Run("judged with a re-judge in flight", func(t *testing.T) {
		fs := &fakeStore{runByID: visibleRun(), pendingJudgeRow: activeJudge}
		judged(fs)
		svc := New(fs, newBox(t), testParams())
		review, pending, err := svc.GetRunReviewPanel(context.Background(), owner, false, target)
		if err != nil {
			t.Fatalf("GetRunReviewPanel: %v", err)
		}
		// BOTH set: the prior verdict is still the best thing to show while the fresh
		// judge runs. This is the reload case — the re-queued state used to live only in
		// the tab that clicked, and now comes from the server.
		if review == nil || len(review.Recommendations) != 1 {
			t.Fatalf("review = %+v, want the prior verdict + its rec", review)
		}
		if pending == nil {
			t.Fatal("pending = nil, want the in-flight re-judge alongside the existing review")
		}
	})

	t.Run("judged, no judge in flight", func(t *testing.T) {
		fs := &fakeStore{runByID: visibleRun(), pendingJudgeErr: pgx.ErrNoRows}
		judged(fs)
		svc := New(fs, newBox(t), testParams())
		review, pending, err := svc.GetRunReviewPanel(context.Background(), owner, false, target)
		if err != nil {
			t.Fatalf("GetRunReviewPanel: %v", err)
		}
		if review == nil {
			t.Fatal("review = nil, want the settled verdict")
		}
		if pending != nil {
			t.Fatalf("pending = %+v, want nil — ErrNoRows is 'no judge in flight', not an error", pending)
		}
	})

	t.Run("never judged, no judge in flight", func(t *testing.T) {
		fs := &fakeStore{
			runByID:           visibleRun(),
			reviewByTargetErr: pgx.ErrNoRows,
			pendingJudgeErr:   pgx.ErrNoRows,
		}
		svc := New(fs, newBox(t), testParams())
		review, pending, err := svc.GetRunReviewPanel(context.Background(), owner, false, target)
		if err != nil {
			t.Fatalf("GetRunReviewPanel: %v", err)
		}
		if review != nil || pending != nil {
			t.Fatalf("(review, pending) = (%+v, %+v), want (nil, nil) — the genuinely-unjudged run "+
				"whose enabled Run-judge button is correct", review, pending)
		}
	})

	t.Run("admin sees another owner's panel", func(t *testing.T) {
		other := uuid.New()
		fs := &fakeStore{
			runByIDPlain:      store.Run{ID: target, UserID: other, Kind: RunKindIssue, Status: "completed"},
			reviewByTargetErr: pgx.ErrNoRows,
			pendingJudgeRow:   activeJudge,
		}
		svc := New(fs, newBox(t), testParams())
		_, pending, err := svc.GetRunReviewPanel(context.Background(), uuid.New(), true, target)
		if err != nil || pending == nil {
			t.Fatalf("admin panel read: pending=%+v err=%v; the pending signal rides the SAME "+
				"owner-or-admin gate as the review", pending, err)
		}
	})
}

// TestRerunJudgeHappyPath: an owner re-running a terminal, eligible run with the
// feature on and a token mints a judge run for the same owner, targeting the run.
func TestRerunJudgeHappyPath(t *testing.T) {
	owner, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		runByID:   store.Run{ID: target, UserID: owner, Kind: RunKindIssue, Status: "failed"},
		anthropic: []byte("sealed"),
	}
	svc := New(fs, newBox(t), testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

	judge, err := svc.RerunJudge(context.Background(), owner, false, target)
	if err != nil {
		t.Fatalf("RerunJudge: %v", err)
	}
	if judge.Kind != RunKindJudge || judge.UserID != owner {
		t.Errorf("judge run = %+v, want kind=judge owner=%v", judge, owner)
	}
	if fs.createdJudgeRun == nil {
		t.Fatal("expected a judge run to be minted")
	}
	if fs.createdJudgeRun.UserID != owner || uuid.UUID(fs.createdJudgeRun.TargetRunID.Bytes) != target {
		t.Errorf("minted judge scoped wrong: %+v", fs.createdJudgeRun)
	}
}
