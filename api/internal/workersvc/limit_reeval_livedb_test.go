package workersvc

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
)

// This file is the live-DB half of PRD #1247 M3: it EXECUTES the new duration-time
// re-evaluation queries (ListLimitWaitReeval, LowerLimitWaitRetryNow) and the widened
// ListPoolWaitRuns against a REAL Postgres, which a fake store cannot vouch for — sqlc's
// type deduction is not Postgres's, and a LEFT-JOINed nullable column or a guarded UPDATE
// can pass `sqlc generate` yet fail at prepare/execute. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (setupCodexLiveDB skips).
//
// The clock is pinned a couple of minutes AHEAD of wall time: the parked runs' future
// retry_not_before is set relative to svc.now() (so they qualify for re-evaluation and are
// NOT promoted by the ordinary pass), while LowerLimitWaitRetryNow stamps the DB's real
// now() — which is BEHIND svc.now() — so the same tick's PromoteLimitWaitRuns (svc.now())
// sees the lowered stamp as due. The gauge is seeded at DB now(), well inside autoParams'
// 15-minute staleness window relative to svc.now().

type reevalOwner struct {
	userID, repoID, workerID uuid.UUID
	deadTok, altTok          uuid.UUID
}

// seedReevalOwner seeds one owner with a forge connection, a repo, a worker bound in the
// given anthropic bind mode, and two auto_eligible anthropic tokens (a dead one and an
// alternative). When altEligible, the alternative gets a fresh, high-headroom gauge so it
// classifies StatusEligible; otherwise it has no gauge and nothing is spendable.
func seedReevalOwner(t *testing.T, env codexTestEnv, bindMode string, altEligible bool) reevalOwner {
	t.Helper()
	userID := uuid.New()
	env.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("reeval-%s@e2e", userID))
	connID, repoID := uuid.New(), uuid.New()
	env.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	          VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte("x"))
	env.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	          VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, "g/"+repoID.String(), "https://forge.e2e/g/"+repoID.String())
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, anthropic_bind_mode)
	          VALUES ($1, $2, $3, $4, 'online', $5)`, workerID, userID, "w-"+workerID.String(), workerID[:], bindMode)

	deadTok := env.seedAnthropicSecret(t, userID, "dead-"+uuid.NewString(), false)
	altTok := env.seedAnthropicSecret(t, userID, "alt-"+uuid.NewString(), false)
	// Both pooled; the ranker/exclude never see a token the owner did not opt in.
	env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, deadTok)
	env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, altTok)
	if altEligible {
		env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
		          VALUES ($1, $2, 20, 10, 'usage_endpoint', now())`, altTok, userID)
	}
	return reevalOwner{userID: userID, repoID: repoID, workerID: workerID, deadTok: deadTok, altTok: altTok}
}

