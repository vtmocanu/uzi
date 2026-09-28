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

// capabilityWorker advertises advice_claim_fence_v1, the capability the stamping requirement
// is keyed on (alongside credential_switch_v1, which every such worker also advertises).
func capabilityWorker() store.Worker {
	w := worker()
	w.ProtocolCapabilities = []string{capability.CredentialSwitchV1, capability.AdviceClaimFenceV1}
	return w
}

// credentialSwitchOnlyWorker is a worker image from before advice posts were stamped: it
// advertises credential_switch_v1 (shipped earlier) but not advice_claim_fence_v1, and posts
// advice without claim_generation or advice_run_id. An api upgraded ahead of it must accept.
func credentialSwitchOnlyWorker() store.Worker {
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
		ReviewSubmission{Verdict: "ok", Status: "complete"}, AdviceClaim{Generation: i64(7), RunID: &judgeID}); err != nil {
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
		ReviewSubmission{Verdict: "ok", Status: "complete"}, AdviceClaim{}); err != nil {
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
	}, AdviceClaim{Generation: i64(3), RunID: &judgeID})
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
		ReviewSubmission{Verdict: "ok", Status: "complete"}, AdviceClaim{})
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
		ReviewSubmission{Verdict: "ok", Status: "complete"}, AdviceClaim{})
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
		TaskReviewSubmission{Status: "complete"}, AdviceClaim{Generation: i64(9), RunID: &reviewID}); err != nil {
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
	err := svc.PostTaskReview(context.Background(), capabilityWorker(), target, TaskReviewSubmission{Status: "complete"}, AdviceClaim{Generation: i64(2), RunID: &reviewID})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
}

func TestPostTaskReviewCapabilityWorkerMissingGeneration(t *testing.T) {
	owner, reviewID, target := uuid.New(), uuid.New(), uuid.New()
	fs := taskReviewFenceStore(owner, reviewID, target)
	svc := New(fs, newBox(t), testParams())
	err := svc.PostTaskReview(context.Background(), capabilityWorker(), target, TaskReviewSubmission{Status: "complete"}, AdviceClaim{})
	if !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("err = %v, want ErrMissingClaimGeneration", err)
	}
	if fs.upsertTaskReviewParams != nil {
		t.Fatal("a capability worker's unstamped post must write nothing")
	}
}

func TestPostTaskReviewUnauthorizedBeatsMissingGeneration(t *testing.T) {
	fs := &fakeStore{activeTaskReviewRunErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	err := svc.PostTaskReview(context.Background(), capabilityWorker(), uuid.New(),
		TaskReviewSubmission{Status: "complete"}, AdviceClaim{})
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound (authorization is checked first)", err)
	}
	if fs.upsertTaskReviewParams != nil {
		t.Fatal("an unauthorized post must write nothing")
	}
}

