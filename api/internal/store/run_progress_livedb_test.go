package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// runProgressDB opens the live database (skipping without UZI_TEST_DATABASE_URL, as the
// other *LiveDB tests do), migrates it, and returns the pool.
func runProgressDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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
	return ctx, pool
}

// runProgressScene is one owner with one forge connection and repo, every id fresh so
// scenes never collide on a shared database.
type runProgressScene struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	user   uuid.UUID
	repo   uuid.UUID
	conn   uuid.UUID
	nextID int64
}

func (s *runProgressScene) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(s.ctx, sql, args...); err != nil {
		s.t.Fatalf("exec %q: %v", sql, err)
	}
}

func newRunProgressScene(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *runProgressScene {
	t.Helper()
	s := &runProgressScene{t: t, ctx: ctx, pool: pool, user: uuid.New(), conn: uuid.New(), nextID: 100}
	s.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, s.user, fmt.Sprintf("rp-%s@e2e", s.user))
	s.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, s.conn, s.user, []byte{0x1})
	s.repo = s.newRepo()
	return s
}

// newRepo adds another repo under the scene's connection.
func (s *runProgressScene) newRepo() uuid.UUID {
	id := uuid.New()
	s.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/rp', 'main', true)`,
		id, s.conn, time.Now().UnixNano()%1_000_000_000, "g/rp-"+id.String())
	return id
}

