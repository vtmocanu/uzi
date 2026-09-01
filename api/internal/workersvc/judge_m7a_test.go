package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #69 M7a Pass B: the judge CONSUMES runs.fail_origin. Two consumers are pinned
// here — the failure-CLASS signal on the judge claim (from the reviewed run's trusted
// fail_origin), and the pre-start-infra gate that skips the judge entirely for a run
// that failed before the agent did anything reviewable.

// TestJudgeClaimCarriesFailureClass: assembleJudgeClaim reads the TARGET run's
// fail_origin (via GetRunByID) and surfaces it as ClaimPayload.FailureClass — the enum
// VALUE ONLY. A NULL fail_origin (the default target row) leaves it nil, so a claim for
// a run with no recognised origin carries no class.
func TestJudgeClaimCarriesFailureClass(t *testing.T) {
	t.Run("provisioning_failed target surfaces the class", func(t *testing.T) {
		box := newBox(t)
		sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
		uid, target := uuid.New(), uuid.New()
		fs := &fakeStore{
			claimRun:     judgeRun(uid, target),
			anthropic:    sealedTok,
			runByIDPlain: store.Run{ID: target, FailOrigin: pgconv.TextOrNull("provisioning_failed")},
		}
		svc := New(fs, box, testParams())
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

		payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
		if err != nil || payload == nil {
			t.Fatalf("Claim: payload=%v err=%v", payload, err)
		}
		if payload.FailureClass == nil || *payload.FailureClass != "provisioning_failed" {
			t.Errorf("FailureClass = %v, want provisioning_failed", payload.FailureClass)
		}
	})

	t.Run("NULL fail_origin leaves the class nil", func(t *testing.T) {
		box := newBox(t)
		sealedTok, _ := box.Seal([]byte("anthropic-judge-token-abcdef1234567890"))
		uid, target := uuid.New(), uuid.New()
		fs := &fakeStore{
			claimRun:  judgeRun(uid, target),
			anthropic: sealedTok,
			// runByIDPlain default zero value ⇒ FailOrigin invalid (SQL NULL).
			runByIDPlain: store.Run{ID: target},
		}
		svc := New(fs, box, testParams())
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})

		payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
		if err != nil || payload == nil {
			t.Fatalf("Claim: payload=%v err=%v", payload, err)
		}
		if payload.FailureClass != nil {
			t.Errorf("FailureClass = %v, want nil for a NULL fail_origin", *payload.FailureClass)
		}
	})
}

// TestPreStartInfraFailureSkipsJudge (Decision 12): a failed run at iteration_count == 0
// whose fail_origin is one of the three pre-start policy/config-denied origins is NOT
// judged — there is no agent behavior to retrospect, and skipping avoids the most
// expensive per-run call. The deterministic failure notification is delivered separately
// by RunFailureNotifier on the same PublishState transition (asserted by trace, not here).
func TestPreStartInfraFailureSkipsJudge(t *testing.T) {
	for _, origin := range []string{"provisioning_failed", "credential_unavailable", "guardrail_blocked"} {
		t.Run(origin+" at iter 0 is not enqueued", func(t *testing.T) {
			fs, svc, run := eligibleFixture(t)
			run.Status = "failed"
			run.IterationCount = 0
			run.FailOrigin = pgconv.TextOrNull(origin)
			svc.maybeEnqueueJudge(context.Background(), run)
			if fs.createdJudgeRun != nil {
				t.Fatalf("%s at iteration 0 is a pre-start infra failure and must NOT be judged, got %+v", origin, fs.createdJudgeRun)
			}
		})
	}
}

// TestLiveCancelledRunNotJudged (PRD #503 M1, REC A regression): after M1 a live-worker
// cancel routes to a `cancelled` terminal status (not `failed`+`agent_failure`), so Gate 0
// (maybeEnqueueJudge, judge_enqueue.go:62 — only completed/failed are judged) excludes it.
// This pins that "cancelled runs are not judged" holds uniformly for the live path now, not
// just the server-side cancel path. (Also covered by TestEnqueueJudgeGatesBlock's "cancelled
// status" case; kept here as the named REC A regression.)
func TestLiveCancelledRunNotJudged(t *testing.T) {
	fs, svc, run := eligibleFixture(t)
	run.Status = "cancelled" // what CancelRunByWorker now writes for a live cancel
	run.IterationCount = 0
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun != nil {
		t.Fatalf("a cancelled run must NOT be enqueued for judging (Gate 0), got %+v", fs.createdJudgeRun)
	}
}

// TestAgentFailureAtIterZeroStillJudged (SC3): an agent that started and crashed at
// iteration 0 carries fail_origin='agent_failure' (the worker-reported default), which
// is NOT in the pre-start set — so it is still judged.
func TestAgentFailureAtIterZeroStillJudged(t *testing.T) {
	fs, svc, run := eligibleFixture(t)
	run.Status = "failed"
	run.IterationCount = 0
	run.FailOrigin = pgconv.TextOrNull("agent_failure")
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun == nil {
		t.Fatal("agent_failure at iteration 0 is judgeable (not pre-start infra) and must still be enqueued")
	}
}

// TestPreStartInfraOriginAtNonZeroIterStillJudged: the conjunction matters. A
// provisioning_failed origin at iteration_count > 0 is NOT pre-start-gated — the run got
// far enough to have behavior worth reviewing — so it is still enqueued.
func TestPreStartInfraOriginAtNonZeroIterStillJudged(t *testing.T) {
	fs, svc, run := eligibleFixture(t)
	run.Status = "failed"
	run.IterationCount = 3
	run.FailOrigin = pgconv.TextOrNull("provisioning_failed")
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun == nil {
		t.Fatal("a provisioning_failed origin at iteration_count>0 is not pre-start-gated (conjunction) and must be enqueued")
	}
}
