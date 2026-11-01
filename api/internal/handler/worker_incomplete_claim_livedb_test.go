package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Agent tests byte-check these exact bytes against a real Outbox with a retained
// journal hidden behind a refused UUID symlink. HTTP here proves actual intake,
// rather than merely the stubbed request emission the old D1 control observed.
func TestWorkerIncompleteInventoryClaimLiveDB(t *testing.T) {
	exerciseIncompleteInventoryClaim(t, false)
}

// Manual protocol-skew probe only. It is intentionally outside the LiveDB$ gate:
// the normal regression above can never be switched to accepting legacy idle.
func TestWorkerIncompleteInventoryOlderAPISkew(t *testing.T) {
	if os.Getenv("UZI_INCOMPLETE_EXPECT_LEGACY") != "1" {
		t.Skip("manual older-API probe requires UZI_INCOMPLETE_EXPECT_LEGACY=1, an isolated LiveDB DSN and pre-marker API source")
	}
	exerciseIncompleteInventoryClaim(t, true)
}

func exerciseIncompleteInventoryClaim(t *testing.T, expectLegacy bool) {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/worker-register-snapshot/incomplete-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f := newSkewFixture(ctx, t)
	q := store.New(f.pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), ActiveSnapshotMaxEntries: 256, WorkerOutboxMaxPending: 4,
		TerminalPendingLease: time.Hour, RunTimeout: 24 * time.Hour, WorkerHeartbeatStale: time.Minute,
	})
	svc.SetVault(vault.NewPostgres(box, f.pool))
	svc.SetTxBeginner(f.pool)
	h := &Handler{pool: f.pool, q: q, box: box, wsvc: svc, now: time.Now, startedAt: time.Now()}
	f.srv = h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	rec := f.post(t, "/api/worker/register", `{"version":"test","template":"base","max_concurrent_runs":8,"protocol_capabilities":["pending_outcome_scoped_claim_v1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register=%d %s", rec.Code, rec.Body.String())
	}
	var registration struct {
		WorkerID uuid.UUID `json:"worker_id"`
		Nonce    string    `json:"register_nonce"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE workers SET snapshot_register_nonce='incomplete-inventory-fixture',snapshot_epoch=0 WHERE id=$1", registration.WorkerID)
	source := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
 SELECT $1,user_id,repo_id,'issue',7722,'hidden journal source','d','queued',$2,9 FROM runs WHERE id=$3`, source, registration.WorkerID, f.runID)
	t.Cleanup(func() {
		// A deliberately broken owner-exclusion mutation may create a hold for
		// this test source. Release only this fixture user's rows before deleting it.
		if _, err := f.pool.Exec(ctx, "UPDATE recovery_custody_holds SET state='released',live_run_id=NULL,live_worker_id=NULL,released_at=now() WHERE run_id=$1 AND user_id=$2", source, f.owner); err != nil {
			t.Error(err)
		}
		if _, err := f.pool.Exec(ctx, "DELETE FROM runs WHERE id=$1", source); err != nil {
			t.Error(err)
		}
	})
	rec = f.post(t, "/api/worker/runs/claim", string(raw))
	if expectLegacy {
		// Only for the watched baseline/skew control. The earlier API ignores the
		// marker without rejecting unknown nested fields and must remain idle.
		if rec.Code != http.StatusNoContent {
			t.Fatalf("old API must remain idle: %d %s", rec.Code, rec.Body.String())
		}
		return
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated incomplete-inventory claim=%d %s", rec.Code, rec.Body.String())
	}
	var claimed struct {
		RunID      uuid.UUID `json:"run_id"`
		Generation int64     `json:"claim_generation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil || claimed.RunID != f.runID || claimed.Generation != 1 {
		t.Fatalf("unrelated claim=%+v err=%v", claimed, err)
	}
	worker, err := q.GetWorkerByID(ctx, registration.WorkerID)
	if err != nil || !worker.PendingOverflowUntil.Valid || !worker.PendingOverflowUntil.Time.After(time.Now()) {
		t.Fatalf("overflow closure lost: %+v %v", worker, err)
	}
	var holds int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open'", f.runID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 1 {
		t.Fatalf("normal claim must open exactly one custody hold, got %d", holds)
	}
	// Once unrelated work is terminal, the protected owner's queued source is the
	// only candidate. Neither a fresh marked owner claim nor a peer may take it.
	exec("UPDATE runs SET status='completed' WHERE id=$1", f.runID)
	exec("UPDATE runs SET updated_at=now()-interval '3 hours' WHERE id=$1", source)
	ownerBody := `{"active_snapshot":{"snapshot_epoch":2,"register_nonce":"incomplete-inventory-fixture","active":[],"pending_overflow":true,"pending_inventory_incomplete":true}}`
	if rec := f.post(t, "/api/worker/runs/claim", ownerBody); rec.Code != http.StatusNoContent {
		t.Fatalf("owner took protected source: %d %s", rec.Code, rec.Body.String())
	}
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at) VALUES($1,$2,'incomplete-peer',$3,'online',now())", uuid.New(), f.owner, hash)
	peer := f
	peer.token = token
	if rec := peer.post(t, "/api/worker/runs/claim", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("peer took protected source: %d %s", rec.Code, rec.Body.String())
	}
	retained, err := q.GetRunByID(ctx, source)
	if err != nil || retained.Status != "queued" || retained.ClaimGeneration != 9 || retained.WorkerID.Bytes != registration.WorkerID {
		t.Fatalf("source changed: %+v %v", retained, err)
	}
	var sourceHolds int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", source).Scan(&sourceHolds); err != nil || sourceHolds != 0 {
		t.Fatalf("protected source got custody writes: %d %v", sourceHolds, err)
	}
	// Feature loss cannot reuse the marker. Keep the original request bytes but
	// use the fresh nonce and epoch from genuine registration without capability.
	rec = f.post(t, "/api/worker/register", `{"version":"test","template":"base","max_concurrent_runs":8,"protocol_capabilities":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("downgrade register=%d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	candidate := uuid.New()
	exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) SELECT $1,user_id,repo_id,'issue',7723,'unassigned downgrade control','d','queued' FROM runs WHERE id=$2`, candidate, f.runID)
	body := fmt.Sprintf(`{"active_snapshot":{"snapshot_epoch":1,"register_nonce":%q,"active":[],"pending_overflow":true,"pending_inventory_incomplete":true}}`, registration.Nonce)
	if rec := f.post(t, "/api/worker/runs/claim", body); rec.Code != http.StatusNoContent {
		t.Fatalf("downgrade bypassed overflow: %d %s", rec.Code, rec.Body.String())
	}
}
