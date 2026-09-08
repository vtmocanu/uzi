package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSetRunAutopilotPlanLiveDB exercises RC1 (issue #1197) end to end against a REAL
// Postgres — the parts the pure-Go unit tests structurally cannot answer, because the
// guarded write's whole contract lives in its SQL WHERE clause:
//
//	(1) a FIRST write on an autopilot row (auto_approve=true, plan_source='agent',
//	    plan_md NULL, status='running') stores the body, returns rows==1, and leaves
//	    plan_source='agent';
//	(2) an IDENTICAL retry is idempotent (rows==1, body unchanged) — the
//	    `plan_md IS NULL OR plan_md = @plan_md` clause matches the already-stored body;
//	(3) a DIFFERENT body on an already-set row is REFUSED (rows==0) with the stored body
//	    untouched — write-once;
//	(4) an auto_approve=false row is REFUSED (rows==0, no mutation) — the human-gate guard;
//	(5) a plan_source='seeded' row keeps its seeded body/source (rows==0) — the provenance
//	    allowlist never overwrites a user-authored plan;
//	(6) a TERMINAL ('failed') and a PARKED ('limit_wait') row are both REFUSED (rows==0,
//	    plan_md/plan_source unchanged) — the status IN ('claimed','running') guard;
//	(7) persistence-to-resume: after a successful write, a claimable-state row round-trips
//	    through the REAL svc.Claim path and its ClaimPayload carries the stored plan_md with
//	    PlanApproved=true — proving a resumed autopilot run enters implementation instead of
//	    re-planning (the incident this fix closes).
//
// Non-vacuity: the refusal cases (3)-(6) each re-read the row and assert the guarded columns
// did NOT move, so a green result cannot come from an absent/mis-wired guard — it comes from
// the WHERE clause admitting exactly the legitimate write and refusing the rest.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is
// INVALID, not green.
func TestSetRunAutopilotPlanLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	box := newBox(t)
	q := store.New(pool)
	svc := New(q, box, testParams())

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	// A real box-sealed bot PAT so the claim assembly gets past PAT decryption; a default
	// box-sealed Anthropic token so openAnthropic resolves a credential — an ordinary run
	// always spends one (mirrors TestCodexDarkNoCodexOnNormalClaimPathLiveDB's setup).
	sealedPAT, err := box.Seal([]byte("bot-pat-RC1-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedAnthropic, err := box.Seal([]byte("anthropic-RC1-token-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rc1-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rc1', 'https://forge.e2e/g/rc1', 'main', true)`, repoID, connID)
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), sealedAnthropic)

	// A worker under the user, brought ONLINE so svc.Claim can pick up the resume run.
	wkrID := uuid.New()
	tokenHash := wkrID[:] // unique per run (workers_token_hash_key); Migrate does not truncate
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-rc1', $3, 'offline')`,
		wkrID, userID, tokenHash)
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: wkrID}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	wkr, err := q.GetWorkerByID(ctx, wkrID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}

	// seedRun inserts one issue run owned by wkr. Distinct issue_iid per call so the partial
	// unique index uq_runs_one_active_per_issue (repo_id, issue_iid WHERE status non-terminal)
	// never collides across the non-terminal fixtures. planMd is nil (SQL NULL) or a string.
	seedRun := func(issueIID int64, status, planSource string, autoApprove bool, planMd any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, auto_approve, plan_source, plan_md)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, $8, $9)`,
			id, userID, repoID, issueIID, status, wkrID, autoApprove, planSource, planMd)
		return id
	}
	reread := func(id uuid.UUID) store.Run {
		t.Helper()
		r, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatalf("GetRunByID(%s): %v", id, err)
		}
		return r
	}
	write := func(id uuid.UUID, body string) int64 {
		t.Helper()
		rows, err := q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{
			PlanMd:   pgconv.TextOrNull(body),
			ID:       id,
			WorkerID: pgconv.UUID(wkrID),
		})
		if err != nil {
			t.Fatalf("SetRunAutopilotPlan(%s): %v", id, err)
		}
		return rows
	}

	const planBody = "## Approved plan\n1. implement RC1\n2. add tests\n"

	t.Run("first write stores the body on an autopilot row", func(t *testing.T) {
		id := seedRun(1, "running", "agent", true, nil)
		if rows := write(id, planBody); rows != 1 {
			t.Fatalf("first write rows = %d, want 1 (the plan must be durably stored)", rows)
		}
		r := reread(id)
		if !r.PlanMd.Valid || r.PlanMd.String != planBody {
			t.Fatalf("plan_md = {valid=%t %q}, want the stored body", r.PlanMd.Valid, r.PlanMd.String)
		}
		if r.PlanSource != "agent" {
			t.Fatalf("plan_source = %q, want agent", r.PlanSource)
		}

		// (2) identical retry is idempotent: rows==1, body unchanged.
		if rows := write(id, planBody); rows != 1 {
			t.Fatalf("identical retry rows = %d, want 1 (idempotent success)", rows)
		}
		if r := reread(id); r.PlanMd.String != planBody {
			t.Fatalf("plan_md after idempotent retry = %q, want it unchanged", r.PlanMd.String)
		}

		// (3) a DIFFERENT body on an already-set row is refused, body untouched.
		if rows := write(id, "## A totally different plan\n"); rows != 0 {
			t.Fatalf("different-body write rows = %d, want 0 (write-once refuses)", rows)
		}
		if r := reread(id); r.PlanMd.String != planBody {
			t.Fatalf("plan_md after a refused different-body write = %q, want the original %q", r.PlanMd.String, planBody)
		}
	})

	t.Run("auto_approve=false row is refused with no mutation", func(t *testing.T) {
		id := seedRun(2, "running", "agent", false, nil)
		if rows := write(id, planBody); rows != 0 {
			t.Fatalf("auto_approve=false write rows = %d, want 0 (human-gate guard)", rows)
		}
		if r := reread(id); r.PlanMd.Valid {
			t.Fatalf("plan_md = %q, want it to stay NULL on a refused auto_approve=false row", r.PlanMd.String)
		}
	})

	t.Run("seeded plan_source is preserved (refused)", func(t *testing.T) {
		const seededBody = "user-authored seeded plan"
		id := seedRun(3, "running", "seeded", true, seededBody)
		if rows := write(id, planBody); rows != 0 {
			t.Fatalf("seeded-row write rows = %d, want 0 (provenance allowlist)", rows)
		}
		r := reread(id)
		if r.PlanSource != "seeded" || r.PlanMd.String != seededBody {
			t.Fatalf("seeded row mutated: plan_source=%q plan_md=%q, want seeded/%q preserved", r.PlanSource, r.PlanMd.String, seededBody)
		}
	})

	t.Run("terminal and parked rows are refused with no mutation", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			iid    int64
			status string
		}{
			{"failed (terminal)", 4, "failed"},
			{"limit_wait (parked)", 5, "limit_wait"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id := seedRun(tc.iid, tc.status, "agent", true, nil)
				if rows := write(id, planBody); rows != 0 {
					t.Fatalf("%s write rows = %d, want 0 (status guard)", tc.status, rows)
				}
				r := reread(id)
				if r.PlanMd.Valid {
					t.Fatalf("%s row: plan_md = %q, want it to stay NULL", tc.status, r.PlanMd.String)
				}
				if r.Status != tc.status {
					t.Fatalf("%s row: status = %q, want it unchanged", tc.status, r.Status)
				}
			})
		}
	})

	t.Run("persistence-to-resume: a stored plan rides svc.Claim with PlanApproved", func(t *testing.T) {
		id := seedRun(6, "running", "agent", true, nil)
		if rows := write(id, planBody); rows != 1 {
			t.Fatalf("resume-fixture write rows = %d, want 1", rows)
		}
		if r := reread(id); !r.PlanMd.Valid || r.PlanMd.String != planBody {
			t.Fatalf("plan not stored before resume: plan_md = {valid=%t %q}", r.PlanMd.Valid, r.PlanMd.String)
		}
		// Return the run to a claimable state (worker_id kept for resume affinity, so the
		// spread bypass + affinity clauses in ClaimRun make it claimable by this worker).
		exec(`UPDATE runs SET status = 'queued', started_at = NULL, claimed_at = NULL WHERE id = $1`, id)

		payload, err := svc.Claim(ctx, wkr)
		if err != nil {
			t.Fatalf("svc.Claim: %v", err)
		}
		if payload == nil {
			t.Fatal("svc.Claim returned nil (idle) — the queued autopilot run was not claimed")
		}
		if payload.RunID != id.String() {
			t.Fatalf("claimed the wrong run: got %s, want %s", payload.RunID, id)
		}
		if payload.PlanMd == nil || *payload.PlanMd != planBody {
			t.Fatalf("ClaimPayload.PlanMd = %v, want the stored body %q — a resume would re-plan without it", payload.PlanMd, planBody)
		}
		if !payload.PlanApproved {
			t.Fatal("ClaimPayload.PlanApproved = false, want true (auto_approve=true) — the resumed run must enter implementation, not the gate")
		}
	})

	t.Run("a different worker cannot write the plan", func(t *testing.T) {
		id := seedRun(7, "running", "agent", true, nil)
		before := reread(id)
		rows, err := q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{
			PlanMd:   pgconv.TextOrNull(planBody),
			ID:       id,
			WorkerID: pgconv.UUID(uuid.New()),
		})
		if err != nil {
			t.Fatalf("SetRunAutopilotPlan: %v", err)
		}
		if rows != 0 {
			t.Fatalf("wrong-worker plan write rows = %d, want 0", rows)
		}
		after := reread(id)
		if after.PlanMd != before.PlanMd || after.PlanSource != before.PlanSource ||
			after.WorkerID != before.WorkerID || after.UpdatedAt != before.UpdatedAt {
			t.Fatal("a refused wrong-worker plan write mutated the run")
		}
	})
}
