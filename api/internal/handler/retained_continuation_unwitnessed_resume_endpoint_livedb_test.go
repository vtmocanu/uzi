package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUnwitnessedParkResumeRouteLiveDB drives the owner's Resume through the REAL router (a uzc_
// Bearer POST to /api/runs/{id}/resume-now, the verb `uzi run resume-now` uses), unlike the
// direct-handler test beside it: a run parked at retained_continuation_unwitnessed is re-queued
// with the worker-recovery episode advanced, worker affinity kept, the cause cleared and every
// custody hold byte for byte untouched. A foreign owner's token gets a 404 and changes nothing.
// After the Resume the released-incarnation fence holds: the same worker registration cannot
// claim it, and the same worker after a re-registration (a fixture event here, not a claim that
// the earlier source use ended) can. The repeated refusal bound in the new episode is driven
// through the sweep in workersvc's TestUnwitnessedParkResumeClaimFenceLiveDB.
func TestUnwitnessedParkResumeRouteLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	ctx := context.Background()
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	t.Cleanup(func() {
		for _, q := range []string{
			"DELETE FROM recovery_captures WHERE user_id=ANY($1)",
			"DELETE FROM recovery_custody_holds WHERE user_id=ANY($1)",
			"DELETE FROM users WHERE id=ANY($1)",
		} {
			if _, err := pool.Exec(ctx, q, []uuid.UUID{owner, other}); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
	})
	ownerTok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	otherTok := cliMintToken(t, pool, other, clitoken.ScopeUser)
	repo := cliSeedOwnedRepo(t, pool, owner)
	worker := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,snapshot_register_nonce,snapshot_epoch)
 VALUES($1,$2,$3,$4,'online',now(),'parked',1)`, worker, owner, worker.String(), worker[:])
	run := uuid.New()
	cliMustExec(t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,
 recovery_wait_cause,claim_generation,worker_id,claim_released_at,released_worker_id,released_worker_nonce,
 requeue_count,requeue_episode_baseline,worker_recovery_episode,budget_wall_seconds,started_at,status_since)
 VALUES($1,$2,$3,'issue',3859,'park','d','recovery_wait','retained_continuation_unwitnessed',2,$4,now(),$4,'parked',
 2,0,0,86400,now()-interval '1 hour',now()-interval '2 minutes')`, run, owner, repo, worker)
	for gen := 1; gen <= 2; gen++ {
		cliMustExec(t, pool, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
 VALUES ($1,$2,$3,$4,$5,'open',$6,'parked incarnation',$6,$4)`, uuid.New(), owner, repo, run, gen, worker)
	}
	holds := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.generation)::text,'[]') FROM recovery_custody_holds h WHERE h.run_id=$1`, run).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	type snap struct {
		status, cause, retry          string
		worker                        uuid.UUID
		episode, requeues, generation int64
		released                      bool
	}
	read := func() snap {
		t.Helper()
		var s snap
		if err := pool.QueryRow(ctx, `SELECT status, coalesce(recovery_wait_cause,'<null>'), coalesce(recovery_retry_not_before::text,'<null>'),
 worker_id, worker_recovery_episode, requeue_count, claim_generation, claim_released_at IS NOT NULL FROM runs WHERE id=$1`, run).
			Scan(&s.status, &s.cause, &s.retry, &s.worker, &s.episode, &s.requeues, &s.generation, &s.released); err != nil {
			t.Fatal(err)
		}
		return s
	}
	path := "/api/runs/" + run.String() + "/resume-now"
	before, holdsBefore := read(), holds()

	if rec := bearerReq(router, http.MethodPost, path, otherTok); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign Resume = %d %s, want 404", rec.Code, rec.Body.String())
	}
	if after := read(); after != before || holds() != holdsBefore {
		t.Fatalf("foreign Resume changed the park: %+v", after)
	}

	rec := bearerReq(router, http.MethodPost, path, ownerTok)
	if rec.Code < 200 || rec.Code > 299 {
		t.Fatalf("owner Resume = %d %s, want 2xx", rec.Code, rec.Body.String())
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
	after := read()
	if after.status != "queued" || after.cause != "<null>" || after.retry != "<null>" {
		t.Fatalf("resumed run = %+v, want queued with the park cleared", after)
	}
	if after.episode != before.episode+1 {
		t.Fatalf("worker_recovery_episode = %d, want %d", after.episode, before.episode+1)
	}
	if after.worker != worker || after.requeues != before.requeues || after.generation != before.generation || !after.released {
		t.Fatalf("Resume changed worker affinity or the released-claim fence: before=%+v after=%+v", before, after)
	}
	if got := holds(); got != holdsBefore {
		t.Fatalf("Resume changed custody holds:\n%s\n%s", holdsBefore, got)
	}
	var relWorker uuid.UUID
	var relNonce string
	var baseline, count int64
	if err := pool.QueryRow(ctx, `SELECT released_worker_id, released_worker_nonce, requeue_episode_baseline, requeue_count FROM runs WHERE id=$1`, run).
		Scan(&relWorker, &relNonce, &baseline, &count); err != nil {
		t.Fatal(err)
	}
	if relWorker != worker || relNonce != "parked" || baseline != count {
		t.Fatalf("released pair = (%s, %q), baseline=%d count=%d; want (%s, \"parked\") and the episode re-baselined to the count",
			relWorker, relNonce, baseline, count, worker)
	}

	q := store.New(pool)
	claim := func() (store.Run, error) {
		return q.ClaimRun(ctx, store.ClaimRunParams{
			WorkerID: pgconv.UUID(worker), UserID: owner, CapabilityAware: true,
			WorkerCaps: []string{}, WorkerProtocolCaps: []string{}, DockerRepoAllowlist: []uuid.UUID{},
			CustodyHoldLimit: 10, SnapshotFreshCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
			RequestActiveIds: []uuid.UUID{}, RequestActiveGens: []int64{},
			WorkerIdentity: "parked incarnation", BackgroundGraceCutoff: pgconv.Time(time.Now()),
		})
	}
	resumed := read()
	if _, err := claim(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the released incarnation claimed the resumed run (err=%v), want it fenced", err)
	}
	if got := read(); got != resumed || holds() != holdsBefore {
		t.Fatalf("the fenced claim changed the run: %+v", got)
	}
	cliMustExec(t, pool, `UPDATE workers SET snapshot_register_nonce='re-registered', snapshot_epoch=2 WHERE id=$1`, worker)
	claimed, err := claim()
	if err != nil || claimed.ID != run || claimed.ClaimGeneration != resumed.generation+1 || claimed.ClaimReleasedAt.Valid {
		t.Fatalf("re-registered claim = (%s gen %d released=%v, %v), want %s at generation %d with the claim held",
			claimed.ID, claimed.ClaimGeneration, claimed.ClaimReleasedAt.Valid, err, run, resumed.generation+1)
	}
}