// seedParkedLimitWait inserts one run parked in limit_wait for owner o: it recorded the
// dead token, carries a FUTURE retry_not_before (relative to svc.now()), and is owned by
// o's worker so the D8 pass's LEFT JOIN reads that worker's bind mode. status_since drives
// the oldest-first ordering.
func seedParkedLimitWait(t *testing.T, env codexTestEnv, o reevalOwner, issueIID int64, statusSince, retryNotBefore time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, status_since, worker_id, anthropic_secret_id, limit_dead_secret_id,
	             retry_not_before, limit_wait_count, wait_on_limit, started_at)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'limit_wait', $5, $6, $7, $7, $8, 1, true, now())`,
		id, o.userID, o.repoID, issueIID, statusSince, o.workerID, o.deadTok, pgconv.Time(retryNotBefore))
	return id
}

func statusOf(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := env.pool.QueryRow(env.ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read status %s: %v", id, err)
	}
	return status
}

// TestReEvaluateParkedLimitWaitPromotesOnePerOwnerLiveDB is the D8 duration-time pass end
// to end: two owners each with two parked `auto` limit_wait runs and a now-eligible pooled
// alternative, plus a third owner with a single PINNED parked run. ONE Sweep lowers AND
// promotes exactly the oldest run of each auto owner (same tick — the re-eval pass runs
// before PromoteLimitWaitRuns), leaves each owner's newer run parked, and never touches the
// pinned run. The next Sweep takes each owner's remaining run.
//
// This FAILS on pre-M3 code: without the reEvaluateParkedLimitWaitRuns pass, a future
// retry_not_before is never lowered, so every run stays limit_wait forever (verified by
// reading the pre-M3 Sweep, which has no such pass).
func TestReEvaluateParkedLimitWaitPromotesOnePerOwnerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, autoParams())
	now := time.Now().UTC().Add(2 * time.Minute)
	svc.now = func() time.Time { return now }
	ctx := env.ctx

	// Isolate the parked namespace so the counter assertions see only this test's runs.
	env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'pool_wait')`)

	future := now.Add(time.Hour) // still-closed window: future relative to svc.now()
	o1 := seedReevalOwner(t, env, BindModeAuto, true)
	o2 := seedReevalOwner(t, env, BindModeAuto, true)
	o3 := seedReevalOwner(t, env, BindModePinned, true) // has an eligible alt, but pinned

	a1 := seedParkedLimitWait(t, env, o1, 1, now.Add(-10*time.Minute), future) // owner1 older
	b1 := seedParkedLimitWait(t, env, o1, 2, now.Add(-1*time.Minute), future)  // owner1 newer
	c2 := seedParkedLimitWait(t, env, o2, 3, now.Add(-10*time.Minute), future) // owner2 older
	d2 := seedParkedLimitWait(t, env, o2, 4, now.Add(-1*time.Minute), future)  // owner2 newer
	p3 := seedParkedLimitWait(t, env, o3, 5, now.Add(-10*time.Minute), future) // owner3 pinned

	res1, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep 1: %v", err)
	}
	if res1.LimitReevaluated != 2 {
		t.Fatalf("Sweep 1 LimitReevaluated = %d, want 2 (one auto owner each, oldest first)", res1.LimitReevaluated)
	}
	if res1.LimitPromoted != 2 {
		t.Fatalf("Sweep 1 LimitPromoted = %d, want 2 — the lowered runs promote in the SAME tick", res1.LimitPromoted)
	}
	if s := statusOf(t, env, a1); s != "queued" {
		t.Fatalf("owner1 older run status = %q, want queued — its window was lowered and promoted this tick", s)
	}
	if s := statusOf(t, env, b1); s != "limit_wait" {
		t.Fatalf("owner1 newer run status = %q, want limit_wait — at most one per owner per tick", s)
	}
	if s := statusOf(t, env, c2); s != "queued" {
		t.Fatalf("owner2 older run status = %q, want queued", s)
	}
	if s := statusOf(t, env, d2); s != "limit_wait" {
		t.Fatalf("owner2 newer run status = %q, want limit_wait", s)
	}
	if s := statusOf(t, env, p3); s != "limit_wait" {
		t.Fatalf("owner3 PINNED run status = %q, want limit_wait — a pinned next claim would re-park, so it is never promoted early", s)
	}

	// Next tick: each owner's remaining run is taken; the pinned run stays parked.
	res2, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep 2: %v", err)
	}
	if res2.LimitReevaluated != 2 || res2.LimitPromoted != 2 {
		t.Fatalf("Sweep 2 reevaluated=%d promoted=%d, want 2/2 (each owner's remaining run)", res2.LimitReevaluated, res2.LimitPromoted)
	}
	if s := statusOf(t, env, b1); s != "queued" {
		t.Fatalf("owner1 newer run status = %q after tick 2, want queued", s)
	}
	if s := statusOf(t, env, d2); s != "queued" {
		t.Fatalf("owner2 newer run status = %q after tick 2, want queued", s)
	}
	if s := statusOf(t, env, p3); s != "limit_wait" {
		t.Fatalf("owner3 pinned run status = %q after tick 2, want limit_wait — it must NEVER be promoted early", s)
	}
}

// 🔴 TestReEvaluateParkedLimitWaitPinLiveDB is the 2026-09-15 pin: a run parked with NO
// eligible alternative sleeps to its retry_not_before as before (the ordinary promote does
// not touch it), and only once an alternative becomes eligible does the NEXT sweep tick
// lower-and-promote it. This is the exact scenario the PRD exists for: token B pools/turns
// eligible AFTER the park and nobody re-evaluated. It FAILS on pre-M3 code, which has no
// duration-time pass — the run would stay parked after the alternative became eligible.
func TestReEvaluateParkedLimitWaitPinLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, autoParams())
	now := time.Now().UTC().Add(2 * time.Minute)
	svc.now = func() time.Time { return now }
	ctx := env.ctx

	env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'pool_wait')`)

	// altEligible=false: the alternative token is pooled but has NO gauge, so nothing is
	// spendable and the run must stay parked on the first sweep.
	o := seedReevalOwner(t, env, BindModeAuto, false)
	run := seedParkedLimitWait(t, env, o, 1, now.Add(-time.Minute), now.Add(time.Hour))

	res, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep (no alternative): %v", err)
	}
	if res.LimitReevaluated != 0 {
		t.Fatalf("LimitReevaluated = %d with no eligible alternative, want 0", res.LimitReevaluated)
	}
	if s := statusOf(t, env, run); s != "limit_wait" {
		t.Fatalf("status = %q with no eligible alternative, want limit_wait — nothing spendable, so no early promote", s)
	}

	// Token B becomes eligible: a fresh, high-headroom gauge appears on the alternative.
	env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
	          VALUES ($1, $2, 20, 10, 'usage_endpoint', now())`, o.altTok, o.userID)

	res2, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep (alternative eligible): %v", err)
	}
	if res2.LimitReevaluated != 1 || res2.LimitPromoted != 1 {
		t.Fatalf("after the alternative turned eligible: reevaluated=%d promoted=%d, want 1/1 — the duration-time pass resumes it",
			res2.LimitReevaluated, res2.LimitPromoted)
	}
	if s := statusOf(t, env, run); s != "queued" {
		t.Fatalf("status = %q after the alternative became eligible, want queued — this is the 2026-09-15 scenario resolving with no human", s)
	}
}