// Issue #1423 advice_run_id: generations are per-run counters, so the flight also stamps the
// advice run it holds. A stamped id that is not the authorized (active) advice run is a stale
// flight of an earlier run and is refused before any write; the matching id passes, and the
// posting worker's id reaches the in-statement ownership fence.
func TestAdviceRunIDFence(t *testing.T) {
	type lane struct {
		name string
		// post runs the lane against a fresh store authorizing adviceID and reports whether
		// the upsert was reached, plus the WorkerID the upsert received.
		post func(t *testing.T, wkr store.Worker, adviceID uuid.UUID, claim AdviceClaim) (wrote bool, gotWorker uuid.UUID, err error)
	}
	lanes := []lane{
		{"judge review", func(t *testing.T, wkr store.Worker, adviceID uuid.UUID, claim AdviceClaim) (bool, uuid.UUID, error) {
			target := uuid.New()
			fs := judgeFenceStore(wkr.UserID, adviceID, target)
			_, err := New(fs, newBox(t), testParams()).PostReview(context.Background(), wkr, target,
				ReviewSubmission{Verdict: "ok", Status: "complete"}, claim)
			if fs.upsertedReview == nil {
				return false, uuid.Nil, err
			}
			return true, fs.upsertedReview.WorkerID, err
		}},
		{"task review", func(t *testing.T, wkr store.Worker, adviceID uuid.UUID, claim AdviceClaim) (bool, uuid.UUID, error) {
			target := uuid.New()
			fs := taskReviewFenceStore(wkr.UserID, adviceID, target)
			err := New(fs, newBox(t), testParams()).PostTaskReview(context.Background(), wkr, target,
				TaskReviewSubmission{Status: "complete"}, claim)
			if fs.upsertTaskReviewParams == nil {
				return false, uuid.Nil, err
			}
			return true, fs.upsertTaskReviewParams.WorkerID, err
		}},
	}
	for _, l := range lanes {
		t.Run(l.name+"/mismatched id is stale", func(t *testing.T) {
			other := uuid.New()
			wrote, _, err := l.post(t, capabilityWorker(), uuid.New(), AdviceClaim{Generation: i64(1), RunID: &other})
			if !errors.Is(err, ErrStaleClaim) || wrote {
				t.Fatalf("err = %v wrote = %v, want ErrStaleClaim and no write", err, wrote)
			}
		})
		t.Run(l.name+"/missing generation beats a mismatched id", func(t *testing.T) {
			other := uuid.New()
			wrote, _, err := l.post(t, capabilityWorker(), uuid.New(), AdviceClaim{RunID: &other})
			if !errors.Is(err, ErrMissingClaimGeneration) || wrote {
				t.Fatalf("err = %v wrote = %v, want ErrMissingClaimGeneration and no write", err, wrote)
			}
		})
		// A capability worker that stamps the generation but OMITS the advice run id would let
		// a stale post of ended run A (same target, same per-run generation) resolve to the
		// active run B, so the omission is refused exactly like a missing generation.
		t.Run(l.name+"/capability worker missing run id is refused", func(t *testing.T) {
			wrote, _, err := l.post(t, capabilityWorker(), uuid.New(), AdviceClaim{Generation: i64(1)})
			if !errors.Is(err, ErrMissingClaimGeneration) || wrote {
				t.Fatalf("err = %v wrote = %v, want ErrMissingClaimGeneration and no write", err, wrote)
			}
		})
		// Rolling-upgrade regression: credential_switch_v1 shipped before advice posts were
		// stamped, so keying the requirement on it would 409 every current worker's post.
		t.Run(l.name+"/credential_switch_v1-only worker may omit both fields", func(t *testing.T) {
			wkr := credentialSwitchOnlyWorker()
			wrote, gotWorker, err := l.post(t, wkr, uuid.New(), AdviceClaim{})
			if err != nil || !wrote {
				t.Fatalf("err = %v wrote = %v, want the unstamped post to reach the upsert", err, wrote)
			}
			if gotWorker != wkr.ID {
				t.Fatalf("upsert worker_id = %v, want the posting worker %v (SQL ownership fence)", gotWorker, wkr.ID)
			}
		})
		t.Run(l.name+"/credential_switch_v1-only worker mismatched id is still stale", func(t *testing.T) {
			other := uuid.New()
			wrote, _, err := l.post(t, credentialSwitchOnlyWorker(), uuid.New(), AdviceClaim{Generation: i64(1), RunID: &other})
			if !errors.Is(err, ErrStaleClaim) || wrote {
				t.Fatalf("err = %v wrote = %v, want ErrStaleClaim and no write", err, wrote)
			}
		})
		t.Run(l.name+"/legacy worker may omit the run id", func(t *testing.T) {
			wrote, _, err := l.post(t, worker(), uuid.New(), AdviceClaim{})
			if err != nil || !wrote {
				t.Fatalf("err = %v wrote = %v, want the legacy post to reach the upsert", err, wrote)
			}
		})
		t.Run(l.name+"/matching id lands with the posting worker fenced", func(t *testing.T) {
			adviceID := uuid.New()
			wkr := capabilityWorker()
			wrote, gotWorker, err := l.post(t, wkr, adviceID, AdviceClaim{Generation: i64(1), RunID: &adviceID})
			if err != nil || !wrote {
				t.Fatalf("err = %v wrote = %v, want a write", err, wrote)
			}
			if gotWorker != wkr.ID {
				t.Fatalf("upsert worker_id = %v, want the posting worker %v", gotWorker, wkr.ID)
			}
		})
	}
}
