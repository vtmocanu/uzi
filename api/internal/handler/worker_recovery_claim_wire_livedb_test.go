package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Exercise the owner-authenticated Resume endpoint and the full Service claim,
// including its serialized wire, rather than assembling a DTO from a reread.
func TestWorkerRecoveryOwnerResumeServiceClaimWireLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	pat, err := fx.box.Seal([]byte("fixture-forge-pat"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := fx.box.Seal([]byte("fixture-model-token"))
	if err != nil {
		t.Fatal(err)
	}
	exec("UPDATE forge_connections SET token_ciphertext=$2 WHERE user_id=$1", fx.userID, pat)
	exec("INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'anthropic_token','recovery-wire',true,$3,'master')", uuid.New(), fx.userID, token)
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{
		RunTimeout: 24 * time.Hour, RunWallCeiling: 24 * time.Hour, WorkerHeartbeatStale: time.Minute,
		CustodyHoldLimit: defaultCustodyLimit(),
	})
	svc.SetTxBeginner(fx.pool)
	oldWorker := fx.onlineWorker("recovery-wire-old", false)
	newWorker := fx.onlineWorker("recovery-wire-new", false)
	memoryResumeWorker := fx.onlineWorker("recovery-wire-memory-resume", false)
	exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=ANY($1::uuid[])", []uuid.UUID{oldWorker, newWorker, memoryResumeWorker}, []string{capability.RecoveryArchiveV1})
	id := fx.queuedRun([]string{})
	claim := func(workerID uuid.UUID, want int64) {
		t.Helper()
		w, err := fx.q.GetWorkerByID(fx.ctx, workerID)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := svc.Claim(fx.ctx, w, nil)
		if err != nil || payload == nil || payload.RunID != id.String() {
			t.Fatalf("Service claim run=%s payload present=%v err=%v", id, payload != nil, err)
		}
		wire, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(wire, &fields); err != nil {
			t.Fatal(err)
		}
		raw, present := fields["worker_recovery_episode"]
		var got int64
		if !present {
			t.Fatal("Service claim JSON lacks worker_recovery_episode")
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Service claim episode=%d want=%d", got, want)
		}
	}
	requeue := func(workerID uuid.UUID) {
		t.Helper()
		exec("UPDATE runs SET status='running' WHERE id=$1", id)
		rows, err := fx.q.RequeueWorkerRuns(fx.ctx, store.RequeueWorkerRunsParams{
			WorkerID: pgtype.UUID{Bytes: workerID, Valid: true}, MaxRequeues: 3})
		if err != nil || len(rows) != 1 || rows[0] != id {
			t.Fatalf("requeue rows=%v err=%v", rows, err)
		}
	}
	claim(oldWorker, 0)
	requeue(oldWorker)
	claim(oldWorker, 0)
	exec("UPDATE runs SET status='running',requeue_count=3,checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", id, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	rows, err := fx.q.FailWorkerRunsOverCap(fx.ctx, store.FailWorkerRunsOverCapParams{
		WorkerID: pgtype.UUID{Bytes: oldWorker, Valid: true}, MaxRequeues: 3})
	if err != nil || len(rows) != 1 {
		t.Fatalf("exhaustion rows=%v err=%v", rows, err)
	}
	h := &Handler{q: fx.q}
	h.cfg.RunTimeout = 24 * time.Hour
	h.cfg.RunMaxRequeues = 3
	if rec := recoveryResumeRequest(h, id, uuid.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign Resume=%d", rec.Code)
	}
	before, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.WorkerRecoveryEpisode != 0 {
		t.Fatalf("automatic loss or foreign Resume changed episode=%d", before.WorkerRecoveryEpisode)
	}
	if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusOK {
		t.Fatalf("owner Resume=%d %s", rec.Code, rec.Body.String())
	}
	after, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil || after.WorkerRecoveryEpisode != before.WorkerRecoveryEpisode+1 {
		t.Fatalf("owner Resume episode=%d err=%v", after.WorkerRecoveryEpisode, err)
	}
	claim(newWorker, 1)
	requeue(newWorker)
	claim(newWorker, 1)

	// A memory-pressure release opens only a memory episode.
	beforeMemory, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	const memoryNonce = "recovery-wire-memory-nonce"
	exec("UPDATE workers SET protocol_capabilities=$2,snapshot_register_nonce=$3 WHERE id=$1", newWorker,
		[]string{capability.RecoveryArchiveV1, capability.WorkerMemoryPressureV1}, memoryNonce)
	memoryWorker, err := fx.q.GetWorkerByID(fx.ctx, newWorker)
	if err != nil {
		t.Fatal(err)
	}
	cause := "worker_memory_pressure"
	parked, applied, err := svc.SetState(fx.ctx, memoryWorker, id, workersvc.StateRequest{
		State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &beforeMemory.ClaimGeneration,
		RegisterNonce: memoryNonce, MemoryEpisode: &beforeMemory.MemoryEpisode})
	if err != nil || !applied {
		t.Fatalf("memory park applied=%v err=%v", applied, err)
	}
	if parked.Status != "recovery_wait" || parked.RecoveryWaitCause.String != cause ||
		!parked.ClaimReleasedAt.Valid || parked.ReleasedWorkerID != (pgtype.UUID{Bytes: newWorker, Valid: true}) ||
		!parked.ReleasedWorkerNonce.Valid || parked.ReleasedWorkerNonce.String != memoryNonce {
		t.Fatalf("memory park release fences: %+v", parked)
	}
	if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusOK {
		t.Fatalf("memory Resume=%d %s", rec.Code, rec.Body.String())
	}
	afterMemory, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil || afterMemory.MemoryEpisode != beforeMemory.MemoryEpisode+1 ||
		afterMemory.WorkerRecoveryEpisode != beforeMemory.WorkerRecoveryEpisode ||
		afterMemory.RequeueCount != beforeMemory.RequeueCount ||
		afterMemory.RequeueEpisodeBaseline != beforeMemory.RequeueEpisodeBaseline {
		t.Fatalf("memory Resume changed independent recovery counters: before=%+v after=%+v err=%v", beforeMemory, afterMemory, err)
	}
	claim(memoryResumeWorker, 1)
}
