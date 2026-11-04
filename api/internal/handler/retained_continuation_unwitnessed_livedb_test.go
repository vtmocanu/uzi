package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #3859: a retained custody continuation the sweep parked at
// recovery_wait/retained_continuation_unwitnessed is OWNER-HELD. These tests drive the owner
// routes (Resume and the cancel steering input) and the two server-side promoters against the
// real SQL. The park is seeded with the exact shape the sweep writes (claim released to the
// worker incarnation, worker kept, no retry stamp, an earlier-generation hold still open); the
// sweep that writes it is covered in the workersvc LiveDB suite.

type unwitnessedParkFixture struct {
	fx     *ephemeralFixture
	worker uuid.UUID
	run    uuid.UUID
	holds  []uuid.UUID
}

func seedUnwitnessedPark(t *testing.T) *unwitnessedParkFixture {
	t.Helper()
	fx := newEphemeralFixture(t, false)
	t.Cleanup(func() {
		if _, err := fx.pool.Exec(fx.ctx, "DELETE FROM recovery_captures WHERE user_id=$1", fx.userID); err != nil {
			t.Errorf("cleanup captures: %v", err)
		}
		if _, err := fx.pool.Exec(fx.ctx, "DELETE FROM recovery_custody_holds WHERE user_id=$1", fx.userID); err != nil {
			t.Errorf("cleanup holds: %v", err)
		}
		if _, err := fx.pool.Exec(fx.ctx, "DELETE FROM users WHERE id=$1", fx.userID); err != nil {
			t.Errorf("cleanup owner: %v", err)
		}
	})
	worker := fx.onlineWorker("unwitnessed park", true)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',
        recovery_wait_cause='retained_continuation_unwitnessed',claim_generation=2,worker_id=$2,
        claim_released_at=now(),released_worker_id=$2,released_worker_nonce='parked',
        requeue_count=2,requeue_episode_baseline=0,worker_recovery_episode=0,
        budget_wall_seconds=86400,started_at=now()-interval '1 hour',status_since=now()-interval '2 minutes',
        recovery_retry_not_before=NULL WHERE id=$1`, id, worker); err != nil {
		t.Fatal(err)
	}
	p := &unwitnessedParkFixture{fx: fx, worker: worker, run: id}
	for gen := 1; gen <= 2; gen++ {
		hold := uuid.New()
		if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO recovery_custody_holds
            (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
            VALUES ($1,$2,$3,$4,$5,'open',$6,'parked incarnation',$6,$4)`, hold, fx.userID, fx.repoID, id, gen, worker); err != nil {
			t.Fatal(err)
		}
		p.holds = append(p.holds, hold)
	}
	return p
}

func (p *unwitnessedParkFixture) handler() *Handler {
	h := &Handler{q: p.fx.q, wsvc: workersvc.New(p.fx.q, nil, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(),
		RunTimeout:       2 * time.Hour, RunWallCeiling: 8 * time.Hour,
		WorkerHeartbeatStale: time.Minute,
	})}
	h.cfg.RunTimeout = 2 * time.Hour
	return h
}

func (p *unwitnessedParkFixture) get(t *testing.T) store.Run {
	t.Helper()
	r, err := p.fx.q.GetRunByID(p.fx.ctx, p.run)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (p *unwitnessedParkFixture) holdStates(t *testing.T) string {
	t.Helper()
	var s string
	if err := p.fx.pool.QueryRow(p.fx.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.generation)::text, '[]') FROM recovery_custody_holds h WHERE h.run_id=$1`, p.run).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// custodySnapshot is every server-side custody table for the run, serialised in a stable order:
// holds, captures, capture chunks and prerequisite closures. A Cancel must leave it byte for byte
// unchanged (no release, discard or deletion).
func (p *unwitnessedParkFixture) custodySnapshot(t *testing.T) string {
	t.Helper()
	var s string
	q := `SELECT jsonb_build_object(
        'holds', COALESCE((SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM recovery_custody_holds h WHERE h.run_id=$1), '[]'::jsonb),
        'captures', COALESCE((SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM recovery_captures c WHERE c.run_id=$1), '[]'::jsonb),
        'chunks', COALESCE((SELECT jsonb_agg(to_jsonb(k) ORDER BY k.capture_id, k.chunk_index) FROM recovery_capture_chunks k
            JOIN recovery_captures c ON c.id=k.capture_id WHERE c.run_id=$1), '[]'::jsonb),
        'closures', COALESCE((SELECT jsonb_agg(to_jsonb(z) ORDER BY z.id) FROM recovery_prerequisite_closures z WHERE z.run_id=$1), '[]'::jsonb)
    )::text`
	if err := p.fx.pool.QueryRow(p.fx.ctx, q, p.run).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// cancelOwner sends the owner's cancel steering input and requires the server-side 202.
func (p *unwitnessedParkFixture) cancelOwner(t *testing.T, h *Handler) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(store.User{ID: p.fx.userID, IsActive: true}, p.run, `{"kind":"cancel","body":"end it"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner Cancel = %d %s", rec.Code, rec.Body.String())
	}
}

