package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1423 unit coverage: the advice posts hand the fenced upsert the ADVICE run's id and
// the stamped generation, map the upsert's no-row (fenced-out) outcome to ErrStaleClaim, and
// refuse a capability worker's unstamped post before any write. The live-DB suite
// (advice_claim_fence_livedb_test.go) runs the real SQL fence.

func capabilityWorker() store.Worker {
	w := worker()
	w.ProtocolCapabilities = []string{capability.CredentialSwitchV1}
	return w
}

func judgeFenceStore(owner, judgeID, target uuid.UUID) *fakeStore {
	return &fakeStore{
		activeJudgeRun: store.Run{ID: judgeID, UserID: owner, Kind: runkind.Judge},
		runByIDPlain:   store.Run{ID: target, UserID: owner, Kind: runkind.Issue, Status: "completed"},
	}
}

func TestPostReviewPassesAdviceRunAndClaimGeneration(t *testing.T) {
	owner, judgeID, target := uuid.New(), uuid.New(), uuid.New()
	fs := judgeFenceStore(owner, judgeID, target)
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.PostReview(context.Background(), capabilityWorker(), target,
		ReviewSubmission{Verdict: "ok", Status: "complete"}, i64(7)); err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	got := fs.upsertedReview
	if got == nil {
		t.Fatal("expected a review upsert")
	}
	if !got.JudgeRunID.Valid || uuid.UUID(got.JudgeRunID.Bytes) != judgeID {
		t.Errorf("fence judge_run_id = %v, want the judge run %v", got.JudgeRunID, judgeID)
	}
	if !got.ClaimGeneration.Valid || got.ClaimGeneration.Int64 != 7 {
		t.Errorf("fence claim_generation = %+v, want 7", got.ClaimGeneration)
	}
}

func TestPostReviewLegacyNilGenerationIsUnstamped(t *testing.T) {
	owner, judgeID, target := uuid.New(), uuid.New(), uuid.New()
	fs := judgeFenceStore(owner, judgeID, target)
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.PostReview(context.Background(), worker(), target,
		ReviewSubmission{Verdict: "ok", Status: "complete"}, nil); err != nil {
		t.Fatalf("legacy PostReview: %v", err)
	}
	if fs.upsertedReview == nil || fs.upsertedReview.ClaimGeneration.Valid {
		t.Fatalf("legacy post must reach the upsert with a NULL generation, got %+v", fs.upsertedReview)
	}
	if !fs.upsertedReview.JudgeRunID.Valid {
		t.Fatal("legacy post must still pass the judge run id (the released-claim fence)")
	}
}

func TestPostReviewFencedOutIsStaleClaim(t *testing.T) {
	owner, judgeID, target := uuid.New(), uuid.New(), uuid.New()
	fs := judgeFenceStore(owner, judgeID, target)
	fs.upsertReviewErr = pgx.ErrNoRows
	svc := New(fs, newBox(t), testParams())
	_, err := svc.PostReview(context.Background(), capabilityWorker(), target, ReviewSubmission{
		Verdict: "issues", Status: "complete",
		Recommendations: []ReviewRecommendation{{Category: "enable_tool", Target: "gh", RationaleMd: "r"}},
	}, i64(3))
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
	if len(fs.systemDismissed) != 0 {
		t.Fatalf("a fenced-out review must not run the auto-dismiss net, got %d dismissals", len(fs.systemDismissed))
	}
}

func TestPostReviewCapabilityWorkerMissingGeneration(t *testing.T) {
	owner, judgeID, target := uuid.New(), uuid.New(), uuid.New()
	fs := judgeFenceStore(owner, judgeID, target)
	svc := New(fs, newBox(t), testParams())
	_, err := svc.PostReview(context.Background(), capabilityWorker(), target,
		ReviewSubmission{Verdict: "ok", Status: "complete"}, nil)
	if !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("err = %v, want ErrMissingClaimGeneration", err)
	}
	if fs.upsertedReview != nil {
		t.Fatal("a capability worker's unstamped post must write nothing")
	}
}

func TestPostReviewUnauthorizedBeatsMissingGeneration(t *testing.T) {
	fs := &fakeStore{activeJudgeRunErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.PostReview(context.Background(), capabilityWorker(), uuid.New(),
		ReviewSubmission{Verdict: "ok", Status: "complete"}, nil)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound (authorization is checked first)", err)
	}
}

func taskReviewFenceStore(owner, reviewID, target uuid.UUID) *fakeStore {
	return &fakeStore{
		activeTaskReviewRun: store.Run{ID: reviewID, UserID: owner, Kind: runkind.Task},
		runByIDPlain:        store.Run{ID: target, UserID: owner, Kind: runkind.Task, Status: "completed"},
		upsertTaskReviewID:  uuid.New(),
	}
}

func TestPostTaskReviewPassesAdviceRunAndClaimGeneration(t *testing.T) {
	owner, reviewID, target := uuid.New(), uuid.New(), uuid.New()
	fs := taskReviewFenceStore(owner, reviewID, target)
	svc := New(fs, newBox(t), testParams())
	if err := svc.PostTaskReview(context.Background(), capabilityWorker(), target,
		TaskReviewSubmission{Status: "complete"}, i64(9)); err != nil {
		t.Fatalf("PostTaskReview: %v", err)
	}
	got := fs.upsertTaskReviewParams
	if got == nil {
		t.Fatal("expected a task-review upsert")
	}
	if !got.ReviewRunID.Valid || uuid.UUID(got.ReviewRunID.Bytes) != reviewID {
		t.Errorf("fence review_run_id = %v, want the review run %v", got.ReviewRunID, reviewID)
	}
	if !got.ClaimGeneration.Valid || got.ClaimGeneration.Int64 != 9 {
		t.Errorf("fence claim_generation = %+v, want 9", got.ClaimGeneration)
	}
}

func TestPostTaskReviewFencedOutIsStaleClaim(t *testing.T) {
	owner, reviewID, target := uuid.New(), uuid.New(), uuid.New()
	fs := taskReviewFenceStore(owner, reviewID, target)
	fs.upsertTaskReviewErr = pgx.ErrNoRows
	svc := New(fs, newBox(t), testParams())
	err := svc.PostTaskReview(context.Background(), capabilityWorker(), target, TaskReviewSubmission{Status: "complete"}, i64(2))
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
}

func TestPostTaskReviewCapabilityWorkerMissingGeneration(t *testing.T) {
	owner, reviewID, target := uuid.New(), uuid.New(), uuid.New()
	fs := taskReviewFenceStore(owner, reviewID, target)
	svc := New(fs, newBox(t), testParams())
	err := svc.PostTaskReview(context.Background(), capabilityWorker(), target, TaskReviewSubmission{Status: "complete"}, nil)
	if !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("err = %v, want ErrMissingClaimGeneration", err)
	}
	if fs.upsertTaskReviewParams != nil {
		t.Fatal("a capability worker's unstamped post must write nothing")
	}
}
