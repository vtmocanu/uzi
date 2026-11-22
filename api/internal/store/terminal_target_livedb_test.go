package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4616: live-DB proofs for the four terminal-MR-target queries
// (ListActiveMRTargetRuns, StampTerminalTargetStop, StampTerminalTargetCancelInput,
// CancelRunServerSideForTerminalTarget). A green sqlc generate does not prove these
// statements prepare or run; each guard is exercised here against a real Postgres.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// e2e/run-store-it.sh).

type terminalTargetEnv struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
	worker uuid.UUID
	target uuid.UUID // completed issue run, the mr_rework target_run_id
}

func newTerminalTargetEnv(t *testing.T) *terminalTargetEnv {
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
	e := &terminalTargetEnv{ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), worker: uuid.New(), target: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, e.userID, fmt.Sprintf("tt-%s@e2e", e.userID)); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	// Deleting the user cascades its forge_connections, repos, workers and runs, so the
	// fixtures do not leak into other store LiveDB tests sharing the database.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, e.userID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	e.repoID = e.newRepo(t)
	e.exec(t, `INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, e.worker, e.userID, e.worker[:])
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	           VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'completed')`, e.target, e.userID, e.repoID)
	return e
}

func (e *terminalTargetEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *terminalTargetEnv) newRepo(t *testing.T) uuid.UUID {
	t.Helper()
	connID, repoID := uuid.New(), uuid.New()
	e.exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', $3, 'bot', 1, $4)`, connID, e.userID, "https://forge.e2e/"+connID.String(), []byte{0x1})
	e.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/tt', 'main', true)`, repoID, connID, int64(repoID[0])+1, "g/tt-"+repoID.String())
	return repoID
}

type ttRun struct {
	kind       string // ci_fix | mr_rework
	status     string
	iid        int64 // target MR iid; 0 = unbound ci_fix
	generation int64
	worker     bool
	released   bool
	stopKind   string
	stopReason string
	failure    bool
	repo       uuid.UUID
	altMRIid   int64 // ci_fix only: also set the mr_iid column (iid sets ci_fix_mr_iid)
	dirty      bool  // seed health / pause / milestones state a terminal cancel must clear
}

