package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type continuedIssueFixture struct {
	ctx                   context.Context
	pool                  *pgxpool.Pool
	q                     *store.Queries
	user, repo, otherRepo uuid.UUID
	nextIID               int64
}

func newContinuedIssueFixture(t *testing.T) *continuedIssueFixture {
	t.Helper()
	ctx, pool := runProgressDB(t)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	t.Cleanup(cancel)
	f := &continuedIssueFixture{ctx: ctx, pool: pool, q: store.New(pool),
		user: uuid.New(), repo: uuid.New(), otherRepo: uuid.New()}
	conn := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')",
		f.user, fmt.Sprintf("continued-%s@e2e", f.user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections
		(id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, f.user, []byte{1})
	for i, repo := range []uuid.UUID{f.repo, f.otherRepo} {
		mustExec(ctx, t, pool, `INSERT INTO repos
			(id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
			VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main')`,
			repo, conn, i+1, "g/"+repo.String())
	}
	return f
}

func (f *continuedIssueFixture) iid() int64 {
	f.nextIID++
	return f.nextIID
}

const insertContinuedIssueRun = `INSERT INTO runs
	(id, user_id, repo_id, kind, issue_iid, continued_issue_iid, branch,
	 issue_title, issue_description, status, mr_iid, mr_state)
	VALUES ($1, $2, $3, $4, $5, $6, 'task-branch', 't', 'd', $7, $8, $9)`

func (f *continuedIssueFixture) insert(t *testing.T, repo uuid.UUID, kind, status string, issue, continued, mr, state any) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(f.ctx, t, f.pool, insertContinuedIssueRun,
		id, f.user, repo, kind, issue, continued, status, mr, state)
	return id
}

func requireContinuedIssuePGError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
		t.Fatalf("error = %v, want SQLSTATE %s constraint %s", err, code, constraint)
	}
}

func (f *continuedIssueFixture) active(t *testing.T, repo uuid.UUID, iid int64, want bool) {
	t.Helper()
	got, err := f.q.HasActiveRunForIssue(f.ctx, store.HasActiveRunForIssueParams{
		RepoID: repo, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
	})
	if err != nil || got != want {
		t.Fatalf("HasActiveRunForIssue(%s, %d) = %v, %v; want %v", repo, iid, got, err, want)
	}
}

func TestTaskContinuedIssuePoliciesLiveDB(t *testing.T) {
	f := newContinuedIssueFixture(t)
	// Expectations are explicit: pool_wait blocks the query but does not own the index.
	statuses := []struct {
		name            string
		active, indexed bool
	}{
		{"queued", true, true}, {"claimed", true, true}, {"running", true, true},
		{"awaiting_approval", true, true}, {"awaiting_input", true, true},
		{"limit_wait", true, true}, {"completed", false, false}, {"failed", false, false},
		{"cancelled", false, false}, {"awaiting_followup", true, true},
		{"pool_wait", true, false}, {"paused", true, true}, {"recovery_wait", true, true},
	}
	for _, status := range statuses {
		t.Run(status.name, func(t *testing.T) {
			for _, kind := range []string{"issue", "task", "self_improve"} {
				t.Run(kind, func(t *testing.T) {
					iid := f.iid()
					var issue, continued any = iid, nil
					if kind == "task" {
						issue, continued = nil, iid
					}
					id := f.insert(t, f.repo, kind, status.name, issue, continued, nil, nil)
					t.Cleanup(func() { mustExec(f.ctx, t, f.pool, "DELETE FROM runs WHERE id = $1", id) })
					f.active(t, f.repo, iid, status.active)
					f.active(t, f.otherRepo, iid, false)
					f.active(t, f.repo, f.iid(), false)
					// Exercise the index independently of the query, against both kinds.
					for _, challenger := range []string{"issue", "task"} {
						var ci, cc any = iid, nil
						if challenger == "task" {
							ci, cc = nil, iid
						}
						id := uuid.New()
						_, err := f.pool.Exec(f.ctx, insertContinuedIssueRun,
							id, f.user, f.repo, challenger, ci, cc, "queued", nil, nil)
						if status.indexed && kind != "self_improve" {
							requireContinuedIssuePGError(t, err, "23505", "uq_runs_one_active_per_issue")
						} else {
							if err != nil {
								t.Fatalf("non-indexed %s blocks %s: %v", kind, challenger, err)
							}
							mustExec(f.ctx, t, f.pool, "DELETE FROM runs WHERE id = $1", id)
						}
					}
				})
			}
		})
	}
	t.Run("unlinked and unrelated tasks", func(t *testing.T) {
		iid := f.iid()
		f.insert(t, f.repo, "task", "queued", nil, nil, nil, nil)
		f.insert(t, f.repo, "task", "queued", nil, nil, nil, nil)
		f.insert(t, f.otherRepo, "task", "queued", nil, iid, nil, nil)
		f.insert(t, f.repo, "task", "queued", nil, f.iid(), nil, nil)
		f.active(t, f.repo, iid, false)
		// A different repo and issue must also leave the structural slot free.
		f.insert(t, f.repo, "issue", "queued", iid, nil, nil, nil)
	})
}

