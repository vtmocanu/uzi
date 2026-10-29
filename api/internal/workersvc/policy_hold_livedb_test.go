package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyHoldApplyReplayAndFencesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, _ := e.providerParkService(t, ppBound)
	wid := e.seedWorker(t, []string{capability.ProviderPolicyHoldV1})
	worker, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	id := e.fpSeedRunning(t, wid, 1)
	e.exec(t, "UPDATE runs SET harness='codex',session_id='original-session' WHERE id=$1", id)
	req := policyHoldFixture()
	got, err := svc.ReportPolicyHold(e.ctx, worker, id, req)
	if err != nil || got.Disposition != "applied" || got.ObservedStatus != "paused" {
		t.Fatalf("hold: %+v %v", got, err)
	}
	row, err := e.q.GetRunByIDForUser(e.ctx, store.GetRunByIDForUserParams{ID: id, UserID: e.userID})
	if err != nil {
		t.Fatal(err)
	}
	if !row.PolicyPreserveRequired || row.PolicyRefusalCount != 1 || row.ClaimReleasedAt.Valid || row.SessionID.String != "original-session" || row.PolicyRefusalTag.String != "cyberPolicy" {
		t.Fatalf("hold lost preservation: %+v", row)
	}
	got, err = svc.ReportPolicyHold(e.ctx, worker, id, req)
	if err != nil || got.Disposition != "already_applied" {
		t.Fatalf("replay: %+v %v", got, err)
	}
	// SQL itself refuses generic owner continuation, before queue/session mutation.
	_, err = e.q.ResumePausedRun(e.ctx, store.ResumePausedRunParams{ID: id, UserID: e.userID, AllowCompletionBlockedHold: true, GlobalTimeoutSeconds: 3600})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("generic resume: %v", err)
	}
	n, err := e.q.SetRunFailed(e.ctx, store.SetRunFailedParams{ID: id, WorkerID: pgconv.UUID(wid), FailOrigin: pgconv.Text("provider_policy_refusal"), ClaimGeneration: pgconv.Int8Ptr(&req.ClaimGeneration)})
	if err != nil || n != 0 {
		t.Fatalf("late failure overwrote hold: %d %v", n, err)
	}
	if _, err := svc.SubmitInput(e.ctx, e.userID, id, "stop", "", nil); !errors.Is(err, ErrProviderHoldStopUnavailable) {
		t.Fatalf("policy Stop should remain unavailable: %v", err)
	}
	// Model a future, independently authorized owner transition: replay must
	// recognize the old outcome without reopening or requeueing the episode.
	e.exec(t, "UPDATE runs SET status='queued',hold_reason=NULL,policy_resolved_at=now(),policy_preserve_required=false WHERE id=$1", id)
	got, err = svc.ReportPolicyHold(e.ctx, worker, id, req)
	if err != nil || got.Disposition != "already_applied" || got.ObservedStatus != "queued" {
		t.Fatalf("owner race: %+v %v", got, err)
	}
	replacement := e.seedWorker(t, []string{capability.ProviderPolicyHoldV1})
	e.exec(t, "UPDATE runs SET worker_id=$2 WHERE id=$1", id, replacement)
	got, err = svc.ReportPolicyHold(e.ctx, worker, id, req)
	if err != nil || got.Disposition != "already_applied" || got.ObservedStatus != "queued" {
		t.Fatalf("recorded worker replay after reassignment: %+v %v", got, err)
	}
	e.exec(t, "UPDATE runs SET worker_id=$2 WHERE id=$1", id, wid)
	e.exec(t, "UPDATE runs SET status='running',claim_generation=2 WHERE id=$1", id)
	next := policyHoldFixture()
	next.SourceGeneration = 2
	next.ClaimGeneration = 2
	got, err = svc.ReportPolicyHold(e.ctx, worker, id, next)
	if err != nil || got.Disposition != "applied" {
		t.Fatalf("new episode: %+v %v", got, err)
	}
	old := req
	old.OutcomeID = uuid.NewString()
	old.Detail = "resume_rejected"
	old.PolicyTag = nil
	old.Phase = nil
	old.CorrelationID = nil
	got, err = svc.ReportPolicyHold(e.ctx, worker, id, old)
	if err != nil || (got.Disposition != "resolved_newer_policy" || got.ApplyingGeneration != 2) {
		t.Fatalf("newer protects owner: %+v %v", got, err)
	}
}

func TestPolicyHoldAdmissionAndConversionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, _ := e.providerParkService(t, ppBound)
	wid := e.seedWorker(t, []string{capability.ProviderPolicyHoldV1})
	worker, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, status, kind, harness string
		interactive                 bool
		reason                      string
	}{
		{"approval", "awaiting_approval", "issue", "codex", false, "ineligible_status"},
		{"claude", "running", "issue", "claude", false, "ineligible_lane"},
		{"interactive", "running", "issue", "codex", true, "ineligible_lane"},
		{"other kind", "running", "ci_fix", "codex", false, "ineligible_lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := e.fpSeedRunning(t, wid, 3)
			e.exec(t, "UPDATE runs SET status=$2,kind=$3,harness=$4,interactive=$5 WHERE id=$1", id, tc.status, tc.kind, tc.harness, tc.interactive)
			r := policyHoldFixture()
			r.SourceGeneration = 3
			r.ClaimGeneration = 3
			got, err := svc.ReportPolicyHold(e.ctx, worker, id, r)
			if err != nil || got.Disposition != "pending" || got.Reason != tc.reason {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	id := e.fpSeedRunning(t, wid, 3)
	e.exec(t, "UPDATE runs SET harness='codex' WHERE id=$1", id)
	req := policyHoldFixture()
	got, err := svc.ReportPolicyHold(e.ctx, worker, id, req)
	if err != nil || got.Reason != "stale_claim" {
		t.Fatalf("stale: %+v %v", got, err)
	}
	req.ClaimGeneration = 3
	req.Detail = "resume_rejected"
	req.PolicyTag = nil
	req.Phase = nil
	req.CorrelationID = nil
	req.ContinuationMode = "fresh_from_saved_work"
	got, err = svc.ReportPolicyHold(e.ctx, worker, id, req)
	if err != nil || got.Disposition != "applied" {
		t.Fatalf("current conversion: %+v %v", got, err)
	}
	row, err := e.q.GetRunByIDForUser(e.ctx, store.GetRunByIDForUserParams{ID: id, UserID: e.userID})
	if err != nil {
		t.Fatal(err)
	}
	if row.PolicyRefusalCount != 0 || row.PolicyRefusalTag.Valid || row.PolicySourceGeneration.Int64 != 1 || row.PolicyApplyingGeneration.Int64 != 3 {
		t.Fatalf("conversion fabricated refusal: %+v", row)
	}
}
