package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// decision_memo_livedb_test.go covers the run decisions memo store (issue #2083 M1): the fenced
// single-statement write (UpsertRunDecisionMemoFenced) and the lineage resolver
// (GetLatestDecisionMemoForLineage) against a real throwaway Postgres. Skipped unless
// UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh). They reuse interlockLiveDB.

// memoRun describes one seeded run. Zero values mean: issue kind, running, generation 1, held by
// the fixture worker, the fixture owner and repo.
type memoRun struct {
	kind        string
	status      string
	worker      uuid.UUID // uuid.Nil: unheld
	gen         int64
	released    bool
	branch      string
	pipelineRef string
	mrIID       int64
	finishedAt  time.Time
	userID      uuid.UUID
	repoID      uuid.UUID
	targetRunID uuid.UUID
}

func (e interlockLiveDB) seedMemoRun(t *testing.T, worker uuid.UUID, m memoRun) uuid.UUID {
	t.Helper()
	if m.kind == "" {
		m.kind = "issue"
	}
	if m.status == "" {
		m.status = "running"
	}
	if m.gen == 0 {
		m.gen = 1
	}
	if m.userID == uuid.Nil {
		m.userID = e.userID
	}
	if m.repoID == uuid.Nil {
		m.repoID = e.repoID
	}
	if m.worker == uuid.Nil && m.status == "running" {
		m.worker = worker
	}
	id := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	var (
		issueIID    any
		pipelineID  any
		pipelineRef any
		mrIID       any
		branch      any
		workerArg   any
		releasedAt  any
		finishedAt  any
		targetRun   any
	)
	switch m.kind {
	case "issue", "self_improve":
		issueIID = iid
	case "ci_fix":
		pipelineID = iid
		pipelineRef = "agent/issue-ci"
	case "mr_rework":
		pipelineRef = m.pipelineRef
		target := m.targetRunID
		if target == uuid.Nil {
			target = id
		}
		targetRun = target
	}
	if m.mrIID != 0 {
		mrIID = m.mrIID
	}
	if m.branch != "" {
		branch = m.branch
	}
	if m.worker != uuid.Nil {
		workerArg = m.worker
	}
	if m.released {
		releasedAt = time.Now()
	}
	if !m.finishedAt.IsZero() {
		finishedAt = m.finishedAt
	}
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, pipeline_id, pipeline_ref, mr_iid, branch, target_run_id,
	               issue_title, issue_description, status, worker_id, claim_generation, claim_released_at, finished_at)
	           VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 't', 'd', $11, $12, $13, $14, $15)`,
		id, m.userID, m.repoID, m.kind, issueIID, pipelineID, pipelineRef, mrIID, branch, targetRun,
		m.status, workerArg, m.gen, releasedAt, finishedAt)
	return id
}

// seedMemoRepo creates a second repo owned by the fixture user, for the wrong-repo lineage case.
func (e interlockLiveDB) seedMemoRepo(t *testing.T) uuid.UUID {
	t.Helper()
	var connID uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT connection_id FROM repos WHERE id = $1`, e.repoID).Scan(&connID); err != nil {
		t.Fatalf("read connection: %v", err)
	}
	id := uuid.New()
	e.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	           VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/other', 'main', true)`,
		id, connID, time.Now().UnixNano(), "g/other-"+id.String()[:8])
	return id
}

type memoRow struct {
	exists  bool
	body    string
	gen     int64
	format  int16
	updated time.Time
}

