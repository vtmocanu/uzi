package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The agent byte-checks this same file against its real projection and serializer.
// Here the unchanged bytes cross Bearer auth, WorkerRoutes and the real Claim TX.
func TestWorkerScopedClaimSharedFixtureLiveDB(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/worker-register-snapshot/scoped-claim.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Snapshot workersvc.ActiveSnapshot `json:"active_snapshot"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	snap := wire.Snapshot
	if len(snap.PendingPolicyProtection) != 1 || len(snap.Active) != 1 || len(snap.PendingAll) != 4 || !snap.PendingOverflow {
		t.Fatalf("fixture prerequisites: %+v", snap)
	}
	ctx := context.Background()
	f := newSkewFixture(ctx, t)
	q := store.New(f.pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), ActiveSnapshotMaxEntries: 256, WorkerOutboxMaxPending: 2,
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
	const capability = "pending_outcome_scoped_claim_v1"
	rec := f.post(t, "/api/worker/register", `{"version":"test","template":"base","max_concurrent_runs":8,"protocol_capabilities":["pending_outcome_scoped_claim_v1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register=%d %s", rec.Code, rec.Body.String())
	}
	var registration struct {
		WorkerID uuid.UUID `json:"worker_id"`
		Nonce    string    `json:"register_nonce"`
		Features []string  `json:"protocol_features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &registration); err != nil {
		t.Fatal(err)
	}
	if registration.Nonce == "" || !slices.Contains(registration.Features, capability) ||
		slices.Contains(registration.Features, "provider_policy_hold_v1") {
		t.Fatalf("independent negotiation: %+v", registration)
	}
	worker, err := q.GetWorkerByID(ctx, registration.WorkerID)
	if err != nil || !slices.Contains(worker.ProtocolCapabilities, capability) {
		t.Fatalf("registered capability missing: %v", err)
	}
	// Test-only deterministic incarnation seed AFTER genuine registration. The
	// production nonce/epoch validation and ownership checks are unchanged.
	exec("UPDATE workers SET snapshot_register_nonce=$2,snapshot_epoch=0 WHERE id=$1", worker.ID, snap.RegisterNonce)
	source := snap.PendingPolicyProtection[0]
	ordinary := snap.Active[0]
	sourceID, err := uuid.Parse(source.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryID, err := uuid.Parse(ordinary.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for i, seed := range []struct {
		id         uuid.UUID
		generation int64
	}{{sourceID, source.OwnerGeneration}, {ordinaryID, ordinary.ClaimGeneration}} {
		exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
			SELECT $1,user_id,repo_id,'issue',$2,'shared wire source','d','running',$3,$4 FROM runs WHERE id=$5`,
			seed.id, 7100+i, worker.ID, seed.generation, f.runID)
		id := seed.id
		t.Cleanup(func() {
			// Fixed IDs are released for a repeat invocation in the same database.
			if _, err := f.pool.Exec(ctx, "DELETE FROM runs WHERE id=$1", id); err != nil {
				t.Errorf("delete fixture run: %v", err)
			}
		})
	}
	candidate, err := q.GetRunByID(ctx, f.runID)
	if err != nil || candidate.Status != "queued" || candidate.WorkerID.Valid {
		t.Fatalf("unrelated candidate prerequisite: %+v err=%v", candidate, err)
	}
	rec = f.post(t, "/api/worker/runs/claim", string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("shared fixture claim=%d %s", rec.Code, rec.Body.String())
	}
	var claimed struct {
		RunID      uuid.UUID `json:"run_id"`
		Generation int64     `json:"claim_generation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil ||
		claimed.RunID != candidate.ID || claimed.Generation != candidate.ClaimGeneration+1 {
		t.Fatalf("unrelated claim=%+v err=%v", claimed, err)
	}
	worker, err = q.GetWorkerByID(ctx, worker.ID)
	if err != nil || !worker.PendingOverflowUntil.Valid || !worker.PendingOverflowUntil.Time.After(time.Now()) ||
		worker.SnapshotEpoch != snap.SnapshotEpoch {
		t.Fatalf("conservative closure/epoch missing: %+v err=%v", worker, err)
	}
	for _, identity := range snap.PendingAll {
		var retained bool
		// Historical grouped generations are accounted by the validated current
		// owner plus conservative overflow. They must not acquire invented leases
		// or identity rows; only the current ordinary source passes the SQL upsert.
		wantRetained := identity.RunID == ordinary.RunID && identity.ClaimGeneration == ordinary.ClaimGeneration
		if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_pending_outcome_identities
			WHERE worker_id=$1 AND run_id=$2 AND claim_generation=$3 AND reported_until>now())`,
			worker.ID, identity.RunID, identity.ClaimGeneration).Scan(&retained); err != nil || retained != wantRetained {
			t.Fatalf("source identity row=%v want=%v: %+v err=%v", retained, wantRetained, identity, err)
		}
	}
	var sourceLeases int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", worker.ID, sourceID).Scan(&sourceLeases); err != nil || sourceLeases != 0 {
		t.Fatalf("unknown historical source acquired a lease: count=%d err=%v", sourceLeases, err)
	}
	owned, err := q.GetRunByID(ctx, sourceID)
	if err != nil || owned.Status != "running" || owned.ClaimGeneration != source.OwnerGeneration ||
		!owned.WorkerID.Valid || owned.WorkerID.Bytes != worker.ID {
		t.Fatalf("historical evidence changed current ownership: %+v err=%v", owned, err)
	}
	// Make the protected source the only queued candidate. Another authenticated
	// worker must also be excluded, independently of this claimant's pause.
	exec("UPDATE runs SET status='queued',updated_at=now()-interval '3 hours' WHERE id=$1", source.RunID)
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at) VALUES($1,$2,'shared-wire-sibling',$3,'online',now())",
		uuid.New(), f.owner, hash)
	sibling := f
	sibling.token = token
	assertExcluded := func() {
		t.Helper()
		for _, claimant := range []skewFixture{f, sibling} {
			if rec := claimant.post(t, "/api/worker/runs/claim", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("protected source claim=%d %s", rec.Code, rec.Body.String())
			}
		}
	}
	assertExcluded()

	// Lose the scoped capability through real registration. A boot snapshot keeps
	// conservative protection; register evidence never grants a claim exception.
	var envelope struct {
		Snapshot json.RawMessage `json:"active_snapshot"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	rec = f.post(t, "/api/worker/register", `{"version":"test","template":"base","max_concurrent_runs":8,"protocol_capabilities":[],"active_snapshot":`+string(envelope.Snapshot)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("feature loss register=%d %s", rec.Code, rec.Body.String())
	}
	worker, err = q.GetWorkerByID(ctx, worker.ID)
	if err != nil || slices.Contains(worker.ProtocolCapabilities, capability) {
		t.Fatalf("capability did not shrink: %v", err)
	}
	assertExcluded()
}