// issueRun inserts an issue run for iid in repo, owned by user, created ageMinutes ago.
func (s *runProgressScene) issueRun(user, repo uuid.UUID, iid int64, status string, ageMinutes int) uuid.UUID {
	id := uuid.New()
	s.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, created_at)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, now() - make_interval(mins => $6))`,
		id, user, repo, iid, status, ageMinutes)
	return id
}

// reworkRun inserts an mr_rework run on branch agent/issue-<iid> (pipeline_ref), targeting
// an existing source run.
func (s *runProgressScene) reworkRun(user, repo uuid.UUID, iid int64, status string, target uuid.UUID) uuid.UUID {
	id := uuid.New()
	s.nextID++
	s.exec(`INSERT INTO runs (id, user_id, repo_id, kind, pipeline_ref, mr_iid, target_run_id, issue_title, issue_description, status)
	        VALUES ($1, $2, $3, 'mr_rework', $4, $5, $6, 't', 'd', $7)`,
		id, user, repo, fmt.Sprintf("agent/issue-%d", iid), s.nextID, target, status)
	return id
}

// TestRunProgressBlockingRunLiveDB executes GetMaybeBlockingRun (PRD #2602 M2) against a
// real Postgres. Every exclusion case has a positive twin: the scene's baseline blocker is
// found first, so an exclusion passes because of the predicate, not because the query
// matches nothing.
func TestRunProgressBlockingRunLiveDB(t *testing.T) {
	ctx, pool := runProgressDB(t)
	q := store.New(pool)

	find := func(s *runProgressScene, owner, repo, self uuid.UUID, iids []int64) (uuid.UUID, error) {
		refs := make([]string, 0, len(iids))
		for _, n := range iids {
			refs = append(refs, fmt.Sprintf("agent/issue-%d", n))
		}
		return q.GetMaybeBlockingRun(ctx, store.GetMaybeBlockingRunParams{Owner: owner, RepoID: repo, RunID: self, Iids: iids, Refs: refs})
	}
	wantFound := func(t *testing.T, got uuid.UUID, err error, want uuid.UUID) {
		t.Helper()
		if err != nil || got != want {
			t.Fatalf("got (%s, %v), want %s", got, err, want)
		}
	}
	wantNone := func(t *testing.T, got uuid.UUID, err error) {
		t.Helper()
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got (%s, %v), want pgx.ErrNoRows", got, err)
		}
	}

	t.Run("issue arm matches a live issue run", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		blocker := s.issueRun(s.user, s.repo, 2512, "running", 4)
		got, err := find(s, s.user, s.repo, parked, []int64{2512})
		wantFound(t, got, err, blocker)
	})

	t.Run("ref arm matches a live mr_rework run by pipeline_ref", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		source := s.issueRun(s.user, s.repo, 77, "completed", 30)
		rework := s.reworkRun(s.user, s.repo, 77, "running", source)
		// A live issue run on a different number must not be what matches.
		s.issueRun(s.user, s.repo, 99, "running", 3)
		got, err := find(s, s.user, s.repo, parked, []int64{77})
		wantFound(t, got, err, rework)
	})

	t.Run("a different owner is excluded", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		blocker := s.issueRun(s.user, s.repo, 2512, "running", 4)
		got, err := find(s, s.user, s.repo, parked, []int64{2512})
		wantFound(t, got, err, blocker) // positive control for the owner predicate

		other := newRunProgressScene(t, ctx, pool)
		// The other owner's run sits in the SAME repo (repos are connection-owned, so reuse ours).
		other.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		            VALUES ($1, $2, $3, 'issue', 3000, 't', 'd', 'running')`, uuid.New(), other.user, s.repo)
		got, err = find(s, s.user, s.repo, parked, []int64{3000})
		wantNone(t, got, err)
	})

	t.Run("a different repo is excluded", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		otherRepo := s.newRepo()
		s.issueRun(s.user, otherRepo, 2512, "running", 4)
		got, err := find(s, s.user, s.repo, parked, []int64{2512})
		wantNone(t, got, err)
		// Positive twin: the same run is found when asked about its own repo.
		got, err = find(s, s.user, otherRepo, parked, []int64{2512})
		if err != nil || got == uuid.Nil {
			t.Fatalf("control: got (%s, %v), want the other-repo run", got, err)
		}
	})

	t.Run("the run itself is excluded", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		self := s.issueRun(s.user, s.repo, 2512, "awaiting_input", 5)
		got, err := find(s, s.user, s.repo, self, []int64{2512})
		wantNone(t, got, err)
		got, err = find(s, s.user, s.repo, uuid.New(), []int64{2512})
		wantFound(t, got, err, self) // control: only the id <> predicate hid it
	})

	t.Run("terminal runs are excluded", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		for i, status := range []string{"completed", "failed", "cancelled"} {
			s.issueRun(s.user, s.repo, int64(4000+i), status, 4)
			got, err := find(s, s.user, s.repo, parked, []int64{int64(4000 + i)})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("%s: got (%s, %v), want pgx.ErrNoRows", status, got, err)
			}
		}
		live := s.issueRun(s.user, s.repo, 4100, "queued", 4)
		got, err := find(s, s.user, s.repo, parked, []int64{4100})
		wantFound(t, got, err, live)
	})

	t.Run("pool_wait counts as live", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		waiting := s.issueRun(s.user, s.repo, 2600, "pool_wait", 4)
		got, err := find(s, s.user, s.repo, parked, []int64{2600})
		wantFound(t, got, err, waiting)
	})

	t.Run("newest matching run wins", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		s.issueRun(s.user, s.repo, 5001, "running", 60)
		newest := s.issueRun(s.user, s.repo, 5002, "running", 1)
		s.issueRun(s.user, s.repo, 5003, "running", 30)
		got, err := find(s, s.user, s.repo, parked, []int64{5001, 5002, 5003})
		wantFound(t, got, err, newest)
	})

	t.Run("an empty reference set matches nothing", func(t *testing.T) {
		s := newRunProgressScene(t, ctx, pool)
		parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
		s.issueRun(s.user, s.repo, 2512, "running", 4)
		got, err := find(s, s.user, s.repo, parked, []int64{})
		wantNone(t, got, err)
	})
}