// TestPoolWaitSoleDeadTokenNoChurnLiveDB is the pool-promoter fix (PRD #1247 M3, Part 3)
// against real SQL: a pool_wait run carrying a FUTURE retry_not_before whose SOLE
// AutoEligible token is its own dead credential — the state early promotion (M4's verb, D8)
// can produce — must NOT be resumed, because autoselect.Floor(cands, claimExclude(run)) is
// false after the still-in-effect exclude. It must also not be churned every tick. This
// EXECUTES the widened ListPoolWaitRuns (the two new projected columns) end to end.
//
// It FAILS on pre-M3 code, whose exclude-blind PoolNonEmpty loop counts the dead token as
// AutoEligible and resumes the run every tick.
func TestPoolWaitSoleDeadTokenNoChurnLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, autoParams())
	now := time.Now().UTC().Add(2 * time.Minute)
	svc.now = func() time.Time { return now }
	ctx := env.ctx

	env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'pool_wait')`)

	// One owner whose ONLY pooled token is the dead one (no separate alt gauge needed;
	// Floor keys off auto_eligible, not the gauge).
	o := seedReevalOwner(t, env, BindModeAuto, false)
	runID := uuid.New()
	// A pool_wait run with a FUTURE retry_not_before and the dead token recorded.
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, status_since, worker_id, anthropic_secret_id, limit_dead_secret_id,
	             retry_not_before, wait_on_limit)
	          VALUES ($1, $2, $3, 'issue', 9, 't', 'd', 'pool_wait', now(), $4, $5, $5, $6, true)`,
		runID, o.userID, o.repoID, o.workerID, o.deadTok, pgconv.Time(now.Add(time.Hour)))
	// Make the dead token the SOLE pooled candidate: un-pool the alt so Floor's only
	// AutoEligible option is the excluded dead credential.
	env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE id = $1`, o.altTok)

	var updatedBefore time.Time
	if err := env.pool.QueryRow(ctx, `SELECT updated_at FROM runs WHERE id = $1`, runID).Scan(&updatedBefore); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	for tick := 1; tick <= 2; tick++ {
		res, err := svc.Sweep(ctx)
		if err != nil {
			t.Fatalf("Sweep %d: %v", tick, err)
		}
		if res.PoolResumed != 0 {
			t.Fatalf("Sweep %d PoolResumed = %d, want 0 — the sole pooled token is the still-excluded dead credential", tick, res.PoolResumed)
		}
		if s := statusOf(t, env, runID); s != "pool_wait" {
			t.Fatalf("Sweep %d status = %q, want pool_wait — a sole-dead-token future-stamp hold must not resume", tick, s)
		}
	}
	var updatedAfter time.Time
	if err := env.pool.QueryRow(ctx, `SELECT updated_at FROM runs WHERE id = $1`, runID).Scan(&updatedAfter); err != nil {
		t.Fatalf("read updated_at after: %v", err)
	}
	if !updatedAfter.Equal(updatedBefore) {
		t.Fatalf("updated_at moved (%v -> %v); the held run was churned by the resume pass despite Floor.ok being false",
			updatedBefore, updatedAfter)
	}

	// Positive control: once the window reopens (retry_not_before in the past), the exclude
	// relaxes and the same dead token is spendable again — the run resumes. Without this a
	// mutation that NEVER resumes would pass the no-churn assertion above.
	env.exec(`UPDATE runs SET retry_not_before = $1 WHERE id = $2`, pgconv.Time(now.Add(-time.Hour)), runID)
	res, err := svc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep (reopened): %v", err)
	}
	if res.PoolResumed != 1 {
		t.Fatalf("PoolResumed = %d after the window reopened, want 1 — claimExclude relaxes and Floor may spend the token", res.PoolResumed)
	}
	if s := statusOf(t, env, runID); s != "queued" {
		t.Fatalf("status = %q after the window reopened, want queued", s)
	}
}
