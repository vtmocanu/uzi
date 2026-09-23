package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1551 M4 (D6) LiveDB coverage of the fail-closed CUSTOM-Codex-model claim gate. A Codex run
// whose EFFECTIVE worker-root model is a CUSTOM (non-curated) id is claimable / peer-deferrable /
// counted-available ONLY by a worker advertising 'codex_custom_model_v1' (in addition to
// 'codex_harness_v1'), so an old codex_harness_v1-only worker can never receive a custom root and
// silently run Astra. Task-review, judge and chat are EXEMPT — the exemption is keyed on
// review_target_run_id/kind, NEVER on all of kind='task'. These reuse the interlockLiveDB harness
// (its claimParams already passes the real curated set). This is a credential-routing SECURITY
// boundary, so it runs against a real throwaway Postgres — skipped unless UZI_TEST_DATABASE_URL
// points at one (run via ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID.

const (
	// customCodexModel is a syntactically valid but NON-curated Codex model id (an escape-hatch
	// value a user could save into their default_codex_model lane, PRD #1551 D5).
	customCodexModel = "gpt-6-custom-preview"
	// curatedCodexModel is one of the three curated ids the gate treats as claimable by any
	// codex_harness_v1 worker.
	curatedCodexModel = "gpt-6-sol"
)

// setUserCodexLane sets (lane != nil) or clears (nil) the run owner's default_codex_model.
func (e interlockLiveDB) setUserCodexLane(t *testing.T, lane *string) {
	t.Helper()
	var v any
	if lane != nil {
		v = *lane
	}
	e.exec(t, `UPDATE users SET default_codex_model = $2 WHERE id = $1`, e.userID, v)
}

// setUserClaudeLane sets (lane != nil) or clears (nil) the run owner's default_claude_model.
func (e interlockLiveDB) setUserClaudeLane(t *testing.T, lane *string) {
	t.Helper()
	var v any
	if lane != nil {
		v = *lane
	}
	e.exec(t, `UPDATE users SET default_claude_model = $2 WHERE id = $1`, e.userID, v)
}

// seedCodexIssueRunWithModel inserts a queued Codex ISSUE run with runs.model set (nil ⇒ NULL, i.e.
// the effective root falls to the owner's default_codex_model lane). worker_id NULL so affinity
// never pins it; required_capabilities '{}' so fn_worker_can_claim is trivially satisfiable and the
// D6 custom-model clause is the ONLY thing that can gate it.
func (e interlockLiveDB) seedCodexIssueRunWithModel(t *testing.T, model *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	var m any
	if model != nil {
		m = *model
	}
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id, harness, model)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'queued', NULL, 'codex', $5)`,
		id, e.userID, e.repoID, iid, m)
	return id
}

// seedCodexTaskRun inserts a queued Codex TASK run. reviewTarget non-nil makes it a task-REVIEW run
// (kind='task', review_target_run_id set) — the D6 exemption case; nil makes it an ordinary task
// handoff (review_target_run_id NULL) — NOT exempt. dispatched_at is set so the run is claimable
// (ClaimRun requires kind<>'task' OR dispatched_at IS NOT NULL).
func (e interlockLiveDB) seedCodexTaskRun(t *testing.T, reviewTarget *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	branch := "uzi/task/" + id.String()
	var rt any
	if reviewTarget != nil {
		rt = *reviewTarget
	}
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, worker_id, harness)
	           VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), 't', 'd', 'queued', NULL, 'codex')`,
		id, e.userID, e.repoID, branch, rt)
	return id
}