func (e *terminalTargetEnv) seed(t *testing.T, r ttRun) uuid.UUID {
	t.Helper()
	id := uuid.New()
	repo := r.repo
	if repo == (uuid.UUID{}) {
		// A fresh repo per seeded run: uq_runs_one_active_mr_rework allows one active
		// mr_rework per (repo, mr_iid), and the guard-miss cases reuse the same iid.
		repo = e.newRepo(t)
	}
	var worker any
	if r.worker {
		worker = e.worker
	}
	var stopKind, stopReason any
	if r.stopKind != "" {
		stopKind = r.stopKind
	}
	if r.stopReason != "" {
		stopReason = r.stopReason
	}
	var failureReason, failOrigin any
	if r.failure {
		failureReason, failOrigin = "boom", "agent_failure"
	}
	ref := "agent/issue-" + id.String()
	switch r.kind {
	case "ci_fix":
		var iid any
		if r.iid != 0 {
			iid = r.iid
		}
		var mrIid any
		if r.altMRIid != 0 {
			mrIid = r.altMRIid
		}
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, pipeline_id, pipeline_ref, ci_fix_mr_iid, status, claim_generation, worker_id,
		                             stop_kind, stop_reason, failure_reason, fail_origin, issue_title, issue_description, mr_iid)
		           VALUES ($1, $2, $3, 'ci_fix', 99, $4, $5, $6, $7, $8, $9, $10, $11, $12, 't', 'd', $13)`,
			id, e.userID, repo, ref, iid, r.status, r.generation, worker, stopKind, stopReason, failureReason, failOrigin, mrIid)
	case "mr_rework":
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, pipeline_ref, mr_iid, target_run_id, status, claim_generation, worker_id,
		                             stop_kind, stop_reason, failure_reason, fail_origin, issue_title, issue_description)
		           VALUES ($1, $2, $3, 'mr_rework', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 't', 'd')`,
			id, e.userID, repo, ref, r.iid, e.target, r.status, r.generation, worker, stopKind, stopReason, failureReason, failOrigin)
	case "task":
		e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, branch, status, claim_generation, worker_id, failure_reason, fail_origin, issue_title, issue_description)
		           VALUES ($1, $2, $3, 'task', $4, $5, $6, $7, $8, $9, 't', 'd')`, id, e.userID, repo, ref, r.status, r.generation, worker, failureReason, failOrigin)
	default:
		t.Fatalf("unsupported kind %q", r.kind)
	}
	if r.released {
		e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
	}
	if r.dirty {
		e.exec(t, `UPDATE runs SET health = 'stalled', health_reason = 'no output', health_since = now(),
		           pause_requested_at = now(), pause_mode = 'now',
		           milestones_in_progress = '["m1"]'::jsonb WHERE id = $1`, id)
	}
	return id
}

func (e *terminalTargetEnv) run(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	run, err := e.q.GetRunByID(e.ctx, id)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	return run
}

func (e *terminalTargetEnv) inputCount(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count inputs: %v", err)
	}
	return n
}

// ttAssertFailurePreserved checks a guard miss left failure_reason / fail_origin intact
// (the seeded rows carry "boom" / agent_failure, which a hit would clear).
func ttAssertFailurePreserved(t *testing.T, before, after store.Run) {
	t.Helper()
	if !before.FailureReason.Valid || !before.FailOrigin.Valid {
		t.Fatalf("seed carries no failure_reason/fail_origin: %+v/%+v", before.FailureReason, before.FailOrigin)
	}
	if after.FailureReason != before.FailureReason || after.FailOrigin != before.FailOrigin {
		t.Errorf("failure_reason/fail_origin changed on a guard miss: before %+v/%+v after %+v/%+v",
			before.FailureReason, before.FailOrigin, after.FailureReason, after.FailOrigin)
	}
}

func ttPgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func TestListActiveMRTargetRunsLiveDB(t *testing.T) {
	e := newTerminalTargetEnv(t)
	otherRepo := e.newRepo(t)

	boundCI := e.seed(t, ttRun{repo: e.repoID, kind: "ci_fix", status: "running", iid: 11, worker: true, generation: 1})
	rework := e.seed(t, ttRun{repo: e.repoID, kind: "mr_rework", status: "queued", iid: 22})
	e.seed(t, ttRun{repo: e.repoID, kind: "ci_fix", status: "queued"})                // unbound ci_fix
	e.seed(t, ttRun{repo: e.repoID, kind: "ci_fix", status: "completed", iid: 33})    // terminal
	e.seed(t, ttRun{repo: e.repoID, kind: "mr_rework", status: "cancelled", iid: 44}) // terminal
	e.seed(t, ttRun{repo: e.repoID, kind: "task", status: "running", worker: true})   // other kind
	e.seed(t, ttRun{kind: "mr_rework", status: "queued", iid: 55, repo: otherRepo})   // other repo
	// ci_fix with mr_iid set but ci_fix_mr_iid NULL: the target is ci_fix_mr_iid only, so excluded.
	e.seed(t, ttRun{repo: e.repoID, kind: "ci_fix", status: "queued", altMRIid: 66})
	// ci_fix with both set to different values: the target is ci_fix_mr_iid (88), not mr_iid (77).
	bothSet := e.seed(t, ttRun{repo: e.repoID, kind: "ci_fix", status: "queued", iid: 88, altMRIid: 77})

	rows, err := e.q.ListActiveMRTargetRuns(e.ctx, store.ListActiveMRTargetRunsParams{RepoID: e.repoID, AfterID: uuid.Nil})
	if err != nil {
		t.Fatalf("ListActiveMRTargetRuns: %v", err)
	}
	got := map[uuid.UUID]store.ListActiveMRTargetRunsRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if len(rows) != 3 || got[boundCI].ID == uuid.Nil || got[rework].ID == uuid.Nil || got[bothSet].ID == uuid.Nil {
		t.Fatalf("rows = %+v, want exactly the bound ci_fix %s, the mr_rework %s and the both-set ci_fix %s", rows, boundCI, rework, bothSet)
	}
	if r := got[bothSet]; r.TargetMrIid != 88 {
		t.Errorf("both-set ci_fix target = %d, want 88 (ci_fix_mr_iid, not mr_iid 77)", r.TargetMrIid)
	}
	if r := got[boundCI]; r.Kind != "ci_fix" || r.Status != "running" || r.TargetMrIid != 11 || !r.WorkerID.Valid || r.WorkerID.Bytes != e.worker || r.ClaimGeneration != 1 || !r.PipelineRef.Valid {
		t.Errorf("ci_fix row = %+v, want kind ci_fix, running, target 11 (from ci_fix_mr_iid, mr_iid NULL), worker+generation 1", r)
	}
	if r := got[rework]; r.Kind != "mr_rework" || r.Status != "queued" || r.TargetMrIid != 22 || r.WorkerID.Valid {
		t.Errorf("mr_rework row = %+v, want kind mr_rework, queued, target 22 (from mr_iid), no worker", r)
	}
}

func TestListActiveMRTargetRunsKeysetLiveDB(t *testing.T) {
	e := newTerminalTargetEnv(t)
	var want []string
	for i := 0; i < 40; i++ {
		want = append(want, e.seed(t, ttRun{repo: e.repoID, kind: "mr_rework", status: "queued", iid: int64(1000 + i)}).String())
	}
	sort.Strings(want)

	first, err := e.q.ListActiveMRTargetRuns(e.ctx, store.ListActiveMRTargetRunsParams{RepoID: e.repoID, AfterID: uuid.Nil})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 33 {
		t.Fatalf("first page has %d rows, want the LIMIT of 33", len(first))
	}
	for i, r := range first {
		if r.ID.String() != want[i] {
			t.Fatalf("first page row %d = %s, want %s (ORDER BY id)", i, r.ID, want[i])
		}
	}
	// Keyset: continue after the 32nd row (the caller keeps 32 and drops the probe row).
	second, err := e.q.ListActiveMRTargetRuns(e.ctx, store.ListActiveMRTargetRunsParams{RepoID: e.repoID, AfterID: first[31].ID})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 8 {
		t.Fatalf("second page has %d rows, want 8", len(second))
	}
	for i, r := range second {
		if r.ID.String() != want[32+i] {
			t.Fatalf("second page row %d = %s, want %s", i, r.ID, want[32+i])
		}
	}
}

func TestStampTerminalTargetStopLiveDB(t *testing.T) {
	e := newTerminalTargetEnv(t)
	const reason = "target MR merged"
	call := func(id uuid.UUID, worker uuid.UUID, gen int64) int64 {
		t.Helper()
		n, err := e.q.StampTerminalTargetStop(e.ctx, store.StampTerminalTargetStopParams{
			StopReason: reason, ID: id, WorkerID: ttPgUUID(worker), ClaimGeneration: gen})
		if err != nil {
			t.Fatalf("StampTerminalTargetStop: %v", err)
		}
		return n
	}

	for _, status := range []string{"claimed", "running"} {
		for _, kind := range []string{"ci_fix", "mr_rework"} {
			id := e.seed(t, ttRun{kind: kind, status: status, iid: 7, worker: true, generation: 3, failure: true})
			if n := call(id, e.worker, 3); n != 1 {
				t.Fatalf("%s/%s: rows = %d, want 1", kind, status, n)
			}
			run := e.run(t, id)
			if run.Status != status {
				t.Errorf("%s/%s: status = %q, the stamp must not change status", kind, status, run.Status)
			}
			if !run.StopKind.Valid || run.StopKind.String != "cancelled" || !run.StopReason.Valid || run.StopReason.String != reason {
				t.Errorf("%s/%s: stop_kind/stop_reason = %+v/%+v, want cancelled/%q", kind, status, run.StopKind, run.StopReason, reason)
			}
			if run.FailureReason.Valid || run.FailOrigin.Valid {
				t.Errorf("%s/%s: failure_reason/fail_origin = %+v/%+v, want both cleared", kind, status, run.FailureReason, run.FailOrigin)
			}
			if c := e.inputCount(t, id); c != 0 {
				t.Errorf("%s/%s: %d run_user_inputs rows written, want none", kind, status, c)
			}
		}
	}

	misses := []struct {
		name string
		run  ttRun
		wkr  uuid.UUID
		gen  int64
	}{
		{"wrong worker", ttRun{kind: "ci_fix", status: "running", iid: 7, worker: true, generation: 3}, uuid.New(), 3},
		{"wrong generation", ttRun{kind: "ci_fix", status: "running", iid: 7, worker: true, generation: 3}, e.worker, 2},
		{"released claim", ttRun{kind: "ci_fix", status: "running", iid: 7, worker: true, generation: 3, released: true}, e.worker, 3},
		{"terminal status", ttRun{kind: "mr_rework", status: "completed", iid: 7, worker: true, generation: 3}, e.worker, 3},
		{"queued status", ttRun{kind: "mr_rework", status: "queued", iid: 7, worker: true, generation: 3}, e.worker, 3},
		{"awaiting_approval status", ttRun{kind: "mr_rework", status: "awaiting_approval", iid: 7, worker: true, generation: 3}, e.worker, 3},
		{"existing stop verdict", ttRun{kind: "ci_fix", status: "running", iid: 7, worker: true, generation: 3, stopKind: "stopped", stopReason: "operator"}, e.worker, 3},
		{"existing cancel verdict", ttRun{kind: "mr_rework", status: "running", iid: 7, worker: true, generation: 3, stopKind: "cancelled", stopReason: "operator said so"}, e.worker, 3},
		{"wrong kind", ttRun{kind: "task", status: "running", worker: true, generation: 3}, e.worker, 3},
	}
	for _, m := range misses {
		t.Run(m.name, func(t *testing.T) {
			m.run.failure = true
			id := e.seed(t, m.run)
			before := e.run(t, id)
			if n := call(id, m.wkr, m.gen); n != 0 {
				t.Fatalf("rows = %d, want 0", n)
			}
			after := e.run(t, id)
			if after.StopKind != before.StopKind || after.StopReason != before.StopReason || after.Status != before.Status {
				t.Errorf("row changed on a guard miss: before %+v/%+v after %+v/%+v", before.StopKind, before.StopReason, after.StopKind, after.StopReason)
			}
			ttAssertFailurePreserved(t, before, after)
			if c := e.inputCount(t, id); c != 0 {
				t.Errorf("%d run_user_inputs rows written, want none", c)
			}
		})
	}
}

func TestStampTerminalTargetCancelInputLiveDB(t *testing.T) {
	e := newTerminalTargetEnv(t)
	const reason = "target MR closed"
	call := func(id uuid.UUID, worker uuid.UUID, gen int64) (store.RunUserInput, error) {
		return e.q.StampTerminalTargetCancelInput(e.ctx, store.StampTerminalTargetCancelInputParams{
			StopReason: reason, ID: id, WorkerID: ttPgUUID(worker), ClaimGeneration: gen})
	}

	id := e.seed(t, ttRun{kind: "ci_fix", status: "running", iid: 9, worker: true, generation: 4, failure: true})
	in, err := call(id, e.worker, 4)
	if err != nil {
		t.Fatalf("StampTerminalTargetCancelInput: %v", err)
	}
	if in.RunID != id || in.Kind != "cancel" || !in.Body.Valid || in.Body.String != reason {
		t.Fatalf("input = %+v, want a cancel for %s with body %q", in, id, reason)
	}
	run := e.run(t, id)
	if !run.StopKind.Valid || run.StopKind.String != "cancelled" || !run.StopReason.Valid || run.StopReason.String != reason {
		t.Errorf("stop_kind/stop_reason = %+v/%+v, want cancelled/%q", run.StopKind, run.StopReason, reason)
	}
	if run.FailureReason.Valid || run.FailOrigin.Valid {
		t.Errorf("failure_reason/fail_origin = %+v/%+v, want both cleared", run.FailureReason, run.FailOrigin)
	}
	// Second call: stop_kind is now set, so it is a no-op and writes no duplicate input.
	if _, err := call(id, e.worker, 4); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second call err = %v, want pgx.ErrNoRows", err)
	}
	if c := e.inputCount(t, id); c != 1 {
		t.Fatalf("%d inputs after a second call, want exactly 1", c)
	}
	// The input is drained by the live worker's ConsumeRunInputs.
	consumed, err := e.q.ConsumeRunInputs(e.ctx, id)
	if err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	if len(consumed) != 1 || consumed[0].Kind != "cancel" || consumed[0].Body.String != reason {
		t.Fatalf("consumed = %+v, want the single cancel input", consumed)
	}

	// Positive cases beyond ci_fix/running: mr_rework and every other live status.
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, status := range []string{"claimed", "running", "awaiting_approval", "awaiting_input"} {
			if kind == "ci_fix" && status == "running" {
				continue // covered above
			}
			pid := e.seed(t, ttRun{kind: kind, status: status, iid: 9, worker: true, generation: 4, failure: true})
			pin, perr := call(pid, e.worker, 4)
			if perr != nil {
				t.Errorf("%s/%s: StampTerminalTargetCancelInput: %v", kind, status, perr)
				continue
			}
			if pin.RunID != pid || pin.Kind != "cancel" || !pin.Body.Valid || pin.Body.String != reason {
				t.Errorf("%s/%s: input = %+v, want a cancel with body %q", kind, status, pin, reason)
			}
			prun := e.run(t, pid)
			if prun.Status != status {
				t.Errorf("%s/%s: status = %q, the stamp must not change status", kind, status, prun.Status)
			}
			if !prun.StopKind.Valid || prun.StopKind.String != "cancelled" || !prun.StopReason.Valid || prun.StopReason.String != reason {
				t.Errorf("%s/%s: stop_kind/stop_reason = %+v/%+v, want cancelled/%q", kind, status, prun.StopKind, prun.StopReason, reason)
			}
			if prun.FailureReason.Valid || prun.FailOrigin.Valid {
				t.Errorf("%s/%s: failure_reason/fail_origin = %+v/%+v, want both cleared", kind, status, prun.FailureReason, prun.FailOrigin)
			}
			if c := e.inputCount(t, pid); c != 1 {
				t.Errorf("%s/%s: %d inputs, want exactly 1", kind, status, c)
			}
		}
	}

	misses := []struct {
		name string
		run  ttRun
		wkr  uuid.UUID
		gen  int64
	}{
		{"wrong worker", ttRun{kind: "mr_rework", status: "running", iid: 7, worker: true, generation: 4}, uuid.New(), 4},
		{"wrong generation", ttRun{kind: "mr_rework", status: "running", iid: 7, worker: true, generation: 4}, e.worker, 5},
		{"released claim", ttRun{kind: "mr_rework", status: "running", iid: 7, worker: true, generation: 4, released: true}, e.worker, 4},
		{"terminal status", ttRun{kind: "ci_fix", status: "failed", iid: 7, worker: true, generation: 4}, e.worker, 4},
		{"queued status", ttRun{kind: "ci_fix", status: "queued", iid: 7, worker: true, generation: 4}, e.worker, 4},
		{"limit_wait status", ttRun{kind: "ci_fix", status: "limit_wait", iid: 7, worker: true, generation: 4}, e.worker, 4},
		{"pool_wait status", ttRun{kind: "ci_fix", status: "pool_wait", iid: 7, worker: true, generation: 4}, e.worker, 4},
		{"recovery_wait status", ttRun{kind: "ci_fix", status: "recovery_wait", iid: 7, worker: true, generation: 4}, e.worker, 4},
		{"paused status", ttRun{kind: "ci_fix", status: "paused", iid: 7, worker: true, generation: 4}, e.worker, 4},
		{"existing stop verdict", ttRun{kind: "ci_fix", status: "running", iid: 7, worker: true, generation: 4, stopKind: "stopped", stopReason: "operator"}, e.worker, 4},
		{"wrong kind", ttRun{kind: "task", status: "running", worker: true, generation: 4}, e.worker, 4},
	}
	for _, m := range misses {
		t.Run(m.name, func(t *testing.T) {
			m.run.failure = true
			mid := e.seed(t, m.run)
			before := e.run(t, mid)
			if _, err := call(mid, m.wkr, m.gen); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("err = %v, want pgx.ErrNoRows", err)
			}
			after := e.run(t, mid)
			if after.StopKind != before.StopKind || after.StopReason != before.StopReason {
				t.Errorf("stamp changed on a guard miss: before %+v/%+v after %+v/%+v", before.StopKind, before.StopReason, after.StopKind, after.StopReason)
			}
			ttAssertFailurePreserved(t, before, after)
			if c := e.inputCount(t, mid); c != 0 {
				t.Errorf("%d run_user_inputs rows written on a guard miss, want none", c)
			}
		})
	}
}

func TestCancelRunServerSideForTerminalTargetLiveDB(t *testing.T) {
	e := newTerminalTargetEnv(t)
	const reason = "target MR merged"
	call := func(id uuid.UUID, status string, gen, iid int64) int64 {
		t.Helper()
		n, err := e.q.CancelRunServerSideForTerminalTarget(e.ctx, store.CancelRunServerSideForTerminalTargetParams{
			StopReason: reason, ID: id, ObservedStatus: status, ObservedGeneration: gen, MrIid: iid})
		if err != nil {
			t.Fatalf("CancelRunServerSideForTerminalTarget: %v", err)
		}
		return n
	}

	for _, kind := range []string{"ci_fix", "mr_rework"} {
		id := e.seed(t, ttRun{kind: kind, status: "queued", iid: 12, generation: 0, failure: true})
		if n := call(id, "queued", 0, 12); n != 1 {
			t.Fatalf("%s: rows = %d, want 1", kind, n)
		}
		run := e.run(t, id)
		if run.Status != "cancelled" || !run.StopKind.Valid || run.StopKind.String != "cancelled" || !run.StopReason.Valid || run.StopReason.String != reason {
			t.Errorf("%s: status/stop_kind/stop_reason = %q/%+v/%+v, want cancelled/cancelled/%q", kind, run.Status, run.StopKind, run.StopReason, reason)
		}
		if run.FailureReason.Valid || run.FailOrigin.Valid {
			t.Errorf("%s: failure_reason/fail_origin = %+v/%+v, want both NULL", kind, run.FailureReason, run.FailOrigin)
		}
		if !run.FinishedAt.Valid {
			t.Errorf("%s: finished_at is NULL after the cancel", kind)
		}
		if n := call(id, "queued", 0, 12); n != 0 {
			t.Errorf("%s: second call rows = %d, want 0 (terminal)", kind, n)
		}
	}

	// Stale-worker shape: a non-live claimed/running run at generation > 0 whose worker
	// stopped answering; the server-side cancel clears the dirty state a live run carries.
	for _, status := range []string{"claimed", "running"} {
		id := e.seed(t, ttRun{kind: "ci_fix", status: status, iid: 12, worker: true, generation: 3, failure: true, dirty: true})
		before := e.run(t, id)
		if before.Health != "stalled" || !before.HealthReason.Valid || !before.HealthSince.Valid ||
			!before.PauseRequestedAt.Valid || !before.PauseMode.Valid || len(before.MilestonesInProgress) == 0 {
			t.Fatalf("%s: seed is not dirty: %+v", status, before)
		}
		if n := call(id, status, 3, 12); n != 1 {
			t.Fatalf("stale %s: rows = %d, want 1", status, n)
		}
		run := e.run(t, id)
		if run.Status != "cancelled" || !run.StopKind.Valid || run.StopKind.String != "cancelled" || !run.StopReason.Valid || run.StopReason.String != reason {
			t.Errorf("stale %s: status/stop_kind/stop_reason = %q/%+v/%+v, want cancelled/cancelled/%q", status, run.Status, run.StopKind, run.StopReason, reason)
		}
		if run.Health != "ok" || run.HealthReason.Valid || run.HealthSince.Valid {
			t.Errorf("stale %s: health/reason/since = %q/%+v/%+v, want ok/NULL/NULL", status, run.Health, run.HealthReason, run.HealthSince)
		}
		if run.PauseRequestedAt.Valid || run.PauseMode.Valid {
			t.Errorf("stale %s: pause_requested_at/pause_mode = %+v/%+v, want NULL", status, run.PauseRequestedAt, run.PauseMode)
		}
		if run.MilestonesInProgress != nil {
			t.Errorf("stale %s: milestones_in_progress = %q, want NULL", status, run.MilestonesInProgress)
		}
		if run.FailureReason.Valid || run.FailOrigin.Valid || !run.FinishedAt.Valid {
			t.Errorf("stale %s: failure_reason/fail_origin/finished_at = %+v/%+v/%+v, want NULL/NULL/set", status, run.FailureReason, run.FailOrigin, run.FinishedAt)
		}
	}

	misses := []struct {
		name   string
		run    ttRun
		status string
		gen    int64
		iid    int64
	}{
		{"status mismatch", ttRun{kind: "ci_fix", status: "queued", iid: 12}, "running", 0, 12},
		{"generation mismatch", ttRun{kind: "ci_fix", status: "queued", iid: 12, generation: 2}, "queued", 0, 12},
		{"existing stop_kind", ttRun{kind: "mr_rework", status: "queued", iid: 12, stopKind: "cancelled", stopReason: "operator"}, "queued", 0, 12},
		{"wrong ci_fix iid", ttRun{kind: "ci_fix", status: "queued", iid: 12}, "queued", 0, 13},
		{"wrong mr_rework iid", ttRun{kind: "mr_rework", status: "queued", iid: 12}, "queued", 0, 13},
		{"unbound ci_fix", ttRun{kind: "ci_fix", status: "queued"}, "queued", 0, 12},
		{"terminal run", ttRun{kind: "mr_rework", status: "completed", iid: 12}, "completed", 0, 12},
		{"wrong kind", ttRun{kind: "task", status: "queued"}, "queued", 0, 12},
	}
	for _, m := range misses {
		t.Run(m.name, func(t *testing.T) {
			m.run.failure = true
			id := e.seed(t, m.run)
			before := e.run(t, id)
			if n := call(id, m.status, m.gen, m.iid); n != 0 {
				t.Fatalf("rows = %d, want 0", n)
			}
			after := e.run(t, id)
			ttAssertFailurePreserved(t, before, after)
			if after.Status != before.Status || after.StopKind != before.StopKind || after.StopReason != before.StopReason {
				t.Errorf("row changed on a guard miss: before %q/%+v/%+v after %q/%+v/%+v",
					before.Status, before.StopKind, before.StopReason, after.Status, after.StopKind, after.StopReason)
			}
		})
	}
}

// sqlCapture is a pgx query tracer that records the statement and arguments the generated
// ListActiveMRTargetRuns sends, so the plan test explains the exact production text.
type sqlCapture struct {
	sql  string
	args []any
}

func (c *sqlCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.sql, c.args = d.SQL, d.Args
	return ctx
}

func (c *sqlCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// The enumeration must have a repo_id-scoped index path on runs. The planner's own choice is
// accepted when it already uses one. A few-page test table can legitimately seq-scan, and a
// production-sized fixture is impractical here (row triggers make bulk-loading runs take
// minutes), so the test then re-plans with enable_seqscan=off and requires the repo_id Index
// Cond. This proves the shipped statement can be served repo-scoped by an index. It does not
// prove the production planner's choice on a large table, which stays a review/EXPLAIN check.
func TestListActiveMRTargetRunsPlanUsesIndexLiveDB(t *testing.T) {
	e := newTerminalTargetEnv(t)
	for i := 0; i < 60; i++ {
		e.seed(t, ttRun{repo: e.repoID, kind: "mr_rework", status: "queued", iid: int64(2000 + i)})
	}
	// Rows in other repos make a repo-scoped index path the selective one: the assertion below
	// is that the plan narrows by repo_id through an index, not merely that some index exists
	// (runs_pkey alone would satisfy ORDER BY id under enable_seqscan=off).
	for i := 0; i < 200; i++ {
		e.seed(t, ttRun{kind: "mr_rework", status: "queued", iid: int64(5000 + i)})
	}
	e.exec(t, `ANALYZE runs`)

	cfg, err := pgxpool.ParseConfig(os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	capt := &sqlCapture{}
	cfg.ConnConfig.Tracer = capt
	cpool, err := pgxpool.NewWithConfig(e.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cpool.Close)
	params := store.ListActiveMRTargetRunsParams{RepoID: e.repoID, AfterID: uuid.Nil}
	if _, err := store.New(cpool).ListActiveMRTargetRuns(e.ctx, params); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capt.sql, "ListActiveMRTargetRuns") || len(capt.args) != 2 {
		t.Fatalf("captured statement %q args %v", capt.sql, capt.args)
	}

	explain := func(seqscanOff bool) string {
		t.Helper()
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(e.ctx) }()
		if seqscanOff {
			if _, err := tx.Exec(e.ctx, `SET LOCAL enable_seqscan = off`); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := tx.Query(e.ctx, "EXPLAIN "+capt.sql, capt.args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return plan.String()
	}
	// repoIndexed reports whether some index node's Index Cond constrains repo_id.
	repoIndexed := func(plan string) bool {
		for _, line := range strings.Split(plan, "\n") {
			if strings.Contains(line, "Index Cond:") && strings.Contains(line, "repo_id") {
				return true
			}
		}
		return false
	}
	own := explain(false)
	t.Logf("planner's own choice on this table:\n%s", own)
	if repoIndexed(own) && !strings.Contains(own, "Seq Scan on runs") {
		return
	}
	plan := explain(true)
	t.Logf("with enable_seqscan=off (a repo-scoped index path is possible check):\n%s", plan)
	if !repoIndexed(plan) || strings.Contains(plan, "Seq Scan on runs") {
		t.Fatalf("no repo_id-scoped index path for ListActiveMRTargetRuns:\n%s", plan)
	}
}
