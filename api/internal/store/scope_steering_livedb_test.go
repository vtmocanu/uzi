package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #634 (run scope steering) — the LIVE-DB half of M6's success criteria. Each test
// below is an assertion against real Postgres for a behaviour the in-memory fake store
// structurally cannot model: the data-modifying CTE in CreateScopeCeilingInput (MVCC
// snapshot supersede), the disposition CHECK/idempotency of SettleScopeInputDisposition,
// the scope_capped stop_kind narrowing CHECK plus the jsonb frozen-list immutability,
// the durable scope_ceiling column riding a requeue+re-claim, and the kind IN
// ('follow_up','scope') filter in ListFollowUpInputsForRun.
//
// These live in the store package DELIBERATELY: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... and
// ./internal/handler/... ONLY, so a *LiveDB test placed in workersvc would never gate.
// The functions under test are pure store queries, so the store package is both their
// correct home and the one the live-DB harness reaches.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 (a silent skip) is INVALID, not
// green — see .claude/rules/go.md.

// scopeSteeringDB is the common boilerplate: skip-guard, migrate, open a pool. It follows
// agent_source_staged_livedb_test.go verbatim.
func scopeSteeringDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, store.New(pool)
}

// scopeSeedRepo inserts a user + forge connection + repo and returns their ids. Fresh
// uuids per call so re-runs against a persistent DB never collide (Migrate does not
// truncate). Mirrors the seed shape in cancel_by_worker_livedb_test.go.
func scopeSeedRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("scope-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', true)`,
		repoID, connID, uuid.New().ID(), "g/scope-"+repoID.String(), "https://forge.e2e/g/scope")
	return userID, repoID
}

// scopeSeedWorker inserts a worker. When online, it carries a fresh heartbeat and a
// max_concurrent_runs so it can claim; otherwise last_heartbeat_at is NULL, which
// RequeueRunsOfStaleWorkers treats as stale. token_hash carries the worker UUID bytes so
// a re-run never collides on workers_token_hash_key.
func scopeSeedWorker(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, online bool) uuid.UUID {
	t.Helper()
	wkr := uuid.New()
	if online {
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs)
			 VALUES ($1, $2, 'w', $3, 'online', now(), 4)`, wkr, userID, wkr[:])
	} else {
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr, userID, wkr[:])
	}
	return wkr
}

// A milestone-structured issue run: 6 frozen milestones, 2 completed. The frozen/completed
// lists are jsonb arrays. status is caller-chosen so the completion test can seed 'running'
// (SetRunCompleted requires a non-terminal, worker-owned row) while the queue tests seed
// what they need.
const scopeFrozenJSON = `["m1", "m2", "m3", "m4", "m5", "m6"]`
const scopeCompletedJSON = `["m1", "m2"]`

func scopeSeedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, worker *uuid.UUID, status string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		                   milestones_frozen, milestones_completed)
		 VALUES ($1, $2, $3, 'issue', 42, 'ship the feature', 'ctx', $4, $5, $6::jsonb, $7::jsonb)`,
		runID, userID, repoID, status, worker, scopeFrozenJSON, scopeCompletedJSON)
	return runID
}

// scopeRow is one run_user_inputs scope audit row, read back for disposition assertions.
type scopeRow struct {
	body        pgtype.Text
	disposition pgtype.Text
}

