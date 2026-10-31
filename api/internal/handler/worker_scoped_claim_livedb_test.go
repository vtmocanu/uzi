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

// Literal wire payloads through Bearer authentication, routing, snapshot decoding,
// the real Claim transaction, credential assembly and exact-claim finish.
func TestWorkerScopedClaimWireLiveDB(t *testing.T) {
	for _, mode := range []string{"historical scoped", "known owner lease plus ordinary", "ephemeral scoped", "malformed independent", "reregister shrink", "ordinary bodyless", "ephemeral bodyless"} {
		t.Run(mode, func(t *testing.T) {
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
			register := func(caps string) string {
				t.Helper()
				rec := f.post(t, "/api/worker/register", `{"version":"test","template":"base","max_concurrent_runs":8,"protocol_capabilities":`+caps+`}`)
				if rec.Code != http.StatusOK {
					t.Fatalf("register=%d %s", rec.Code, rec.Body.String())
				}
				var reply struct {
					Nonce    string   `json:"register_nonce"`
					Features []string `json:"protocol_features"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(reply.Features, "pending_outcome_scoped_claim_v1") || slices.Contains(reply.Features, "provider_policy_hold_v1") {
					t.Fatalf("feature readiness=%v", reply.Features)
				}
				return reply.Nonce
			}
			nonce := register(`["pending_outcome_scoped_claim_v1"]`)
			var wk uuid.UUID
			if err := f.pool.QueryRow(ctx, "SELECT id FROM workers WHERE user_id=$1", f.owner).Scan(&wk); err != nil {
				t.Fatal(err)
			}
			w, err := q.GetWorkerByID(ctx, wk)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(w.ProtocolCapabilities, "pending_outcome_scoped_claim_v1") {
				t.Fatal("registered capability missing")
			}
			candidate, err := q.GetRunByID(ctx, f.runID)
			if err != nil || candidate.Status != "queued" || candidate.WorkerID.Valid {
				t.Fatalf("candidate prerequisite: %+v err=%v", candidate, err)
			}
			source := uuid.New()
			exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) SELECT $1,user_id,repo_id,'issue',7001,'source','d','running',$2,9 FROM runs WHERE id=$3", source, wk, f.runID)
			if mode == "reregister shrink" {
				nonce = register(`[]`)
				exec("UPDATE runs SET status='running' WHERE id=$1", source)
			}
			if mode == "ephemeral scoped" || mode == "ephemeral bodyless" {
				exec("UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1", wk, f.runID)
			}
			group := fmt.Sprintf(`[{"run_id":%q,"owner_generation":9}]`, source.String())
			gen := 7
			if mode == "malformed independent" {
				group = `[{"owner_generation":"bad"}]`
				gen = 9
			}
			body := fmt.Sprintf(`{"active_snapshot":{"snapshot_epoch":1,"register_nonce":%q,"active":[],"pending_overflow":true,"pending_all":[{"run_id":%q,"claim_generation":%d}],"pending_policy_protection":%s}}`, nonce, source.String(), gen, group)
			if mode == "ordinary bodyless" || mode == "ephemeral bodyless" {
				exec("UPDATE workers SET pending_overflow=true,pending_overflow_until=now()+interval '1 hour' WHERE id=$1", wk)
				body = ""
			}
			if mode == "known owner lease plus ordinary" {
				ordinary := uuid.New()
				exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation) SELECT $1,user_id,repo_id,'issue',7002,'ordinary','d','running',$2,2 FROM runs WHERE id=$3", ordinary, wk, candidate.ID)
				body = fmt.Sprintf(`{"active_snapshot":{"snapshot_epoch":1,"register_nonce":%q,"active":[{"run_id":%q,"claim_generation":9,"phase":"running","terminal_pending":true}],"pending_overflow":true,"pending_all":[{"run_id":%q,"claim_generation":1},{"run_id":%q,"claim_generation":2},{"run_id":%q,"claim_generation":3},{"run_id":%q,"claim_generation":2}],"pending_policy_protection":%s}}`, nonce, source.String(), source.String(), source.String(), source.String(), ordinary.String(), group)
			}
			rec := f.post(t, "/api/worker/runs/claim", body)
			if mode == "malformed independent" || mode == "reregister shrink" {
				if rec.Code != http.StatusNoContent {
					t.Fatalf("invalid exception=%d %s", rec.Code, rec.Body.String())
				}
				if mode == "malformed independent" {
					var count int
					if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM worker_pending_outcome_identities WHERE worker_id=$1 AND run_id=$2", wk, source).Scan(&count); err != nil || count != 1 {
						t.Fatalf("valid pending coverage count=%d err=%v", count, err)
					}
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("unrelated claim=%d %s", rec.Code, rec.Body.String())
			}
			var claimed struct {
				RunID      string `json:"run_id"`
				Generation int64  `json:"claim_generation"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil || claimed.RunID != candidate.ID.String() || claimed.Generation != candidate.ClaimGeneration+1 {
				t.Fatalf("claimed=%s err=%v", claimed.RunID, err)
			}
			w, err = q.GetWorkerByID(ctx, wk)
			if err != nil || !w.PendingOverflowUntil.Valid || !w.PendingOverflowUntil.Time.After(time.Now()) {
				t.Fatalf("global closure missing: %v", err)
			}
			// Turn the owned protected source into a candidate. Its owner's closure
			// must still exclude it both for this claimant and for another claimant.
			exec("UPDATE runs SET status='queued',updated_at=now()-interval '3 hours' WHERE id=$1", source)
			if rec := f.post(t, "/api/worker/runs/claim", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("owner reclaimed protected candidate: %d", rec.Code)
			}
			token, hash, err := jointoken.Generate()
			if err != nil {
				t.Fatal(err)
			}
			exec("INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at) VALUES($1,$2,'sibling',$3,'online',now())", uuid.New(), f.owner, hash)
			sibling := f
			sibling.token = token
			if rec := sibling.post(t, "/api/worker/runs/claim", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("sibling reclaimed protected candidate: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
