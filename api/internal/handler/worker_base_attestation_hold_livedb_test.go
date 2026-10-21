package handler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerBaseAttestationHoldReconciledStaleClaimLiveDB pins the HTTP answer of
// POST /api/worker/runs/{id}/base-attestation-hold (issue #3194) on a run that carries a
// milestone reconciliation audit. For such a run beginClaimMutation's generation fence
// (validateReconciledGeneration) refuses a stale generation or a released claim with
// workersvc.ErrStaleClaim before the setter runs; the handler must answer that as a 409 with
// reason stale_claim (it used to fall through to a 500) and change nothing on the run. The
// current-generation live claim is the positive control: 200, paused, base_attestation_missing.
func TestWorkerBaseAttestationHoldReconciledStaleClaimLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	// The reconciliation fence lives only on the transactional path of beginClaimMutation.
	svc.SetTxBeginner(pool)
	svc.SetBackground(func(func()) {})

	exec := func(sql string, args ...any) {
		t.Helper()
		cliMustExec(t, pool, sql, args...)
	}
	owner, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`, owner, fmt.Sprintf("base-hold-http-%s@e2e", owner))
	var runIDs []uuid.UUID
	t.Cleanup(func() {
		// Delete only this fixture's rows, in FK order (audits and inputs cascade with the run).
		for _, id := range runIDs {
			exec("DELETE FROM runs WHERE id=$1", id)
		}
		exec("DELETE FROM workers WHERE id=$1", workerID)
		exec("DELETE FROM repos WHERE id=$1", repoID)
		exec("DELETE FROM forge_connections WHERE id=$1", connID)
		exec("DELETE FROM users WHERE id=$1", owner)
	})
	pat, err := box.Seal([]byte("toy-forge-credential"))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://forge.e2e','bot',1,$3)`, connID, owner, pat)
	exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES($1,$2,1,$3,'https://forge.e2e/g/r','main',true)`, repoID, connID, "g/base-hold-http-"+repoID.String())
	workerToken, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'online',now())`, workerID, owner, "base-hold-http-"+workerID.String(), hash)

	const generation = 3
	// seedReconciled seeds a running --require-base run held by this worker at claim
	// generation 3, carrying one milestone reconciliation audit so the generation fence applies.
	seedReconciled := func(t *testing.T) uuid.UUID {
		t.Helper()
		id := uuid.New()
		runIDs = append(runIDs, id)
		iid := int64(binary.BigEndian.Uint64(id[:8]) >> 1)
		exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,
 claim_generation,require_base_match,planned_base_commit)
 VALUES($1,$2,$3,'issue',$4,'test','test','running',$5,$6,true,'abcdef1234567')`,
			id, owner, repoID, iid, workerID, generation)
		var input int64
		if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'approve_plan','{}') RETURNING id`, id).Scan(&input); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO run_milestone_reconciliations
 (id,run_id,owner_id,gate_revision,presentation_id,gate_digest,reset_state_digest,worker_id,claim_generation,
 reason,consent,before_state,after_state,applied_input_id,stop_cutoff)
 VALUES(gen_random_uuid(),$1,$2,1,gen_random_uuid(),'digest'::bytea,'reset',$3,$4,
 'reset','{"mode":"reset"}'::jsonb,'{}'::jsonb,'{}'::jsonb,$5,0)`, id, owner, workerID, generation, input)
		return id
	}

	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	workerRouter := h.WorkerRoutes(mw.NewLimiter(100000, time.Minute, nil))
	const head = "0123456789abcdef0123456789abcdef01234567"
	post := func(t *testing.T, id uuid.UUID, gen int64, want int) map[string]any {
		t.Helper()
		body := fmt.Sprintf(`{"head":%q,"claim_generation":%d}`, head, gen)
		rec := bearerReqBody(workerRouter, http.MethodPost, "/api/worker/runs/"+id.String()+"/base-attestation-hold", workerToken, body)
		if rec.Code != want {
			t.Fatalf("base-attestation-hold gen=%d: status %d want %d: %s", gen, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return out
	}

	// observed is every run column the hold setter or a misrouted refusal could move.
	type observed struct {
		Status                 string
		HoldReason             pgtype.Text
		HoldCapturedHead       pgtype.Text
		StatusSince            pgtype.Timestamptz
		BudgetPausedSeconds    int32
		BudgetExtensionSeconds int32
		Health                 string
		HealthReason           pgtype.Text
		HealthSince            pgtype.Timestamptz
		WorkerID               pgtype.UUID
		ClaimGeneration        int64
		ClaimReleasedAt        pgtype.Timestamptz
	}
	read := func(t *testing.T, id uuid.UUID) observed {
		t.Helper()
		r, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return observed{r.Status, r.HoldReason, r.HoldCapturedHead, r.StatusSince, r.BudgetPausedSeconds,
			r.BudgetExtensionSeconds, r.Health, r.HealthReason, r.HealthSince, r.WorkerID, r.ClaimGeneration, r.ClaimReleasedAt}
	}
	assertStaleUnchanged := func(t *testing.T, id uuid.UUID, gen int64) {
		t.Helper()
		before := read(t, id)
		if before.Status != "running" || before.HoldReason.Valid {
			t.Fatalf("fixture not a running unheld run: %+v", before)
		}
		out := post(t, id, gen, http.StatusConflict)
		if out["reason"] != "stale_claim" {
			t.Fatalf("stale refusal reason = %v, want stale_claim: %v", out["reason"], out)
		}
		if after := read(t, id); after != before {
			t.Fatalf("a stale refusal changed the run:\nbefore %+v\nafter  %+v", before, after)
		}
	}

	t.Run("a superseded generation is a stale_claim 409 and changes nothing", func(t *testing.T) {
		assertStaleUnchanged(t, seedReconciled(t), generation-1)
	})

	t.Run("a released claim at the current generation is a stale_claim 409 and changes nothing", func(t *testing.T) {
		id := seedReconciled(t)
		exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
		assertStaleUnchanged(t, id, generation)
	})

	t.Run("the current live claim is held", func(t *testing.T) {
		id := seedReconciled(t)
		out := post(t, id, generation, http.StatusOK)
		run, _ := out["run"].(map[string]any)
		if run["status"] != "paused" {
			t.Fatalf("held run DTO status = %v, want paused: %v", run["status"], out)
		}
		after := read(t, id)
		if after.Status != "paused" || after.HoldReason.String != workersvc.HoldReasonBaseAttestationMissing ||
			after.HoldCapturedHead.String != head || after.ClaimGeneration != generation || after.ClaimReleasedAt.Valid {
			t.Fatalf("held run = %+v, want paused/%s at the live claim", after, workersvc.HoldReasonBaseAttestationMissing)
		}
	})
}