// scopeRowsByAge returns all kind='scope' rows for a run oldest-first (id ASC), so
// [0] is the first-written directive and the last element is the newest.
func scopeRowsByAge(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) []scopeRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT body, disposition FROM run_user_inputs WHERE run_id = $1 AND kind = 'scope' ORDER BY id ASC`, runID)
	if err != nil {
		t.Fatalf("query scope rows: %v", err)
	}
	defer rows.Close()
	var out []scopeRow
	for rows.Next() {
		var r scopeRow
		if err := rows.Scan(&r.body, &r.disposition); err != nil {
			t.Fatalf("scan scope row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate scope rows: %v", err)
	}
	return out
}

func scopeCeiling(v int32) pgtype.Int4 { return pgtype.Int4{Int32: v, Valid: true} }
func scopeBody(s string) pgtype.Text   { return pgtype.Text{String: s, Valid: true} }

// Criterion 1: CreateScopeCeilingInput sets runs.scope_ceiling AND writes a pending
// kind='scope' audit row (disposition NULL) with the given body, in one statement.
func TestScopeCeilingRoundTripLiveDB(t *testing.T) {
	ctx, pool, q := scopeSteeringDB(t)
	userID, repoID := scopeSeedRepo(ctx, t, pool)
	wkr := scopeSeedWorker(ctx, t, pool, userID, true)
	runID := scopeSeedRun(ctx, t, pool, userID, repoID, &wkr, "running")

	body := "cap to first 4 milestones"
	inp, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID:        runID,
		Body:         scopeBody(body),
		ScopeCeiling: scopeCeiling(4),
	})
	if err != nil {
		t.Fatalf("CreateScopeCeilingInput: %v", err)
	}
	if inp.Kind != "scope" {
		t.Errorf("returned row kind = %q, want scope", inp.Kind)
	}
	if inp.Disposition.Valid {
		t.Errorf("a freshly created scope row must be pending (disposition NULL); got %q", inp.Disposition.String)
	}
	if inp.Body.String != body {
		t.Errorf("scope row body = %q, want %q", inp.Body.String, body)
	}

	// The column round-trips on a re-read of the run.
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if !run.ScopeCeiling.Valid || run.ScopeCeiling.Int32 != 4 {
		t.Fatalf("scope_ceiling = %+v, want valid 4", run.ScopeCeiling)
	}

	// Exactly one scope row, pending, carrying the body.
	got := scopeRowsByAge(ctx, t, pool, runID)
	if len(got) != 1 {
		t.Fatalf("scope rows = %d, want 1", len(got))
	}
	if got[0].disposition.Valid {
		t.Errorf("scope row disposition = %q, want NULL (pending)", got[0].disposition.String)
	}
	if got[0].body.String != body {
		t.Errorf("scope row body = %q, want %q", got[0].body.String, body)
	}
}

// Criterion 2: a second CreateScopeCeilingInput supersedes the first (the MVCC snapshot
// property — the superseded CTE sees only rows that existed at statement start, never the
// row the main INSERT adds), and the ceiling is last-writer-wins.
func TestScopeCeilingSupersedeFoldLiveDB(t *testing.T) {
	ctx, pool, q := scopeSteeringDB(t)
	userID, repoID := scopeSeedRepo(ctx, t, pool)
	wkr := scopeSeedWorker(ctx, t, pool, userID, true)
	runID := scopeSeedRun(ctx, t, pool, userID, repoID, &wkr, "running")

	if _, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, Body: scopeBody("first: cap to 4"), ScopeCeiling: scopeCeiling(4),
	}); err != nil {
		t.Fatalf("first CreateScopeCeilingInput: %v", err)
	}
	if _, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, Body: scopeBody("second: raise to 5"), ScopeCeiling: scopeCeiling(5),
	}); err != nil {
		t.Fatalf("second CreateScopeCeilingInput: %v", err)
	}

	got := scopeRowsByAge(ctx, t, pool, runID)
	if len(got) != 2 {
		t.Fatalf("scope rows = %d, want 2", len(got))
	}
	// Oldest row superseded; newest row still pending (NOT superseded — the snapshot
	// property). A regression that superseded the just-inserted row would flip this.
	if got[0].disposition.String != "superseded" {
		t.Errorf("first scope row disposition = %q (valid=%v), want superseded", got[0].disposition.String, got[0].disposition.Valid)
	}
	if got[1].disposition.Valid {
		t.Errorf("newest scope row disposition = %q, want NULL (pending, not superseded)", got[1].disposition.String)
	}

	// Last-writer-wins on the column.
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if !run.ScopeCeiling.Valid || run.ScopeCeiling.Int32 != 5 {
		t.Fatalf("scope_ceiling = %+v, want valid 5 (last-writer-wins)", run.ScopeCeiling)
	}
}

// Criterion 3: SettleScopeInputDisposition settles the ONE pending scope row and is
// idempotent (a second call moves 0 rows), never touching an already-superseded row.
func TestSettleScopeInputDispositionLiveDB(t *testing.T) {
	ctx, pool, q := scopeSteeringDB(t)
	userID, repoID := scopeSeedRepo(ctx, t, pool)
	wkr := scopeSeedWorker(ctx, t, pool, userID, true)
	runID := scopeSeedRun(ctx, t, pool, userID, repoID, &wkr, "running")

	// Two submits leave one superseded row and one pending row.
	if _, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, Body: scopeBody("first"), ScopeCeiling: scopeCeiling(4),
	}); err != nil {
		t.Fatalf("first CreateScopeCeilingInput: %v", err)
	}
	if _, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, Body: scopeBody("second"), ScopeCeiling: scopeCeiling(5),
	}); err != nil {
		t.Fatalf("second CreateScopeCeilingInput: %v", err)
	}

	// Settle the pending row → applied. Exactly one row (the NULL one) is affected.
	n, err := q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
		Disposition: scopeBody("applied"), RunID: runID,
	})
	if err != nil {
		t.Fatalf("SettleScopeInputDisposition: %v", err)
	}
	if n != 1 {
		t.Fatalf("first settle affected %d rows, want 1 (the one pending row)", n)
	}

	got := scopeRowsByAge(ctx, t, pool, runID)
	if len(got) != 2 {
		t.Fatalf("scope rows = %d, want 2", len(got))
	}
	if got[0].disposition.String != "superseded" {
		t.Errorf("superseded row was overwritten to %q; settle must leave non-pending rows untouched", got[0].disposition.String)
	}
	if got[1].disposition.String != "applied" {
		t.Errorf("pending row disposition = %q (valid=%v), want applied", got[1].disposition.String, got[1].disposition.Valid)
	}

	// Idempotent: a second settle finds no pending row → 0 rows, nothing changes.
	n, err = q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
		Disposition: scopeBody("declined"), RunID: runID,
	})
	if err != nil {
		t.Fatalf("second SettleScopeInputDisposition: %v", err)
	}
	if n != 0 {
		t.Fatalf("second settle affected %d rows, want 0 (idempotent)", n)
	}
	after := scopeRowsByAge(ctx, t, pool, runID)
	if after[0].disposition.String != "superseded" || after[1].disposition.String != "applied" {
		t.Errorf("a no-op settle changed dispositions: %+v", after)
	}
}

// Criterion 4: SetRunCompleted with StopKind='scope_capped' stamps the scope-capped
// completion, and the immutable milestones_frozen list is byte-identical afterward (a
// ceiling write / completion never rewrites the frozen list).
func TestScopeCappedCompletionFreezesLiveDB(t *testing.T) {
	ctx, pool, q := scopeSteeringDB(t)
	userID, repoID := scopeSeedRepo(ctx, t, pool)
	wkr := scopeSeedWorker(ctx, t, pool, userID, true)
	runID := scopeSeedRun(ctx, t, pool, userID, repoID, &wkr, "running")

	before, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID (before): %v", err)
	}
	frozenBefore := append([]byte(nil), before.MilestonesFrozen...)
	if len(frozenBefore) == 0 {
		t.Fatalf("seed produced an empty milestones_frozen; the immutability assertion would be vacuous")
	}

	rows, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		Branch:   scopeBody("agent/issue-42"),
		StopKind: scopeBody("scope_capped"),
		ID:       runID,
		WorkerID: pgUUID(wkr),
	})
	if err != nil {
		t.Fatalf("SetRunCompleted(scope_capped): %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunCompleted affected %d rows, want 1 (was the run non-terminal and worker-owned?)", rows)
	}

	after, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID (after): %v", err)
	}
	if after.Status != "completed" {
		t.Errorf("status = %q, want completed", after.Status)
	}
	if !after.StopKind.Valid || after.StopKind.String != "scope_capped" {
		t.Errorf("stop_kind = %+v, want scope_capped", after.StopKind)
	}
	if string(after.MilestonesFrozen) != string(frozenBefore) {
		t.Errorf("milestones_frozen mutated by a scope-capped completion:\n before=%s\n after =%s",
			string(frozenBefore), string(after.MilestonesFrozen))
	}
}

// Criterion 5: the durable scope_ceiling column survives a requeue and rides the next
// claim — a re-claiming worker reads the ceiling the operator set before the requeue.
func TestScopeCeilingSurvivesRequeueClaimLiveDB(t *testing.T) {
	ctx, pool, q := scopeSteeringDB(t)
	userID, repoID := scopeSeedRepo(ctx, t, pool)

	// A stale worker (no heartbeat) owns a running run — the shape RequeueRunsOfStaleWorkers
	// reclaims.
	staleWkr := scopeSeedWorker(ctx, t, pool, userID, false)
	runID := scopeSeedRun(ctx, t, pool, userID, repoID, &staleWkr, "running")

	// Operator sets the ceiling while the run is live.
	if _, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, Body: scopeBody("cap to 3"), ScopeCeiling: scopeCeiling(3),
	}); err != nil {
		t.Fatalf("CreateScopeCeilingInput: %v", err)
	}

	// Requeue: the stale worker's non-terminal run goes back to queued.
	requeued, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 5,
		Cutoff:      pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Second), Valid: true},
	})
	if err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	found := false
	for _, r := range requeued {
		if r.ID == runID {
			found = true
			if r.Status != "queued" {
				t.Errorf("requeued run status = %q, want queued", r.Status)
			}
		}
	}
	if !found {
		t.Fatalf("run %s was not requeued; the stale-worker predicate did not match (test setup wrong)", runID)
	}

	// A fresh online worker re-claims it.
	claimWkr := scopeSeedWorker(ctx, t, pool, userID, true)
	claimed, err := q.ClaimRun(ctx, store.ClaimRunParams{
		WorkerID:        pgUUID(claimWkr),
		UserID:          userID,
		AffinityCutoff:  pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
	})
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimed.ID != runID {
		t.Fatalf("ClaimRun returned run %s, want %s", claimed.ID, runID)
	}
	// The load-bearing assertion: the re-claimed run row still carries the ceiling.
	if !claimed.ScopeCeiling.Valid || claimed.ScopeCeiling.Int32 != 3 {
		t.Fatalf("re-claimed run scope_ceiling = %+v, want valid 3 (durable across requeue+claim)", claimed.ScopeCeiling)
	}
}

// Criterion 6: ListFollowUpInputsForRun returns BOTH follow_up and scope rows, and the
// scope row carries its disposition.
func TestListFollowUpInputsIncludesScopeLiveDB(t *testing.T) {
	ctx, pool, q := scopeSteeringDB(t)
	userID, repoID := scopeSeedRepo(ctx, t, pool)
	wkr := scopeSeedWorker(ctx, t, pool, userID, true)
	runID := scopeSeedRun(ctx, t, pool, userID, repoID, &wkr, "running")

	// A follow_up row (kind='follow_up') and a scope row (kind='scope', settled applied).
	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'please also add docs')`, runID)
	if _, err := q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID: runID, Body: scopeBody("cap to 4"), ScopeCeiling: scopeCeiling(4),
	}); err != nil {
		t.Fatalf("CreateScopeCeilingInput: %v", err)
	}
	if _, err := q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
		Disposition: scopeBody("applied"), RunID: runID,
	}); err != nil {
		t.Fatalf("SettleScopeInputDisposition: %v", err)
	}

	got, err := q.ListFollowUpInputsForRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListFollowUpInputsForRun: %v", err)
	}
	var sawFollowUp, sawScope bool
	var scopeDisposition pgtype.Text
	for _, in := range got {
		switch in.Kind {
		case "follow_up":
			sawFollowUp = true
		case "scope":
			sawScope = true
			scopeDisposition = in.Disposition
		}
	}
	if !sawFollowUp {
		t.Errorf("ListFollowUpInputsForRun omitted the follow_up row; got %d rows", len(got))
	}
	if !sawScope {
		t.Errorf("ListFollowUpInputsForRun omitted the scope row; got %d rows", len(got))
	}
	if !scopeDisposition.Valid || scopeDisposition.String != "applied" {
		t.Errorf("scope row disposition in list = %+v, want applied", scopeDisposition)
	}
}