// TestRunProgressAnswerExistsLiveDB executes RunQuestionAnswerExists: any submitted answer
// for the question counts (applied or not), another question id or another kind does not.
func TestRunProgressAnswerExistsLiveDB(t *testing.T) {
	ctx, pool := runProgressDB(t)
	q := store.New(pool)
	s := newRunProgressScene(t, ctx, pool)
	run := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
	other := s.issueRun(s.user, s.repo, 11, "awaiting_input", 5)

	ask := func(runID uuid.UUID, qid string) bool {
		t.Helper()
		got, err := q.RunQuestionAnswerExists(ctx, store.RunQuestionAnswerExistsParams{
			RunID: runID, QuestionID: pgtype.Text{String: qid, Valid: true},
		})
		if err != nil {
			t.Fatalf("RunQuestionAnswerExists: %v", err)
		}
		return got
	}
	if ask(run, "q1") {
		t.Fatalf("no input rows yet: want false")
	}

	// An unapplied answer (submitted, not consumed by the worker) counts.
	s.exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id) VALUES ($1, 'answer', '{}', 'q1')`, run)
	if !ask(run, "q1") {
		t.Errorf("unapplied answer: want true")
	}
	// An applied answer counts too.
	s.exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id, applied_at) VALUES ($1, 'answer', '{}', 'q2', now())`, run)
	if !ask(run, "q2") {
		t.Errorf("applied answer: want true")
	}
	// Another question id on the same run does not.
	if ask(run, "q3") {
		t.Errorf("answer for q1/q2 must not satisfy q3")
	}
	// The same question id on another run does not.
	if ask(other, "q1") {
		t.Errorf("answer on another run must not satisfy this run")
	}
	// A non-answer input for the question does not.
	s.exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id) VALUES ($1, 'follow_up', 'x', 'q9')`, run)
	if ask(run, "q9") {
		t.Errorf("a follow_up input must not count as an answer")
	}
}

// explainDB is a store.DBTX that runs EXPLAIN over the real generated statement text and
// keeps the plan, so the plan asserted is the one the generated query would run (not a
// hand-copied SQL string that could drift).
type explainDB struct {
	store.DBTX
	tx   pgx.Tx
	plan strings.Builder
}

func (d *explainDB) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	rows, err := d.tx.Query(ctx, "EXPLAIN (COSTS OFF) "+sql, args...)
	if err != nil {
		return explainErrRow{err}
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return explainErrRow{err}
		}
		d.plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		return explainErrRow{err}
	}
	return explainErrRow{pgx.ErrNoRows}
}

type explainErrRow struct{ err error }

func (r explainErrRow) Scan(...any) error { return r.err }

// TestRunProgressIndexPlansLiveDB asserts the SHAPE of the two hint queries' plans: the
// indexes the migration and the PRD rely on are used and the big tables are not scanned
// sequentially. It does not assert the full plan text.
//
// Filler rows give the planner a reason to prefer an index (on a near-empty table it would
// rightly pick a seq scan), and ANALYZE refreshes the statistics.
func TestRunProgressIndexPlansLiveDB(t *testing.T) {
	ctx, pool := runProgressDB(t)
	s := newRunProgressScene(t, ctx, pool)

	filler := s.newRepo()
	s.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	        SELECT gen_random_uuid(), $1, $2, 'issue', g, 'f', 'f', 'completed' FROM generate_series(1000, 9000) g`, s.user, filler)
	s.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	        SELECT gen_random_uuid(), $1, $2, 'issue', g, 'f', 'f', 'completed' FROM generate_series(1000, 3000) g`, s.user, s.repo)
	parked := s.issueRun(s.user, s.repo, 10, "awaiting_input", 5)
	s.exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id)
	        SELECT $1, 'follow_up', 'x', 'f' || g FROM generate_series(1, 8000) g`, parked)
	s.exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id)
	        SELECT $1, 'answer', '{}', 'a' || g FROM generate_series(1, 100) g`, parked)
	s.exec(`ANALYZE runs`)
	s.exec(`ANALYZE run_user_inputs`)

	planOf := func(t *testing.T, run func(q *store.Queries)) string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		d := &explainDB{tx: tx}
		run(store.New(d))
		return d.plan.String()
	}

	t.Run("GetMaybeBlockingRun uses idx_runs_issue_history, no seq scan of runs", func(t *testing.T) {
		plan := planOf(t, func(q *store.Queries) {
			_, _ = q.GetMaybeBlockingRun(ctx, store.GetMaybeBlockingRunParams{
				Owner: s.user, RepoID: s.repo, RunID: parked,
				Iids: []int64{2512, 77}, Refs: []string{"agent/issue-2512", "agent/issue-77"},
			})
		})
		if !strings.Contains(plan, "idx_runs_issue_history") {
			t.Errorf("plan does not use idx_runs_issue_history:\n%s", plan)
		}
		if strings.Contains(plan, "Seq Scan on runs") {
			t.Errorf("plan seq-scans runs:\n%s", plan)
		}
	})

	t.Run("RunQuestionAnswerExists uses idx_run_user_inputs_answer, no seq scan of run_user_inputs", func(t *testing.T) {
		plan := planOf(t, func(q *store.Queries) {
			_, _ = q.RunQuestionAnswerExists(ctx, store.RunQuestionAnswerExistsParams{
				RunID: parked, QuestionID: pgtype.Text{String: "a5", Valid: true},
			})
		})
		if !strings.Contains(plan, "idx_run_user_inputs_answer") {
			t.Errorf("plan does not use idx_run_user_inputs_answer:\n%s", plan)
		}
		if strings.Contains(plan, "Seq Scan on run_user_inputs") {
			t.Errorf("plan seq-scans run_user_inputs:\n%s", plan)
		}
	})
}