func (e interlockLiveDB) memoRow(t *testing.T, runID uuid.UUID) memoRow {
	t.Helper()
	var r memoRow
	rows, err := e.pool.Query(e.ctx, `SELECT body, claim_generation, format_version, updated_at FROM run_decision_memos WHERE run_id = $1`, runID)
	if err != nil {
		t.Fatalf("read memo: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		r.exists = true
		if err := rows.Scan(&r.body, &r.gen, &r.format, &r.updated); err != nil {
			t.Fatalf("scan memo: %v", err)
		}
	}
	return r
}

// putMemo inserts a memo row directly (bypassing the fenced write) for resolver fixtures.
func (e interlockLiveDB) putMemo(t *testing.T, runID uuid.UUID, gen int64, format int, body string, updated time.Time) {
	t.Helper()
	e.exec(t, `INSERT INTO run_decision_memos (run_id, claim_generation, format_version, body, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		runID, gen, format, body, updated)
}

func memoWorker(e interlockLiveDB, id uuid.UUID) store.Worker {
	return store.Worker{ID: id, UserID: e.userID}
}

func (e interlockLiveDB) setGen(t *testing.T, runID uuid.UUID, gen int64) {
	t.Helper()
	e.exec(t, `UPDATE runs SET claim_generation = $2 WHERE id = $1`, runID, gen)
}

// TestSaveDecisionsMemoStaleGenerationAfterReclaimLiveDB: a delayed write from generation 1 that
// arrives after the same worker re-claimed the run (generation 2) is refused and leaves the stored
// memo untouched; the generation-2 write then restamps the row.
func TestSaveDecisionsMemoStaleGenerationAfterReclaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	run := e.seedMemoRun(t, w, memoRun{})

	if n, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, "gen1 body"); err != nil || n != len("gen1 body") {
		t.Fatalf("gen1 save = %d, %v; want %d, nil", n, err, len("gen1 body"))
	}
	before := e.memoRow(t, run)
	// The reclaim: same worker, generation bumped (mirrors ClaimRun's claim_generation + 1).
	e.setGen(t, run, 2)

	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, "late gen1 body"); !errors.Is(err, ErrDecisionsMemoClaimNotCurrent) {
		t.Fatalf("stale-generation save err = %v, want ErrDecisionsMemoClaimNotCurrent", err)
	}
	if after := e.memoRow(t, run); after != before {
		t.Fatalf("a refused write mutated the row: before %+v after %+v", before, after)
	}

	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 2, "gen2 body"); err != nil {
		t.Fatalf("gen2 save: %v", err)
	}
	if got := e.memoRow(t, run); got.body != "gen2 body" || got.gen != 2 {
		t.Fatalf("after the gen2 save the row = %+v, want body gen2 body stamped generation 2", got)
	}
}

// TestSaveDecisionsMemoReleasedClaimLiveDB: a write on a claim whose claim_released_at is set is
// refused even at the matching generation, and an existing row is unchanged.
func TestSaveDecisionsMemoReleasedClaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	run := e.seedMemoRun(t, w, memoRun{})
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, "kept"); err != nil {
		t.Fatalf("save: %v", err)
	}
	before := e.memoRow(t, run)
	e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)

	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, "after release"); !errors.Is(err, ErrDecisionsMemoClaimNotCurrent) {
		t.Fatalf("released-claim save err = %v, want ErrDecisionsMemoClaimNotCurrent", err)
	}
	if after := e.memoRow(t, run); after != before {
		t.Fatalf("a refused write mutated the row: before %+v after %+v", before, after)
	}
}

// TestSaveDecisionsMemoRacingCompletionLiveDB: the write races the run's completion. Connection A
// completes the run with the real SetRunCompleted inside an open transaction (holding the run row
// lock); the save runs on another connection and blocks on that lock. After A commits the save must
// observe the terminal row, write nothing and report claim_not_current, leaving the pre-existing
// memo unchanged. SetRunCompleted must also leave claim_generation alone: the resolver's
// generation-compatibility test depends on it.
func TestSaveDecisionsMemoRacingCompletionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	run := e.seedMemoRun(t, w, memoRun{gen: 3})
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 3, "before completion"); err != nil {
		t.Fatalf("save: %v", err)
	}
	before := e.memoRow(t, run)

	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	var txPID int32
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&txPID); err != nil {
		t.Fatalf("read tx backend pid: %v", err)
	}
	rows, err := e.q.WithTx(tx).SetRunCompleted(e.ctx, store.SetRunCompletedParams{
		ID: run, WorkerID: pgconv.UUID(w),
		Branch: pgtype.Text{String: "agent/issue-race", Valid: true},
	})
	if err != nil || rows != 1 {
		t.Fatalf("SetRunCompleted = %d, %v; want 1, nil", rows, err)
	}

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := svc.SaveDecisionsMemo(context.Background(), wkr, run, 3, "racing write")
		done <- result{n, err}
	}()
	// Prove the save is blocked on the completion's row lock (not merely slow to start): poll until
	// some backend is blocked by the completing transaction's pid.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, txPID).Scan(&blocked); err != nil {
			t.Fatalf("read blocked backends: %v", err)
		}
		if blocked >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the save never blocked on the completion's row lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("the save returned %+v while the completion held the row lock; it must block on it", r)
	default:
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit completion: %v", err)
	}
	select {
	case r := <-done:
		if !errors.Is(r.err, ErrDecisionsMemoClaimNotCurrent) {
			t.Fatalf("racing save = %+v, want ErrDecisionsMemoClaimNotCurrent", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the save never returned after the completion committed")
	}
	if after := e.memoRow(t, run); after != before {
		t.Fatalf("the racing write mutated the row: before %+v after %+v", before, after)
	}
	var gen int64
	if err := e.pool.QueryRow(e.ctx, `SELECT claim_generation FROM runs WHERE id = $1`, run).Scan(&gen); err != nil || gen != 3 {
		t.Fatalf("claim_generation after SetRunCompleted = %d, %v; want unchanged 3", gen, err)
	}
}

// TestSaveDecisionsMemoRefusedRunsLiveDB: a non-running run, a kind that carries no memo (ci_fix,
// and chat, which is also repo-less) and another worker's run are all refused, and never create a
// row. The statement's repo_id IS NOT NULL predicate is defence in depth: the runs kind-shape CHECK
// already requires repo_id for the four memo kinds, so no repo-less run of those kinds can exist to
// exercise it, and the chat fixture below is refused by the kind predicate.
func TestSaveDecisionsMemoRefusedRunsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	other := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)

	for _, tc := range []struct {
		name string
		run  uuid.UUID
		as   store.Worker
		want error
	}{
		{"ci_fix kind", e.seedMemoRun(t, w, memoRun{kind: "ci_fix"}), wkr, ErrDecisionsMemoClaimNotCurrent},
		{"awaiting_approval", e.seedMemoRun(t, w, memoRun{status: "awaiting_approval", worker: w}), wkr, ErrDecisionsMemoClaimNotCurrent},
		{"completed", e.seedMemoRun(t, w, memoRun{status: "completed", worker: w}), wkr, ErrDecisionsMemoClaimNotCurrent},
		{"another worker's run", e.seedMemoRun(t, other, memoRun{}), wkr, ErrRunNotOwned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SaveDecisionsMemo(e.ctx, tc.as, tc.run, 1, "body"); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if e.memoRow(t, tc.run).exists {
				t.Fatal("a refused write created a row")
			}
		})
	}

	// A chat run: a kind that carries no memo (it also has no repo, but the kind predicate refuses it).
	chat := uuid.New()
	e.exec(t, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id, claim_generation)
	           VALUES ($1, $2, 'chat', 't', 'd', 'running', $3, 1)`, chat, e.userID, w)
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, chat, 1, "body"); !errors.Is(err, ErrDecisionsMemoClaimNotCurrent) {
		t.Fatalf("repo-less chat run err = %v, want ErrDecisionsMemoClaimNotCurrent", err)
	}
	if e.memoRow(t, chat).exists {
		t.Fatal("a refused write created a row for the chat run")
	}
}

