package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// This behavioral probe uses only pre-scoped-claim Go interfaces and literal wire
// fields. Baseline registration may filter the unknown capability: seeding the
// registered row is an explicit harness prerequisite, not a negotiation claim.
// Copy this file alone into the baseline handler package for the official red.
func TestWorkerScopedClaimBaselineProbeLiveDB(t *testing.T) {
	for _, mode := range []string{"unknown no lease", "known owner lease plus ordinary"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newSkewFixture(ctx, t)
			q := store.New(f.pool)
			box := newHandlerTestBox(t)
			svc := workersvc.New(q, box, workersvc.Params{
				CustodyHoldLimit: defaultCustodyLimit(), ActiveSnapshotMaxEntries: 256,
				WorkerOutboxMaxPending: 2, TerminalPendingLease: time.Hour,
				RunTimeout: 24 * time.Hour, WorkerHeartbeatStale: time.Minute,
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
			// Eight slots keep the one/two source rows from saturating admission.
			const caps = `["pending_outcome_scoped_claim_v1"]`
			rec := f.post(t, "/api/worker/register", `{"version":"test","template":"base","max_concurrent_runs":8,"protocol_capabilities":`+caps+`}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("register prerequisite=%d %s", rec.Code, rec.Body.String())
			}
			var reply struct {
				Nonce string `json:"register_nonce"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Nonce == "" {
				t.Fatalf("register nonce prerequisite: %q err=%v", reply.Nonce, err)
			}
			var wk uuid.UUID
			if err := f.pool.QueryRow(ctx, "SELECT id FROM workers WHERE user_id=$1", f.owner).Scan(&wk); err != nil {
				t.Fatal(err)
			}
			var wireCaps []string
			if err := json.Unmarshal([]byte(caps), &wireCaps); err != nil {
				t.Fatal(err)
			}
			exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", wk, wireCaps)
			w, err := q.GetWorkerByID(ctx, wk)
			if err != nil || !slices.Equal(w.ProtocolCapabilities, wireCaps) {
				t.Fatalf("seeded registered capability prerequisite: %v err=%v", w.ProtocolCapabilities, err)
			}
			candidate, err := q.GetRunByID(ctx, f.runID)
			if err != nil || candidate.Status != "queued" || candidate.WorkerID.Valid {
				t.Fatalf("unassigned queued candidate prerequisite: %+v err=%v", candidate, err)
			}
			// newSkewFixture supplies an unprotected vault and sealed usable PAT/model
			// secret with this deterministic test box; successful assembly is asserted below.
			source := uuid.New()
			exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) SELECT $1,user_id,repo_id,'issue',7001,'source','d','running',$2,9 FROM runs WHERE id=$3", source, wk, candidate.ID)
			pending := fmt.Sprintf(`[{"run_id":%q,"claim_generation":7}]`, source.String())
			active := "[]"
			if mode == "known owner lease plus ordinary" {
				ordinary := uuid.New()
				exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) SELECT $1,user_id,repo_id,'issue',7002,'ordinary','d','running',$2,2 FROM runs WHERE id=$3", ordinary, wk, candidate.ID)
				pending = fmt.Sprintf(`[{"run_id":%q,"claim_generation":1},{"run_id":%q,"claim_generation":2},{"run_id":%q,"claim_generation":3},{"run_id":%q,"claim_generation":2}]`, source.String(), source.String(), source.String(), ordinary.String())
				active = fmt.Sprintf(`[{"run_id":%q,"claim_generation":9,"phase":"running","terminal_pending":true}]`, source.String())
			}
			body := fmt.Sprintf(`{"active_snapshot":{"snapshot_epoch":1,"register_nonce":%q,"active":%s,"pending_overflow":true,"pending_all":%s,"pending_policy_protection":[{"run_id":%q,"owner_generation":9}]}}`, reply.Nonce, active, pending, source.String())
			rec = f.post(t, "/api/worker/runs/claim", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("raw HTTP claim returned %d; want 200 for unrelated candidate %s (baseline overflow guard returns 204): %s", rec.Code, candidate.ID, rec.Body.String())
			}
			var claimed struct {
				RunID      string `json:"run_id"`
				Generation int64  `json:"claim_generation"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil || claimed.RunID != candidate.ID.String() || claimed.Generation != candidate.ClaimGeneration+1 {
				t.Fatalf("claimed=%+v want run=%s generation=%d err=%v", claimed, candidate.ID, candidate.ClaimGeneration+1, err)
			}
			w, err = q.GetWorkerByID(ctx, wk)
			if err != nil || !w.PendingOverflowUntil.Valid || !w.PendingOverflowUntil.Time.After(time.Now()) {
				t.Fatalf("persisted owner overflow closure missing: %v", err)
			}
			if mode == "unknown no lease" {
				var leases int
				if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", wk, source).Scan(&leases); err != nil || leases != 0 {
					t.Fatalf("unknown source acquired invented owner lease: count=%d err=%v", leases, err)
				}
			}
			// The run now becomes a candidate, but its owner's persisted closure
			// must exclude it for both claimants under the unchanged claim SQL.
			exec("UPDATE runs SET status='queued',updated_at=now()-interval '3 hours' WHERE id=$1", source)
			if rec := f.post(t, "/api/worker/runs/claim", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("owner reclaimed protected source: %d %s", rec.Code, rec.Body.String())
			}
			token, hash, err := jointoken.Generate()
			if err != nil {
				t.Fatal(err)
			}
			exec("INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,max_concurrent_runs) VALUES($1,$2,'sibling',$3,'online',now(),8)", uuid.New(), f.owner, hash)
			f.token = token
			if rec := f.post(t, "/api/worker/runs/claim", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("sibling reclaimed protected source: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
