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

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerPhaseSwitchHoldLiveDB pins the HTTP contract of
// POST /api/worker/runs/{id}/phase-switch-hold (PRD #3252 m4): 200 with the paused run on the hold
// (and on a lost-ACK repeat), 400 for a missing claim_generation or a bad phase, 404 for a run that
// is not this worker's, 409 with the real status and hold_reason for every refusal, including the
// owner's resume that already queued the run with the marker. The ownership probe then reports the
// queued marker at the same generation. The job-run refusal is TestWorkerJobRouteRefusalsLiveDB's row.
func TestWorkerPhaseSwitchHoldLiveDB(t *testing.T) {
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
	svc.SetTxBeginner(pool)
	svc.SetBackground(func(func()) {})

	exec := func(sql string, args ...any) {
		t.Helper()
		cliMustExec(t, pool, sql, args...)
	}
	owner, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`, owner, fmt.Sprintf("phase-hold-http-%s@e2e", owner))
	var runIDs []uuid.UUID
	t.Cleanup(func() {
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
 VALUES($1,$2,1,$3,'https://forge.e2e/g/r','main',true)`, repoID, connID, "g/phase-hold-http-"+repoID.String())
	workerToken, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'online',now())`, workerID, owner, "phase-hold-http-"+workerID.String(), hash)

	const generation = 3
	seed := func(t *testing.T, autoApprove bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		runIDs = append(runIDs, id)
		iid := int64(binary.BigEndian.Uint64(id[:8]) >> 1)
		exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,
 claim_generation,auto_approve)
 VALUES($1,$2,$3,'issue',$4,'test','test','running',$5,$6,$7)`, id, owner, repoID, iid, workerID, generation, autoApprove)
		return id
	}

	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	workerRouter := h.WorkerRoutes(mw.NewLimiter(100000, time.Minute, nil))
	const head = "0123456789abcdef0123456789abcdef01234567"
	post := func(t *testing.T, id uuid.UUID, body string, want int) map[string]any {
		t.Helper()
		rec := bearerReqBody(workerRouter, http.MethodPost, "/api/worker/runs/"+id.String()+"/phase-switch-hold", workerToken, body)
		if rec.Code != want {
			t.Fatalf("phase-switch-hold %s: status %d want %d: %s", body, rec.Code, want, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return out
	}
	dtoOf := func(out map[string]any) map[string]any {
		run, _ := out["run"].(map[string]any)
		return run
	}
	req := func(gen int64, phase string) string {
		return fmt.Sprintf(`{"head":%q,"claim_generation":%d,"phase":%q}`, head, gen, phase)
	}

	t.Run("a plan hold answers 200 with the paused run and repeats idempotently", func(t *testing.T) {
		id := seed(t, false)
		run := dtoOf(post(t, id, req(generation, "plan"), http.StatusOK))
		if run["status"] != "paused" || run["hold_reason"] != workersvc.HoldReasonPhaseSwitchUnavailable {
			t.Fatalf("held DTO = (%v, %v), want paused/%s", run["status"], run["hold_reason"], workersvc.HoldReasonPhaseSwitchUnavailable)
		}
		r, err := q.GetRunByID(ctx, id)
		if err != nil || r.HoldCapturedHead.String != head || r.ClaimGeneration != generation || r.ClaimReleasedAt.Valid {
			t.Fatalf("stored hold = (head %q, gen %d, released %v, %v)", r.HoldCapturedHead.String, r.ClaimGeneration, r.ClaimReleasedAt.Valid, err)
		}
		post(t, id, req(generation, "plan"), http.StatusOK)
		again, _ := q.GetRunByID(ctx, id)
		if !again.StatusSince.Time.Equal(r.StatusSince.Time) {
			t.Fatal("a lost-ACK repeat must not reset status_since")
		}
	})

	t.Run("Codex holds at approval and lost ACK does not bank twice", func(t *testing.T) {
		id := seed(t, true)
		exec("UPDATE runs SET harness = 'codex', status = 'awaiting_approval', status_since = now() - interval '30 seconds', session_id = 'plan-thread' WHERE id = $1", id)
		held := dtoOf(post(t, id, req(generation, "implement"), http.StatusOK))
		if held["status"] != "paused" || held["hold_reason"] != workersvc.HoldReasonPhaseSwitchUnavailable {
			t.Fatalf("Codex held DTO = %v", held)
		}
		first, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		post(t, id, req(generation, "implement"), http.StatusOK)
		again, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if first.BudgetPausedSeconds < 30 || first.BudgetPausedSeconds != again.BudgetPausedSeconds ||
			!first.StatusSince.Time.Equal(again.StatusSince.Time) || again.SessionID.String != "plan-thread" || again.ClaimReleasedAt.Valid {
			t.Fatal("Codex hold changed wait budget, session or claim on lost-ACK retry")
		}
	})

	t.Run("an implement hold needs an approved plan", func(t *testing.T) {
		approved := seed(t, true)
		if run := dtoOf(post(t, approved, req(generation, "implement"), http.StatusOK)); run["status"] != "paused" {
			t.Fatalf("approved implement hold status = %v, want paused", run["status"])
		}
		unapproved := seed(t, false)
		out := post(t, unapproved, req(generation, "implement"), http.StatusConflict)
		run := dtoOf(out)
		if run["status"] != "running" || run["hold_reason"] != nil {
			t.Fatalf("refused DTO = (%v, %v), want the real running status and no hold", run["status"], run["hold_reason"])
		}
		if out["reason"] != "plan_not_approved" {
			t.Fatalf("refused reason = %v, want plan_not_approved: %v", out["reason"], out)
		}
		// Every other 409 keeps its shape: no machine reason beside the run.
		if stale := post(t, seed(t, false), req(generation-1, "plan"), http.StatusConflict); stale["reason"] != nil {
			t.Fatalf("stale-generation 409 carries reason %v, want none", stale["reason"])
		}
	})

	t.Run("a body without a positive claim_generation is claim_generation_required", func(t *testing.T) {
		id := seed(t, false)
		for _, body := range []string{`{"phase":"plan"}`, `{"claim_generation":0,"phase":"plan"}`, `{"claim_generation":-1,"phase":"plan"}`} {
			out := post(t, id, body, http.StatusBadRequest)
			if out["reason"] != "claim_generation_required" {
				t.Fatalf("%s reason = %v, want claim_generation_required: %v", body, out["reason"], out)
			}
		}
	})

	t.Run("a bad phase or an unknown field is a 400 and changes nothing", func(t *testing.T) {
		id := seed(t, true)
		for _, body := range []string{
			req(generation, "bogus"), req(generation, ""), `{"claim_generation":3}`,
			`{"claim_generation":3,"phase":"plan","extra":1}`, `not json`,
		} {
			post(t, id, body, http.StatusBadRequest)
		}
		if r, err := q.GetRunByID(ctx, id); err != nil || r.Status != "running" || r.HoldReason.Valid {
			t.Fatalf("a rejected request changed the run: %q %v %v", r.Status, r.HoldReason, err)
		}
	})

	t.Run("a run that is not this worker's is 404", func(t *testing.T) {
		post(t, uuid.New(), req(generation, "plan"), http.StatusNotFound)
	})

	t.Run("a stale generation is 409 with the real status", func(t *testing.T) {
		id := seed(t, false)
		run := dtoOf(post(t, id, req(generation-1, "plan"), http.StatusConflict))
		if run["status"] != "running" {
			t.Fatalf("stale DTO status = %v, want running", run["status"])
		}
	})

	t.Run("after the owner's resume the retry is 409 queued with the marker and the probe agrees", func(t *testing.T) {
		id := seed(t, false)
		post(t, id, req(generation, "plan"), http.StatusOK)
		if _, err := q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: id, UserID: owner, GlobalTimeoutSeconds: 3600}); err != nil {
			t.Fatalf("ResumePausedRun: %v", err)
		}
		before, _ := q.GetRunByID(ctx, id)
		run := dtoOf(post(t, id, req(generation, "plan"), http.StatusConflict))
		if run["status"] != "queued" || run["hold_reason"] != workersvc.HoldReasonPhaseSwitchUnavailable {
			t.Fatalf("retry DTO = (%v, %v), want queued/%s", run["status"], run["hold_reason"], workersvc.HoldReasonPhaseSwitchUnavailable)
		}
		after, _ := q.GetRunByID(ctx, id)
		if after.Status != before.Status || after.ClaimGeneration != before.ClaimGeneration || after.WorkerID != before.WorkerID ||
			after.HoldReason != before.HoldReason || after.HoldCapturedHead != before.HoldCapturedHead ||
			!after.StatusSince.Time.Equal(before.StatusSince.Time) {
			t.Fatal("the refused retry changed the resumed row")
		}
		rec := bearerReqBody(workerRouter, http.MethodGet, "/api/worker/runs/"+id.String()+"/ownership", workerToken, "")
		var probe map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &probe) != nil {
			t.Fatalf("ownership: %d %s", rec.Code, rec.Body.String())
		}
		if probe["status"] != "queued" || probe["hold_reason"] != workersvc.HoldReasonPhaseSwitchUnavailable || probe["claim_generation"] != float64(generation) {
			t.Fatalf("ownership = %v, want queued/marker at generation %d", probe, generation)
		}
	})
}