// TestSaveDecisionsMemoSanitizesAndBoundsLiveDB: control characters are stripped before the cap, a
// sanitized body of exactly 8192 bytes is stored, one more byte is refused, and a later save
// replaces the body in place (one row per run).
func TestSaveDecisionsMemoSanitizesAndBoundsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	run := e.seedMemoRun(t, w, memoRun{})

	dirty := "keep\x1b[31m this\x00 line\nnext\tcol"
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, dirty); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := e.memoRow(t, run).body; strings.ContainsAny(got, "\x1b\x00") || !strings.Contains(got, "\n") || !strings.Contains(got, "\t") {
		t.Fatalf("stored body %q: control characters must be stripped, newline and tab kept", got)
	}

	// 8192 clean bytes plus 100 control bytes sanitizes to the cap exactly and is accepted.
	atCap := strings.Repeat("a", MaxDecisionsMemoBytes) + strings.Repeat("\x01", 100)
	if n, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, atCap); err != nil || n != MaxDecisionsMemoBytes {
		t.Fatalf("cap-sized save = %d, %v; want %d, nil", n, err, MaxDecisionsMemoBytes)
	}
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, strings.Repeat("a", MaxDecisionsMemoBytes+1)); !errors.Is(err, ErrDecisionsMemoTooLarge) {
		t.Fatalf("over-cap save err = %v, want ErrDecisionsMemoTooLarge", err)
	}
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run, 1, " \n\x01\t "); !errors.Is(err, ErrDecisionsMemoEmpty) {
		t.Fatalf("blank save err = %v, want ErrDecisionsMemoEmpty", err)
	}
	if got := e.memoRow(t, run); len(got.body) != MaxDecisionsMemoBytes {
		t.Fatalf("rejected writes changed the stored body (len %d)", len(got.body))
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_decision_memos WHERE run_id = $1`, run).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for the run = %d, %v; want exactly 1", n, err)
	}
}

// lineage fixture: the rework run under test hangs off (owner, repo, branch, mr_iid).
const (
	memoBranch = "agent/issue-2083"
	memoMR     = int64(77)
)

func (e interlockLiveDB) seedLineageRun(t *testing.T, w uuid.UUID, m memoRun) uuid.UUID {
	t.Helper()
	m.status = "completed"
	if m.branch == "" {
		m.branch = memoBranch
	}
	if m.mrIID == 0 {
		m.mrIID = memoMR
	}
	if m.kind == "mr_rework" && m.pipelineRef == "" {
		m.pipelineRef = m.branch
	}
	return e.seedMemoRun(t, w, m)
}

func (e interlockLiveDB) seedRework(t *testing.T, w uuid.UUID, gen int64) uuid.UUID {
	t.Helper()
	return e.seedMemoRun(t, w, memoRun{kind: "mr_rework", gen: gen, pipelineRef: memoBranch, mrIID: memoMR})
}

func (e interlockLiveDB) resolve(t *testing.T, svc *Service, w, run uuid.UUID, gen int64) *DecisionsMemo {
	t.Helper()
	m, err := svc.ResolveDecisionsMemo(e.ctx, memoWorker(e, w), run, gen)
	if err != nil {
		t.Fatalf("ResolveDecisionsMemo: %v", err)
	}
	return m
}

// TestDecisionsMemoGenerationCompatibilityLiveDB: a completed run's memo is served only while the
// row's generation equals the run's. Sequence 1: gen 1 saves, the run is re-claimed as gen 2 and
// completes without saving, so its stale memo is ignored and the prior compatible run's memo is
// served (null when none). Sequence 2: gen 2 saves (the row is restamped) and completes, so its
// memo is served; before completion it is not.
func TestDecisionsMemoGenerationCompatibilityLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	now := time.Now()

	// Sequence 1.
	stale := e.seedMemoRun(t, w, memoRun{branch: memoBranch, mrIID: memoMR})
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, stale, 1, "gen1 memo"); err != nil {
		t.Fatalf("gen1 save: %v", err)
	}
	e.setGen(t, stale, 2) // re-claim
	e.exec(t, `UPDATE runs SET status = 'completed', finished_at = $2 WHERE id = $1`, stale, now)

	rework := e.seedRework(t, w, 1)
	if m := e.resolve(t, svc, w, rework, 1); m != nil {
		t.Fatalf("resolver served the stale-generation memo %+v; want none", m)
	}
	prior := e.seedLineageRun(t, w, memoRun{finishedAt: now.Add(-time.Hour)})
	e.putMemo(t, prior, 1, 1, "prior compatible memo", now)
	m := e.resolve(t, svc, w, rework, 1)
	if m == nil || m.SourceRunID != prior || m.Body != "prior compatible memo" || m.Format != 1 {
		t.Fatalf("resolver = %+v, want the prior compatible run's memo %s", m, prior)
	}

	// Sequence 2 on a fresh lineage head: gen 2 saves over the gen 1 row, then completes.
	run2 := e.seedMemoRun(t, w, memoRun{branch: memoBranch, mrIID: memoMR, gen: 1})
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run2, 1, "gen1 of run2"); err != nil {
		t.Fatalf("run2 gen1 save: %v", err)
	}
	e.setGen(t, run2, 2)
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, run2, 2, "gen2 of run2"); err != nil {
		t.Fatalf("run2 gen2 save: %v", err)
	}
	if row := e.memoRow(t, run2); row.gen != 2 || row.body != "gen2 of run2" {
		t.Fatalf("row after the gen2 save = %+v, want restamped to generation 2", row)
	}
	if m := e.resolve(t, svc, w, rework, 1); m == nil || m.SourceRunID != prior {
		t.Fatalf("a still-running run's memo must not be served; got %+v", m)
	}
	e.exec(t, `UPDATE runs SET finished_at = $2 WHERE id = $1`, run2, now.Add(time.Minute))
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.q.WithTx(tx).SetRunCompleted(e.ctx, store.SetRunCompletedParams{
		ID: run2, WorkerID: pgconv.UUID(w),
		Branch: pgtype.Text{String: memoBranch, Valid: true}, MrIid: pgtype.Int8{Int64: memoMR, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetRunCompleted = %d, %v", n, err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	var gen int64
	if err := e.pool.QueryRow(e.ctx, `SELECT claim_generation FROM runs WHERE id = $1`, run2).Scan(&gen); err != nil || gen != 2 {
		t.Fatalf("SetRunCompleted changed claim_generation to %d (%v); want 2", gen, err)
	}
	if m := e.resolve(t, svc, w, rework, 1); m == nil || m.SourceRunID != run2 || m.Body != "gen2 of run2" {
		t.Fatalf("after completion the resolver = %+v, want run2's gen2 memo", m)
	}
}

// TestDecisionsMemoLineageScopingLiveDB: a memo is served only on the same owner, repo, branch and
// mr_iid, and never the rework's own, an unsupported format_version, or a non-completed run's.
func TestDecisionsMemoLineageScopingLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	now := time.Now()

	otherUser := uuid.New()
	e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, otherUser, fmt.Sprintf("memo-%s@e2e", otherUser))
	otherRepo := e.seedMemoRepo(t)

	rework := e.seedRework(t, w, 1)
	for _, tc := range []struct {
		name string
		run  memoRun
	}{
		{"wrong owner", memoRun{userID: otherUser, finishedAt: now}},
		{"wrong repo", memoRun{repoID: otherRepo, finishedAt: now}},
		{"wrong branch", memoRun{branch: "agent/issue-other", finishedAt: now}},
		{"wrong mr_iid", memoRun{mrIID: memoMR + 1, finishedAt: now}},
	} {
		id := e.seedLineageRun(t, w, tc.run)
		e.putMemo(t, id, 1, 1, tc.name, now)
	}
	// Unsupported format and a failed (non-completed) run on the right lineage.
	fmt2 := e.seedLineageRun(t, w, memoRun{finishedAt: now})
	e.putMemo(t, fmt2, 1, 2, "format two", now)
	failed := e.seedLineageRun(t, w, memoRun{finishedAt: now})
	e.exec(t, `UPDATE runs SET status = 'failed' WHERE id = $1`, failed)
	e.putMemo(t, failed, 1, 1, "failed run", now)
	// The rework's own memo (written before it completes) is never its own input.
	e.putMemo(t, rework, 1, 1, "own memo", now)

	if m := e.resolve(t, svc, w, rework, 1); m != nil {
		t.Fatalf("resolver = %+v, want none: every candidate is out of lineage, unsupported, failed or the run's own", m)
	}

	good := e.seedLineageRun(t, w, memoRun{finishedAt: now.Add(-time.Hour)})
	e.putMemo(t, good, 1, 1, "good", now)
	if m := e.resolve(t, svc, w, rework, 1); m == nil || m.SourceRunID != good {
		t.Fatalf("resolver = %+v, want the one in-lineage memo %s", m, good)
	}
}

// TestDecisionsMemoRoundsLiveDB: round two receives round one's updated memo over the issue run's;
// a failed later round falls back to the prior memo; a completed round without a memo keeps the
// prior one.
func TestDecisionsMemoRoundsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	now := time.Now()

	issue := e.seedLineageRun(t, w, memoRun{finishedAt: now.Add(-3 * time.Hour)})
	e.putMemo(t, issue, 1, 1, "issue memo", now.Add(-3*time.Hour))
	round1 := e.seedLineageRun(t, w, memoRun{kind: "mr_rework", finishedAt: now.Add(-2 * time.Hour)})
	e.putMemo(t, round1, 1, 1, "round one memo", now.Add(-2*time.Hour))

	round2 := e.seedRework(t, w, 1)
	if m := e.resolve(t, svc, w, round2, 1); m == nil || m.SourceRunID != round1 || m.Body != "round one memo" {
		t.Fatalf("round two got %+v, want round one's memo over the issue run's", m)
	}

	// A failed round 2 never supersedes: round 3 still reads round one's memo.
	e.exec(t, `UPDATE runs SET status = 'failed', finished_at = $2 WHERE id = $1`, round2, now.Add(-time.Hour))
	e.putMemo(t, round2, 1, 1, "failed round memo", now.Add(-time.Hour))
	round3 := e.seedRework(t, w, 1)
	if m := e.resolve(t, svc, w, round3, 1); m == nil || m.SourceRunID != round1 {
		t.Fatalf("round three after a failed round got %+v, want round one's memo", m)
	}

	// A completed round with no memo keeps the prior one.
	e.exec(t, `UPDATE runs SET status = 'completed', finished_at = $2 WHERE id = $1`, round3, now.Add(-30*time.Minute))
	round4 := e.seedRework(t, w, 1)
	if m := e.resolve(t, svc, w, round4, 1); m == nil || m.SourceRunID != round1 {
		t.Fatalf("round four after a memo-less completed round got %+v, want round one's memo", m)
	}
}

// TestDecisionsMemoResolveFencesAndKindLiveDB: the read needs the run's current claim, answers
// null for a non-mr_rework run, and a stale or released claim is refused.
func TestDecisionsMemoResolveFencesAndKindLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	other := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	prior := e.seedLineageRun(t, w, memoRun{finishedAt: time.Now()})
	e.putMemo(t, prior, 1, 1, "prior", time.Now())

	rework := e.seedRework(t, w, 2)
	if _, err := svc.ResolveDecisionsMemo(e.ctx, wkr, rework, 1); !errors.Is(err, ErrDecisionsMemoClaimNotCurrent) {
		t.Fatalf("stale-generation read err = %v, want ErrDecisionsMemoClaimNotCurrent", err)
	}
	if _, err := svc.ResolveDecisionsMemo(e.ctx, memoWorker(e, other), rework, 2); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("another worker's read err = %v, want ErrRunNotOwned", err)
	}
	if m := e.resolve(t, svc, w, rework, 2); m == nil || m.SourceRunID != prior {
		t.Fatalf("current-claim read = %+v, want %s", m, prior)
	}
	e.exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, rework)
	if _, err := svc.ResolveDecisionsMemo(e.ctx, wkr, rework, 2); !errors.Is(err, ErrDecisionsMemoClaimNotCurrent) {
		t.Fatalf("released-claim read err = %v, want ErrDecisionsMemoClaimNotCurrent", err)
	}

	issue := e.seedMemoRun(t, w, memoRun{branch: memoBranch, mrIID: memoMR})
	if m := e.resolve(t, svc, w, issue, 1); m != nil {
		t.Fatalf("an issue run reads %+v, want none (only mr_rework resumes from a memo)", m)
	}
}

// TestMRReworkCompletionStoresBranchEqualsPipelineRefLiveDB: the lineage key assumes a completed
// mr_rework run's runs.branch equals the pipeline_ref it was created with. Complete one through the
// real SetState path (the worker reports the branch it pushed) and read the column back.
func TestMRReworkCompletionStoresBranchEqualsPipelineRefLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)
	rework := e.seedRework(t, w, 1)
	if _, err := svc.SaveDecisionsMemo(e.ctx, wkr, rework, 1, "round memo"); err != nil {
		t.Fatalf("save: %v", err)
	}

	branch, mr, gen := memoBranch, memoMR, int64(1)
	run, applied, err := svc.SetState(e.ctx, wkr, rework, StateRequest{State: "completed", Branch: &branch, MrIID: &mr, ClaimGeneration: &gen})
	if err != nil || !applied || run.Status != "completed" {
		t.Fatalf("SetState completed = status %q applied %v err %v", run.Status, applied, err)
	}
	var gotBranch, gotRef string
	var gotGen int64
	if err := e.pool.QueryRow(e.ctx, `SELECT branch, pipeline_ref, claim_generation FROM runs WHERE id = $1`, rework).Scan(&gotBranch, &gotRef, &gotGen); err != nil {
		t.Fatal(err)
	}
	if gotBranch != gotRef || gotGen != 1 {
		t.Fatalf("completed mr_rework: branch %q pipeline_ref %q generation %d; want branch == pipeline_ref and generation 1", gotBranch, gotRef, gotGen)
	}
	// And the completed run's memo now resolves for the next round.
	next := e.seedRework(t, w, 1)
	if m := e.resolve(t, svc, w, next, 1); m == nil || m.SourceRunID != rework || m.Body != "round memo" {
		t.Fatalf("next round resolves %+v, want the completed round's memo", m)
	}
}

// TestUpsertRunDecisionMemoFencedWorkerPredicateLiveDB: the store query alone, with a worker id that
// is not the run's holder, writes nothing even for a running run at its current generation (the
// service's runOwnedByWorker read would otherwise shadow the worker_id predicate).
func TestUpsertRunDecisionMemoFencedWorkerPredicateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	other := e.seedWorker(t, nil)
	run := e.seedMemoRun(t, w, memoRun{gen: 2})
	if _, err := svc.SaveDecisionsMemo(e.ctx, memoWorker(e, w), run, 2, "held"); err != nil {
		t.Fatalf("save: %v", err)
	}
	before := e.memoRow(t, run)

	n, err := e.q.UpsertRunDecisionMemoFenced(e.ctx, store.UpsertRunDecisionMemoFencedParams{
		RunID: run, WorkerID: pgconv.UUID(other), ClaimGeneration: 2, Body: "intruder",
	})
	if err != nil || n != 0 {
		t.Fatalf("upsert as another worker = %d, %v; want 0 rows, nil", n, err)
	}
	if after := e.memoRow(t, run); after != before {
		t.Fatalf("a refused write mutated the row: before %+v after %+v", before, after)
	}
}

// TestGetLatestDecisionMemoForLineageExcludesSelfLiveDB: the store query alone, called with the
// caller's own run id, never returns that run even when it is completed with a valid compatible memo
// and is the only lineage match (the service's status/kind gate would otherwise shadow the
// r.id <> self_run_id predicate).
func TestGetLatestDecisionMemoForLineageExcludesSelfLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	now := time.Now()
	self := e.seedLineageRun(t, w, memoRun{kind: "mr_rework", finishedAt: now})
	e.putMemo(t, self, 1, 1, "own completed memo", now)

	params := store.GetLatestDecisionMemoForLineageParams{
		UserID: e.userID, RepoID: pgconv.UUID(e.repoID),
		Branch: pgtype.Text{String: memoBranch, Valid: true},
		MrIid:  pgtype.Int8{Int64: memoMR, Valid: true},
	}
	params.SelfRunID = uuid.New()
	if row, err := e.q.GetLatestDecisionMemoForLineage(e.ctx, params); err != nil || row.RunID != self {
		t.Fatalf("control: another caller resolves %+v, %v; want the run %s", row, err, self)
	}
	params.SelfRunID = self
	if row, err := e.q.GetLatestDecisionMemoForLineage(e.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("self-excluded lookup = %+v, %v; want pgx.ErrNoRows", row, err)
	}
}