// seedCompletedTaskTarget inserts a completed Codex task run to serve as a review run's target.
func (e interlockLiveDB) seedCompletedTaskTarget(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	branch := "uzi/task/" + id.String()
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, issue_title, issue_description, status, harness)
	           VALUES ($1, $2, $3, 'task', $4, 'main', 't', 'd', 'completed', 'codex')`,
		id, e.userID, e.repoID, branch)
	return id
}

// seedCodexJudgeRun inserts a queued Codex JUDGE run (repo-less, target_run_id set per the
// runs_kind_shape CHECK), for the judge-exemption assertion.
func (e interlockLiveDB) seedCodexJudgeRun(t *testing.T) uuid.UUID {
	t.Helper()
	target := e.seedCompletedTaskTarget(t)
	id := uuid.New()
	e.exec(t, `INSERT INTO runs (id, user_id, kind, status, worker_id, harness, target_run_id, issue_title, issue_description)
	           VALUES ($1, $2, 'judge', 'queued', NULL, 'codex', $3, 't', 'd')`,
		id, e.userID, target)
	return id
}

// seedHeartbeatWorker inserts an ONLINE worker with a fresh heartbeat (so it counts in
// CountOnlineWorkersClaimableForRun, which requires last_heartbeat_at >= cutoff) and an unbounded
// run-lane cap (NULL max_concurrent_runs), advertising the given protocol capabilities.
func (e interlockLiveDB) seedHeartbeatWorker(t *testing.T, protocolCaps []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if protocolCaps == nil {
		protocolCaps = []string{}
	}
	e.exec(t, `INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities, last_heartbeat_at)
	           VALUES ($1, $2, $3, $4, 'online', $5, now())`,
		id, e.userID, "w-"+id.String()[:8], id[:], protocolCaps)
	return id
}

// claimableForRun runs CountOnlineWorkersClaimableForRun with production-shaped params (the real
// curated set, a recent heartbeat cutoff, capability_aware false so only the protocol clauses gate).
func (e interlockLiveDB) claimableForRun(t *testing.T, runID uuid.UUID) int64 {
	t.Helper()
	n, err := e.q.CountOnlineWorkersClaimableForRun(e.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID:              runID,
		HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		CapabilityAware:    false,
		CodexCuratedModels: codexCuratedModelsSlice(),
	})
	if err != nil {
		t.Fatalf("CountOnlineWorkersClaimableForRun: %v", err)
	}
	return n
}

// healthRowFor fetches the SQL-computed ListActiveRunsForHealth row for runID (the shared DB holds
// many rows; we filter to this one), proving the codex_custom_root projection is computed in SQL.
func (e interlockLiveDB) healthRowFor(t *testing.T, runID uuid.UUID) store.ListActiveRunsForHealthRow {
	t.Helper()
	rows, err := e.q.ListActiveRunsForHealth(e.ctx, codexCuratedModelsSlice())
	if err != nil {
		t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if uuid.UUID(r.ID) == runID {
			return r
		}
	}
	t.Fatalf("run %v not found in ListActiveRunsForHealth", runID)
	return store.ListActiveRunsForHealthRow{}
}

// TestClaimCustomCodexModelGateLiveDB is the core D6 claimant assertion: a Codex run whose effective
// worker-root model is CUSTOM is NOT claimable by a codex_harness_v1-only worker (capability_aware
// false does not bypass — the clause is standalone), but a codex_custom_model_v1 worker claims it; a
// curated or NULL lane stays claimable by the harness-only worker.
//
// CALIBRATION (custom-model claimant clause): delete the claimant custom-Codex clause in runtime.sql
// (the `AND ( NOT ( r.harness = 'codex' AND ... COALESCE(...) ) OR 'codex_custom_model_v1' = ANY(...))`
// added after the codex-harness clause) and regenerate — the harness-only worker then claims the
// custom-lane run and the first sub-case fails.
func TestClaimCustomCodexModelGateLiveDB(t *testing.T) {
	// Each subtest gets a FRESH user (setupInterlockLiveDB): ClaimRun claims the OLDEST claimable
	// queued run for the whole user, so a subtest that leaves a run queued would be stolen by the
	// next subtest's claim. Fresh users keep every claim scoped to exactly its own seeded run.
	t.Run("custom lane not claimable by codex_harness_v1-only worker; stays queued", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		lane := customCodexModel
		e.setUserCodexLane(t, &lane)
		harnessOnly := e.seedWorker(t, []string{capability.CodexHarnessV1})
		runID := e.seedCodexIssueRunWithModel(t, nil) // NULL model ⇒ effective root = the custom lane
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(harnessOnly, []string{capability.CodexHarnessV1}, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("custom-root Codex run claimed by a codex_harness_v1-only worker (err=%v); the D6 clause must block it", err)
		}
		if s := e.runStatus(t, runID); s != "queued" {
			t.Fatalf("run must stay queued after the blocked claim; status = %q", s)
		}
	})

	t.Run("custom lane claimed by codex_custom_model_v1 worker", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		lane := customCodexModel
		e.setUserCodexLane(t, &lane)
		capable := e.seedWorker(t, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1})
		runID := e.seedCodexIssueRunWithModel(t, nil)
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(capable, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1}, false))
		if err != nil {
			t.Fatalf("codex_custom_model_v1 worker must claim the custom-root run: %v", err)
		}
		if run.ID != runID {
			t.Fatalf("claimed %v, want %v", run.ID, runID)
		}
	})

	t.Run("curated FROZEN model wins ⇒ claimable by codex_harness_v1-only worker", func(t *testing.T) {
		e := setupInterlockLiveDB(t)
		lane := customCodexModel
		e.setUserCodexLane(t, &lane) // custom lane, but the frozen curated model overrides it
		harnessOnly := e.seedWorker(t, []string{capability.CodexHarnessV1})
		curated := curatedCodexModel
		runID := e.seedCodexIssueRunWithModel(t, &curated) // frozen curated model ⇒ effective root curated
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(harnessOnly, []string{capability.CodexHarnessV1}, false))
		if err != nil {
			t.Fatalf("a curated frozen model must be claimable by a codex_harness_v1-only worker even with a custom lane: %v", err)
		}
		if run.ID != runID {
			t.Fatalf("claimed %v, want %v", run.ID, runID)
		}
	})

	t.Run("NULL lane ⇒ claimable by codex_harness_v1-only worker", func(t *testing.T) {
		e := setupInterlockLiveDB(t) // no lane set ⇒ NULL
		harnessOnly := e.seedWorker(t, []string{capability.CodexHarnessV1})
		runID := e.seedCodexIssueRunWithModel(t, nil) // NULL model, NULL lane ⇒ effective root NULL ⇒ curated
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(harnessOnly, []string{capability.CodexHarnessV1}, false))
		if err != nil {
			t.Fatalf("a NULL-lane Codex run must be claimable by a codex_harness_v1-only worker: %v", err)
		}
		if run.ID != runID {
			t.Fatalf("claimed %v, want %v", run.ID, runID)
		}
	})
}

// TestClaimCustomCodexReviewExemptLiveDB is the REQUIRED D6 regression: with the owner's Codex lane
// set to a CUSTOM id, a Codex task-REVIEW run (kind='task', review_target_run_id set) is claimable by
// a codex_harness_v1-only worker (assembly reads no Codex lane for a review), while an ordinary Codex
// task HANDOFF (review_target_run_id NULL) with the SAME lane is NOT — and a codex_custom_model_v1
// worker claims the handoff.
//
// MUTATION (exemption keyed on review_target_run_id, NOT kind='task'): change the claimant clause's
// `r.review_target_run_id IS NULL` to `r.kind <> 'task'` (or delete the review exemption) and
// regenerate — the review sub-case then blocks the harness-only worker (ErrNoRows) and FAILS, while
// the handoff sub-case would wrongly become claimable if the exemption were widened to all task runs.
func TestClaimCustomCodexReviewExemptLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	lane := customCodexModel
	e.setUserCodexLane(t, &lane)

	t.Run("codex task-review run is claimable by a codex_harness_v1-only worker", func(t *testing.T) {
		harnessOnly := e.seedWorker(t, []string{capability.CodexHarnessV1})
		target := e.seedCompletedTaskTarget(t)
		reviewID := e.seedCodexTaskRun(t, &target)

		// It is NOT flagged custom-root by the SQL projection (review is exempt), and it is claimable.
		if e.healthRowFor(t, reviewID).CodexCustomRoot {
			t.Fatal("a codex task-review run must NOT be flagged codex_custom_root (review_target_run_id exempt)")
		}
		if n := e.claimableForRun(t, e.withHeartbeatWorker(t, reviewID, []string{capability.CodexHarnessV1})); n == 0 {
			t.Fatal("CountOnlineWorkersClaimableForRun must count a codex_harness_v1-only worker for a review run")
		}
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(harnessOnly, []string{capability.CodexHarnessV1}, false))
		if err != nil {
			t.Fatalf("a codex task-review run must be claimable by a codex_harness_v1-only worker despite a custom lane: %v", err)
		}
		if run.ID != reviewID {
			t.Fatalf("claimed %v, want the review run %v", run.ID, reviewID)
		}
	})

	t.Run("ordinary codex task handoff with the same custom lane is NOT claimable by harness-only", func(t *testing.T) {
		harnessOnly := e.seedWorker(t, []string{capability.CodexHarnessV1})
		handoffID := e.seedCodexTaskRun(t, nil) // review_target_run_id NULL ⇒ NOT exempt

		if !e.healthRowFor(t, handoffID).CodexCustomRoot {
			t.Fatal("a custom-lane codex task HANDOFF (review_target_run_id NULL) must be flagged codex_custom_root")
		}
		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(harnessOnly, []string{capability.CodexHarnessV1}, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("a custom-lane codex task handoff must NOT be claimable by a codex_harness_v1-only worker (err=%v)", err)
		}
		if s := e.runStatus(t, handoffID); s != "queued" {
			t.Fatalf("handoff must stay queued; status = %q", s)
		}

		capable := e.seedWorker(t, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1})
		run, err := e.q.ClaimRun(e.ctx, e.claimParams(capable, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1}, false))
		if err != nil {
			t.Fatalf("a codex_custom_model_v1 worker must claim the custom-lane handoff: %v", err)
		}
		if run.ID != handoffID {
			t.Fatalf("claimed %v, want the handoff %v", run.ID, handoffID)
		}
	})
}

// withHeartbeatWorker seeds one heartbeat worker with the given caps and returns runID unchanged, so
// a claimableForRun call has at least one countable worker online. Returned runID keeps call sites terse.
func (e interlockLiveDB) withHeartbeatWorker(t *testing.T, runID uuid.UUID, caps []string) uuid.UUID {
	t.Helper()
	e.seedHeartbeatWorker(t, caps)
	return runID
}

// TestCountAndProjectionCustomCodexLiveDB pins CountOnlineWorkersClaimableForRun and the SQL
// codex_custom_root projection: a custom-root run excludes a codex_harness_v1-only worker and counts a
// codex_custom_model_v1 worker; a curated/NULL/review/judge run is never flagged custom-root.
//
// CALIBRATION (count mirror): delete the CountOnlineWorkersClaimableForRun custom clause and
// regenerate — the harness-only worker is then counted for the custom-root run and the first
// assertion fails. (Projection mirror: delete the codex_custom_root SELECT expression in
// ListActiveRunsForHealth and regenerate — the flag reads false and the projection assertions fail.)
func TestCountAndProjectionCustomCodexLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	custom := customCodexModel
	e.setUserCodexLane(t, &custom)

	t.Run("custom-root run: harness-only excluded, custom-capable counted", func(t *testing.T) {
		e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1}) // harness-only, must NOT count
		runID := e.seedCodexIssueRunWithModel(t, nil)
		if n := e.claimableForRun(t, runID); n != 0 {
			t.Fatalf("custom-root run: claimable count with only a codex_harness_v1 worker = %d, want 0", n)
		}
		e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1})
		if n := e.claimableForRun(t, runID); n != 1 {
			t.Fatalf("custom-root run: claimable count after adding a codex_custom_model_v1 worker = %d, want 1", n)
		}
		if !e.healthRowFor(t, runID).CodexCustomRoot {
			t.Fatal("a custom-lane codex issue run must be flagged codex_custom_root")
		}
	})

	t.Run("curated lane run is not custom-root and stays claimable by harness-only", func(t *testing.T) {
		curated := curatedCodexModel
		e.setUserCodexLane(t, &curated)
		e.seedHeartbeatWorker(t, []string{capability.CodexHarnessV1})
		runID := e.seedCodexIssueRunWithModel(t, nil)
		if e.healthRowFor(t, runID).CodexCustomRoot {
			t.Fatal("a curated-lane run must NOT be flagged codex_custom_root")
		}
		if n := e.claimableForRun(t, runID); n == 0 {
			t.Fatal("a curated-lane codex run must be claimable by a codex_harness_v1-only worker")
		}
	})

	t.Run("judge run is exempt (never custom-root, unaffected by the lane)", func(t *testing.T) {
		e.setUserCodexLane(t, &custom)
		judgeID := e.seedCodexJudgeRun(t)
		if e.healthRowFor(t, judgeID).CodexCustomRoot {
			t.Fatal("a codex judge run must NOT be flagged codex_custom_root (kind exempt)")
		}
	})
}

// TestQueuedReasonNoCustomCodexCapableWorkerLiveDB drives the real Service.queuedReason: a custom-root
// queued run whose owner has a codex_harness_v1 worker online but NO codex_custom_model_v1 worker
// surfaces reasonNoCustomCodexCapableWorker; one WITH a custom-capable worker falls through. The row's
// CodexCustomRoot is set explicitly to isolate the rung from priority-class re-labelling.
//
// CALIBRATION (queued-reason rung): delete the CodexCustomRoot rung in health.go — the first sub-case
// then falls through to the generic wait and this test fails.
func TestQueuedReasonNoCustomCodexCapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	customRootRow := func(runID uuid.UUID) store.ListActiveRunsForHealthRow {
		r := e.codexIndicatingQueuedRow(runID) // harness=codex, no caps/repo/contract
		r.CodexCustomRoot = true
		return r
	}

	t.Run("codex-harness worker but no custom-capable worker -> reasonNoCustomCodexCapableWorker", func(t *testing.T) {
		e.seedWorker(t, []string{capability.CodexHarnessV1}) // online, harness-only
		runID := e.seedCodexIssueRunWithModel(t, nil)
		got := svc.queuedReason(e.ctx, time.Now(), customRootRow(runID))
		if got != reasonNoCustomCodexCapableWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonNoCustomCodexCapableWorker)
		}
		if reasonNoCustomCodexCapableWorker != "no worker supporting custom Codex models is online" {
			t.Fatalf("reasonNoCustomCodexCapableWorker = %q, want the D6 prose", reasonNoCustomCodexCapableWorker)
		}
	})

	t.Run("custom-capable worker online -> falls through", func(t *testing.T) {
		e.seedWorker(t, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1})
		runID := e.seedCodexIssueRunWithModel(t, nil)
		got := svc.queuedReason(e.ctx, time.Now(), customRootRow(runID))
		if got == reasonNoCustomCodexCapableWorker {
			t.Fatalf("queuedReason = %q, must NOT be the custom-Codex reason when a custom-capable worker is online", got)
		}
	})
}
