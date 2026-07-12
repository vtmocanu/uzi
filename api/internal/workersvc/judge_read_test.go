package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
