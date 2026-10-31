package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const settlementTestNonce = "settle-nonce"

// settlementPark parks a run the way the park transaction leaves it (recovery_wait, released pair,
// generation 2, episode 0) with its memory_park_releases row, for a worker that negotiated the receipt.
func settlementPark(t *testing.T, fx *ephemeralFixture) (store.Worker, uuid.UUID) {
	t.Helper()
	wid := fx.onlineWorker("memory settlement", false)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE workers SET snapshot_register_nonce=$2,protocol_capabilities=$3 WHERE id=$1",
		wid, settlementTestNonce, []string{capability.WorkerMemoryPressureV1, capability.MemoryParkSettlementV1}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',recovery_wait_cause='worker_memory_pressure',
        claim_generation=2,worker_id=$2,claim_released_at=now(),released_worker_id=$2,released_worker_nonce=$3,
        memory_episode=0,memory_policy='{"version":1,"max_interventions":3}',
        recovery_retry_not_before=now()-interval '1 minute' WHERE id=$1`, id, wid, settlementTestNonce); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO memory_park_releases
        (run_id,claim_generation,memory_episode,worker_id,register_nonce,released_at)
        SELECT id,claim_generation,memory_episode,released_worker_id,released_worker_nonce,claim_released_at
        FROM runs WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	worker, err := fx.q.GetWorkerByID(fx.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	return worker, id
}

// settlementReceipt is the shared typical fixture rebound to this park; mutate edits the decoded map.
func settlementReceipt(t *testing.T, worker store.Worker, runID uuid.UUID, episode int64, mutate func(map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "memory-park", "settlement-typical.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, b := range []map[string]any{m["binding"].(map[string]any), m["settlement"].(map[string]any)["evidence"].(map[string]any)["parkAck"].(map[string]any)} {
		b["runId"], b["workerId"], b["registerNonce"] = runID.String(), worker.ID.String(), settlementTestNonce
		b["claimGeneration"], b["memoryEpisode"] = 2, episode
	}
	m["settlement"].(map[string]any)["evidence"].(map[string]any)["interventionIds"] = []any{}
	if mutate != nil {
		mutate(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func settlementHandler(fx *ephemeralFixture) *Handler {
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	svc.SetTxBeginner(fx.pool)
	return &Handler{q: fx.q, pool: fx.pool, wsvc: svc}
}

// WorkerMemoryParkSettlement maps each service outcome to its status and reason through the real
// handler, service and SQL.
func TestWorkerMemoryParkSettlementStatusMappingLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	worker, id := settlementPark(t, fx)
	h := settlementHandler(fx)
	post := func(runID uuid.UUID, body []byte) (int, map[string]any) {
		t.Helper()
		rec := settlementHTTP(h, &worker, runID.String(), body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// ErrMemoryStale: a receipt for an episode the park is not in.
	if code, out := post(id, settlementReceipt(t, worker, id, 1, nil)); code != http.StatusConflict || out["reason"] != "stale_claim" {
		t.Fatalf("stale = %d %v", code, out)
	}
	// ErrRunNotOwned: a run another owner holds (or that does not exist).
	foreign := uuid.New()
	if code, _ := post(foreign, settlementReceipt(t, worker, foreign, 0, nil)); code != http.StatusNotFound {
		t.Fatalf("foreign run = %d, want 404", code)
	}
	// Success: 200 with the receipt's sha256.
	first := settlementReceipt(t, worker, id, 0, nil)
	sum := sha256.Sum256(first)
	code, out := post(id, first)
	if code != http.StatusOK || out["settlement_sha256"] != hex.EncodeToString(sum[:]) || len(out) != 1 {
		t.Fatalf("success = %d %v", code, out)
	}
	// The exact bytes replay as success.
	if code, out := post(id, first); code != http.StatusOK || out["settlement_sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("replay = %d %v", code, out)
	}
	// ErrMemoryBinding: different valid bytes never replace the stored receipt.
	other := settlementReceipt(t, worker, id, 0, func(m map[string]any) {
		m["origin"].(map[string]any)["executorIncarnation"] = "executor-2:def"
	})
	if code, out := post(id, other); code != http.StatusConflict || out["reason"] != "memory_binding_conflict" {
		t.Fatalf("different receipt = %d %v", code, out)
	}
}

// The owner Resume route lifts the released-pair exclusion in the same transaction when, and only
// when, the park's settlement receipt is stored.
func TestResumeRouteLiftsSettledMemoryParkLiveDB(t *testing.T) {
	for _, settled := range []bool{true, false} {
		name := map[bool]string{true: "settled receipt", false: "no receipt"}[settled]
		t.Run(name, func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			t.Cleanup(func() {
				if _, err := fx.pool.Exec(fx.ctx, "DELETE FROM users WHERE id=$1", fx.userID); err != nil {
					t.Errorf("cleanup resume lift owner: %v", err)
				}
			})
			worker, id := settlementPark(t, fx)
			h := settlementHandler(fx)
			if settled {
				if rec := settlementHTTP(h, &worker, id.String(), settlementReceipt(t, worker, id, 0, nil)); rec.Code != http.StatusOK {
					t.Fatalf("settle = %d %s", rec.Code, rec.Body.String())
				}
			}
			if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusOK {
				t.Fatalf("owner Resume = %d %s", rec.Code, rec.Body.String())
			}
			run, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil || run.Status != "queued" || run.MemoryEpisode != 1 {
				t.Fatalf("resumed run: %+v err=%v", run, err)
			}
			var liftedAt *string
			if err := fx.pool.QueryRow(fx.ctx, "SELECT lifted_at::text FROM memory_park_releases WHERE run_id=$1 AND claim_generation=2", id).Scan(&liftedAt); err != nil {
				t.Fatal(err)
			}
			if settled {
				if run.ReleasedWorkerID.Valid || run.ReleasedWorkerNonce.Valid || liftedAt == nil {
					t.Fatalf("settled park not lifted: released=%v/%v lifted_at=%v", run.ReleasedWorkerID.Valid, run.ReleasedWorkerNonce.Valid, liftedAt)
				}
			} else if !run.ReleasedWorkerID.Valid || !run.ReleasedWorkerNonce.Valid || liftedAt != nil {
				t.Fatalf("unsettled park lifted: released=%v/%v lifted_at=%v", run.ReleasedWorkerID.Valid, run.ReleasedWorkerNonce.Valid, liftedAt)
			}
		})
	}
}