// TestUnwitnessedParkOwnerResumeLiveDB: the owner's Resume moves the park to queued with a new
// worker-recovery episode, keeps worker affinity (the retained source lives on that worker's
// disk) and the open holds; a foreign user is refused and nothing changes; a second Resume is a
// 409.
func TestUnwitnessedParkOwnerResumeLiveDB(t *testing.T) {
	p := seedUnwitnessedPark(t)
	h := p.handler()
	held := p.get(t)
	holdsBefore := p.holdStates(t)

	if rec := recoveryResumeRequest(h, p.run, uuid.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign Resume = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if after := p.get(t); !reflect.DeepEqual(after, held) {
		t.Fatalf("foreign Resume changed the parked run: %+v", after)
	}

	rec := recoveryResumeRequest(h, p.run, p.fx.userID)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner Resume = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Run.Status != "queued" {
		t.Fatalf("response status = %q, want queued", body.Run.Status)
	}
	r := p.get(t)
	if r.Status != "queued" || r.RecoveryWaitCause.Valid || r.RecoveryRetryNotBefore.Valid {
		t.Fatalf("resumed run status=%s cause=%v retry=%v, want queued with the park cleared", r.Status, r.RecoveryWaitCause, r.RecoveryRetryNotBefore)
	}
	if r.WorkerRecoveryEpisode != held.WorkerRecoveryEpisode+1 || r.RequeueEpisodeBaseline != held.RequeueCount {
		t.Fatalf("episode=%d baseline=%d, want episode %d and baseline advanced to requeue_count %d",
			r.WorkerRecoveryEpisode, r.RequeueEpisodeBaseline, held.WorkerRecoveryEpisode+1, held.RequeueCount)
	}
	if r.RequeueCount != held.RequeueCount || r.ClaimGeneration != held.ClaimGeneration {
		t.Fatalf("Resume changed requeue_count/claim_generation: %d/%d", r.RequeueCount, r.ClaimGeneration)
	}
	if r.WorkerID != held.WorkerID || !r.WorkerID.Valid {
		t.Fatalf("worker_id = %v, want affinity kept (%v)", r.WorkerID, held.WorkerID)
	}
	if !r.ClaimReleasedAt.Valid || r.ReleasedWorkerID != held.ReleasedWorkerID || r.ReleasedWorkerNonce != held.ReleasedWorkerNonce {
		t.Fatalf("Resume changed the released-claim fence: %+v", r)
	}
	if r.StartedAt != held.StartedAt || r.BudgetWallSeconds != held.BudgetWallSeconds {
		t.Fatalf("Resume changed the wall budget: %+v", r)
	}
	if got := p.holdStates(t); got != holdsBefore {
		t.Fatalf("Resume changed custody holds:\n%s\n%s", holdsBefore, got)
	}
	if rec := recoveryResumeRequest(h, p.run, p.fx.userID); rec.Code != http.StatusConflict {
		t.Fatalf("second Resume = %d %s, want 409", rec.Code, rec.Body.String())
	}
}

// TestUnwitnessedParkOwnerCancelLiveDB: Cancel is applied server-side (a recovery_wait run has no
// live poller even though its worker heartbeats), ends the run, and leaves every hold and capture
// byte for byte as it was. A foreign user, an admin included, is refused.
func TestUnwitnessedParkOwnerCancelLiveDB(t *testing.T) {
	p := seedUnwitnessedPark(t)
	h := p.handler()
	foreign := store.User{ID: uuid.New(), IsActive: true}
	admin := store.User{ID: uuid.New(), IsActive: true, IsAdmin: true}
	t.Cleanup(func() {
		if _, err := p.fx.pool.Exec(p.fx.ctx, "DELETE FROM users WHERE id IN ($1,$2)", foreign.ID, admin.ID); err != nil {
			t.Errorf("cleanup users: %v", err)
		}
	})
	for _, u := range []store.User{foreign, admin} {
		if _, err := p.fx.pool.Exec(p.fx.ctx, "INSERT INTO users (id,email,password_hash,is_admin) VALUES ($1,$2,'x',$3)", u.ID, "unwitnessed-"+u.ID.String()+"@e2e", u.IsAdmin); err != nil {
			t.Fatal(err)
		}
	}
	capture := uuid.New()
	if _, err := p.fx.pool.Exec(p.fx.ctx, `INSERT INTO recovery_captures
        (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state)
        VALUES ($1,$2,$3,$4,$5,'parked incarnation','H0','k','needs_action')`, capture, p.holds[0], p.run, p.fx.userID, p.worker); err != nil {
		t.Fatal(err)
	}
	var capsBefore string
	capsQuery := `SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id)::text, '[]') FROM recovery_captures c WHERE c.run_id=$1`
	if err := p.fx.pool.QueryRow(p.fx.ctx, capsQuery, p.run).Scan(&capsBefore); err != nil {
		t.Fatal(err)
	}
	held := p.get(t)
	holdsBefore := p.holdStates(t)
	custodyBefore := p.custodySnapshot(t)

	for _, u := range []store.User{foreign, admin} {
		rec := httptest.NewRecorder()
		h.CreateRunInput(rec, inputReq(u, p.run, `{"kind":"cancel"}`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("non-owner Cancel (admin=%t) = %d %s", u.IsAdmin, rec.Code, rec.Body.String())
		}
		if after := p.get(t); !reflect.DeepEqual(after, held) {
			t.Fatalf("non-owner Cancel (admin=%t) changed the parked run", u.IsAdmin)
		}
	}

	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(store.User{ID: p.fx.userID, IsActive: true}, p.run, `{"kind":"cancel","body":"end the park"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner Cancel = %d %s", rec.Code, rec.Body.String())
	}
	var resp apitypes.RunInputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.ServerSide {
		t.Fatalf("owner Cancel response = %+v, want server-side", resp)
	}
	r := p.get(t)
	if r.Status != "cancelled" || !r.FinishedAt.Valid || !r.StopKind.Valid || r.StopKind.String != "cancelled" {
		t.Fatalf("owner Cancel did not end the run: status=%s finished=%v stop=%v", r.Status, r.FinishedAt.Valid, r.StopKind)
	}
	if r.WorkerID != held.WorkerID || r.ClaimReleasedAt != held.ClaimReleasedAt || r.ReleasedWorkerID != held.ReleasedWorkerID {
		t.Fatalf("Cancel changed the worker binding or the released-claim fence: %+v", r)
	}
	if got := p.holdStates(t); got != holdsBefore {
		t.Fatalf("Cancel changed custody holds:\n%s\n%s", holdsBefore, got)
	}
	if got := p.custodySnapshot(t); got != custodyBefore {
		t.Fatalf("Cancel changed server-side custody rows:\n%s\n%s", custodyBefore, got)
	}
	var capsAfter string
	if err := p.fx.pool.QueryRow(p.fx.ctx, capsQuery, p.run).Scan(&capsAfter); err != nil || capsAfter != capsBefore {
		t.Fatalf("Cancel changed captures (%v):\n%s\n%s", err, capsBefore, capsAfter)
	}
	var inputs int
	if err := p.fx.pool.QueryRow(p.fx.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1", p.run).Scan(&inputs); err != nil || inputs != 0 {
		t.Fatalf("Cancel left %d steering inputs for an absent poller (%v)", inputs, err)
	}
}

// TestUnwitnessedParkPromotersLeaveItAloneLiveDB: neither the timer promoter nor the owner's
// credential-switch promote releases the park, even with a retry stamp in the past. The
// forge_unreachable controls prove the same statements DO promote a run in the same shape, so
// a fence regression turns this red.
func TestUnwitnessedParkPromotersLeaveItAloneLiveDB(t *testing.T) {
	p := seedUnwitnessedPark(t)
	fx := p.fx
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET recovery_retry_not_before = now() - interval '1 hour' WHERE id=$1`, p.run); err != nil {
		t.Fatal(err)
	}
	held := p.get(t)
	if !held.RecoveryRetryNotBefore.Valid {
		t.Fatal("precondition: the stamp was not written")
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	rows, err := fx.q.PromoteRecoveryWaitRuns(fx.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == p.run {
			t.Fatalf("PromoteRecoveryWaitRuns promoted the unwitnessed park")
		}
	}
	if n, err := fx.q.PromoteRecoveryWaitRunNow(fx.ctx, store.PromoteRecoveryWaitRunNowParams{ID: p.run, UserID: fx.userID}); err != nil || n != 0 {
		t.Fatalf("PromoteRecoveryWaitRunNow rows=%d err=%v, want 0", n, err)
	}
	if after := p.get(t); !reflect.DeepEqual(after, held) {
		t.Fatalf("a promoter changed the parked run: %+v", after)
	}

	// Controls: the same shape under a cause that is NOT fenced is promoted by both statements.
	for _, name := range []string{"timer", "owner"} {
		ctl := fx.queuedRun([]string{})
		if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait', recovery_wait_cause='forge_unreachable',
            claim_generation=1, worker_id=$2, started_at=now()-interval '1 hour', recovery_retry_not_before=now()-interval '1 hour' WHERE id=$1`, ctl, p.worker); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "timer":
			rows, err := fx.q.PromoteRecoveryWaitRuns(fx.ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				found = found || row.ID == ctl
			}
			if !found {
				t.Fatal("control: PromoteRecoveryWaitRuns did not promote a forge_unreachable park")
			}
		case "owner":
			if n, err := fx.q.PromoteRecoveryWaitRunNow(fx.ctx, store.PromoteRecoveryWaitRunNowParams{ID: ctl, UserID: fx.userID}); err != nil || n != 1 {
				t.Fatalf("control: PromoteRecoveryWaitRunNow rows=%d err=%v, want 1", n, err)
			}
		}
	}
}

// TestChargedQueuedContinuationOwnerCancelLiveDB: a continuation the claimed-never-started
// charge pass put back to queued (requeue_count + 1, claim NOT released, worker kept, Codex
// capability revoked, holds untouched) is ended by the owner's Cancel with every custody row
// byte for byte as it was. The row is seeded with the shape the charge statement writes.
func TestChargedQueuedContinuationOwnerCancelLiveDB(t *testing.T) {
	p := seedUnwitnessedPark(t)
	h := p.handler()
	if _, err := p.fx.pool.Exec(p.fx.ctx, `UPDATE runs SET status='queued', recovery_wait_cause=NULL,
        claim_released_at=NULL, released_worker_id=NULL, released_worker_nonce=NULL,
        requeue_count=3, codex_cap_hash=NULL, codex_claim_epoch=codex_claim_epoch+1,
        status_since=now() WHERE id=$1`, p.run); err != nil {
		t.Fatal(err)
	}
	capture := uuid.New()
	if _, err := p.fx.pool.Exec(p.fx.ctx, `INSERT INTO recovery_captures
        (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state)
        VALUES ($1,$2,$3,$4,$5,'parked incarnation','H0','k','needs_action')`, capture, p.holds[0], p.run, p.fx.userID, p.worker); err != nil {
		t.Fatal(err)
	}
	held := p.get(t)
	if held.Status != "queued" || held.RequeueCount != 3 || !held.WorkerID.Valid {
		t.Fatalf("precondition: status=%s requeue_count=%d worker=%v", held.Status, held.RequeueCount, held.WorkerID)
	}
	custodyBefore := p.custodySnapshot(t)

	p.cancelOwner(t, h)

	r := p.get(t)
	if r.Status != "cancelled" || !r.FinishedAt.Valid {
		t.Fatalf("owner Cancel did not end the charged queued run: status=%s finished=%v", r.Status, r.FinishedAt.Valid)
	}
	if r.RequeueCount != held.RequeueCount || r.WorkerID != held.WorkerID {
		t.Fatalf("Cancel changed requeue_count/worker_id: %d/%v", r.RequeueCount, r.WorkerID)
	}
	if got := p.custodySnapshot(t); got != custodyBefore {
		t.Fatalf("Cancel changed server-side custody rows:\n%s\n%s", custodyBefore, got)
	}
}