func TestTaskContinuedIssueShapeLiveDB(t *testing.T) {
	f := newContinuedIssueFixture(t)
	cases := []struct {
		name, kind             string
		repo, issue, continued any
	}{
		{"zero", "task", f.repo, nil, int64(0)},
		{"negative", "task", f.repo, nil, int64(-1)},
		{"issue", "issue", f.repo, int64(1), int64(1)},
		{"self improve", "self_improve", f.repo, int64(2), int64(2)},
		{"prompt", "prompt", f.repo, nil, int64(3)},
		{"task without repo", "task", nil, nil, int64(4)},
		{"task with issue", "task", f.repo, int64(5), int64(5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.pool.Exec(f.ctx, insertContinuedIssueRun,
				uuid.New(), f.user, tc.repo, tc.kind, tc.issue, tc.continued, "queued", nil, nil)
			requireContinuedIssuePGError(t, err, "23514", "runs_continued_issue_shape")
		})
	}
	f.insert(t, f.repo, "task", "queued", nil, f.iid(), nil, nil)
	f.insert(t, f.repo, "task", "queued", nil, nil, nil, nil)
	f.insert(t, f.repo, "issue", "queued", f.iid(), nil, nil, nil)
}

func TestTaskContinuedIssueConcurrentInsertLiveDB(t *testing.T) {
	f := newContinuedIssueFixture(t)
	for _, kinds := range [][2]string{{"issue", "task"}, {"task", "task"}} {
		t.Run(kinds[0]+" vs "+kinds[1], func(t *testing.T) {
			iid := f.iid()
			// Acquire both connections before releasing the barrier; each makes one
			// autocommit attempt. The fixture deadline bounds DB waits; neither
			// contender depends on the other's result or retries after failure.
			conns := make([]*pgxpool.Conn, 2)
			for i := range conns {
				conn, err := f.pool.Acquire(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Release()
				conns[i] = conn
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			ids := [2]uuid.UUID{uuid.New(), uuid.New()}
			for i, kind := range kinds {
				go func() {
					<-start
					var issue, continued any = iid, nil
					if kind == "task" {
						issue, continued = nil, iid
					}
					_, err := conns[i].Exec(f.ctx, insertContinuedIssueRun,
						ids[i], f.user, f.repo, kind, issue, continued, "queued", nil, nil)
					results <- err
				}()
			}
			close(start)
			// Drain both attempts before any fatal assertion can return this test.
			attempts := [2]error{<-results, <-results}
			winners := 0
			for _, err := range attempts {
				if err == nil {
					winners++
				} else {
					requireContinuedIssuePGError(t, err, "23505", "uq_runs_one_active_per_issue")
				}
			}
			if winners != 1 {
				t.Fatalf("successful inserts = %d, want exactly one", winners)
			}
			var stored int
			if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM runs WHERE id = ANY($1::uuid[])", ids[:]).Scan(&stored); err != nil || stored != 1 {
				t.Fatalf("persisted contenders = %d, %v; want 1", stored, err)
			}
		})
	}
}

func TestTaskContinuedIssueOpenMRLiveDB(t *testing.T) {
	f := newContinuedIssueFixture(t)
	states := []struct {
		name  string
		value any
		block bool
	}{
		{"null", nil, true}, {"opened", "opened", true}, {"locked", "locked", true},
		{"closed", "closed", false}, {"merged", "merged", false},
	}
	check := func(t *testing.T, iid int64, want bool) {
		t.Helper()
		mr, err := f.q.GetOpenMRRunForIssue(f.ctx, store.GetOpenMRRunForIssueParams{
			RepoID: f.repo, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
		})
		if want {
			if err != nil || !mr.Valid || mr.Int64 != 71 {
				t.Fatalf("GetOpenMRRunForIssue = %v, %v; want MR 71", mr, err)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetOpenMRRunForIssue = %v, %v; want no rows", mr, err)
		}
	}
	for _, kind := range []string{"issue", "task"} {
		for _, state := range states {
			t.Run(kind+"/"+state.name, func(t *testing.T) {
				iid := f.iid()
				var issue, continued any = iid, nil
				if kind == "task" {
					issue, continued = nil, iid
				}
				f.insert(t, f.repo, kind, "completed", issue, continued, int64(71), state.value)
				check(t, iid, state.block)
			})
		}
		for _, control := range []string{"other repo", "other issue", "no MR", "not completed", "unlinked"} {
			t.Run(kind+"/"+control, func(t *testing.T) {
				iid, repo, status := f.iid(), f.repo, "completed"
				var issue, continued, mr any = iid, nil, int64(71)
				if kind == "task" {
					issue, continued = nil, iid
				}
				switch control {
				case "other repo":
					repo = f.otherRepo
				case "other issue":
					if kind == "task" {
						continued = f.iid()
					} else {
						issue = f.iid()
					}
				case "no MR":
					mr = nil
				case "not completed":
					status = "running"
				case "unlinked":
					if kind != "task" {
						return
					}
					continued = nil
				}
				f.insert(t, repo, kind, status, issue, continued, mr, "opened")
				check(t, iid, false)
			})
		}
	}
	t.Run("self improve is not an issue MR", func(t *testing.T) {
		iid := f.iid()
		f.insert(t, f.repo, "self_improve", "completed", iid, nil, int64(71), "opened")
		check(t, iid, false)
	})
}
