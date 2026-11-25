package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue #4684: live-database proofs for RunAnalyticsSnapshot and the analytics options and
// authority statements. Skipped without UZI_TEST_DATABASE_URL.

func openAnalyticsSnapshotDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

func analyticsExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %.60q: %v", sql, err)
	}
}

func TestAnalyticsSnapshotSettingsLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsSnapshotDB(t)
	err := RunAnalyticsSnapshot(ctx, pool, func(dctx context.Context, s *AnalyticsSnapshot) error {
		for _, c := range []struct{ name, want string }{
			{"transaction_isolation", "repeatable read"},
			{"transaction_read_only", "on"},
			{"statement_timeout", "8s"},
			{"jit", "off"},
		} {
			var got string
			if err := s.tx.QueryRow(dctx, "SHOW "+c.name).Scan(&got); err != nil {
				return err
			}
			if got != c.want {
				t.Errorf("%s = %q, want %q", c.name, got, c.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnalyticsSnapshotStatementTimeoutLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsSnapshotDB(t)
	var sawErr error
	err := RunAnalyticsSnapshot(ctx, pool, func(dctx context.Context, s *AnalyticsSnapshot) error {
		if _, err := s.tx.Exec(dctx, "SET LOCAL statement_timeout = '1ms'"); err != nil {
			return err
		}
		_, sawErr = s.tx.Exec(dctx, "SELECT pg_sleep(2)")
		if dctx.Err() != nil {
			t.Errorf("dctx already expired: %v", dctx.Err())
		}
		return sawErr
	})
	var pgErr *pgconn.PgError
	if !errors.As(sawErr, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("want SQLSTATE 57014, got %v", sawErr)
	}
	if got := ClassifyAnalyticsErr(err); !errors.Is(got, ErrAnalyticsTimeout) {
		t.Fatalf("classified %v, want ErrAnalyticsTimeout", got)
	}
}

func TestAnalyticsOptionsQueriesLiveDB(t *testing.T) {
	ctx, pool := openAnalyticsSnapshotDB(t)
	tag := uuid.NewString()[:8]
	owner, other := uuid.New(), uuid.New()
	ownerConn, otherConn := uuid.New(), uuid.New()
	for i, u := range []uuid.UUID{owner, other} {
		analyticsExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("aopt-%s-%d@e2e", tag, i))
	}
	for _, c := range []struct{ conn, user uuid.UUID }{{ownerConn, owner}, {otherConn, other}} {
		analyticsExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, c.conn, c.user, []byte{0x1})
	}
	// Owner: 202 repos, ids random so the (path, id) order is not insertion order.
	analyticsExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 SELECT gen_random_uuid(), $1, i, 'r'||$2||lpad(i::text, 4, '0'), 'https://forge.e2e/x', 'main', true
		 FROM generate_series(1, 202) i`, ownerConn, tag)
	otherRepo := uuid.New()
	analyticsExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'r'||$3||'0000foreign', 'https://forge.e2e/x', 'main', true)`, otherRepo, otherConn, tag)

	q := New(pool)
	rows, err := q.AnalyticsOwnerRepoOptions(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 201 {
		t.Fatalf("owner options = %d rows, want 201", len(rows))
	}
	for i, r := range rows {
		if want := fmt.Sprintf("r%s%04d", tag, i+1); r.PathWithNamespace != want {
			t.Fatalf("row %d path %q, want %q", i, r.PathWithNamespace, want)
		}
		if r.ID == otherRepo {
			t.Fatal("other user's repo leaked into owner options")
		}
	}

	all, err := q.AnalyticsAllRepoOptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 201 || !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].PathWithNamespace < all[j].PathWithNamespace }) {
		t.Fatalf("all options: %d rows or unsorted", len(all))
	}
	users, err := q.AnalyticsUserOptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) == 0 || len(users) > 201 || !sort.SliceIsSorted(users, func(i, j int) bool { return users[i].Email < users[j].Email }) {
		t.Fatalf("user options: %d rows or unsorted", len(users))
	}
	tzs, err := q.AnalyticsTimezoneNames(ctx)
	if err != nil || len(tzs) == 0 {
		t.Fatalf("timezones: %d, %v", len(tzs), err)
	}

	acc := func(u, r uuid.UUID) bool {
		ok, err := q.AnalyticsOwnerRepoAccessible(ctx, AnalyticsOwnerRepoAccessibleParams{UserID: u, RepoID: r})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !acc(other, otherRepo) || acc(owner, otherRepo) || acc(owner, uuid.New()) {
		t.Fatal("accessible point check wrong")
	}
	if ok, _ := q.AnalyticsRepoExists(ctx, otherRepo); !ok {
		t.Fatal("repo should exist")
	}
	if ok, _ := q.AnalyticsRepoExists(ctx, uuid.New()); ok {
		t.Fatal("random repo should not exist")
	}
	if ok, _ := q.AnalyticsUserExists(ctx, owner); !ok {
		t.Fatal("user should exist")
	}
	if ok, _ := q.AnalyticsUserExists(ctx, uuid.New()); ok {
		t.Fatal("random user should not exist")
	}

	// History start: owner runs in repo 1 and 2, kinds issue/chat/self_improve, harness mixed.
	repo1, repo2 := rows[0].ID, rows[1].ID
	base := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	iid := int64(4_684_000)
	seed := func(u, repo uuid.UUID, kind, harness string, at time.Time) {
		iid++
		var repoArg, iidArg any = repo, iid
		if kind == "chat" {
			repoArg, iidArg = nil, nil // runs_kind_shape: a chat run has no repo or issue
		}
		analyticsExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, created_at, finished_at, harness)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'completed', $6, $6, $7)`,
			uuid.New(), u, repoArg, iidArg, kind, at, harness)
	}
	seed(owner, repo1, "issue", "claude", base.Add(48*time.Hour))
	seed(owner, repo1, "self_improve", "codex", base.Add(24*time.Hour))
	seed(owner, repo2, "issue", "codex", base.Add(72*time.Hour))
	seed(owner, uuid.Nil, "chat", "claude", base)
	seed(other, otherRepo, "issue", "claude", base.Add(-240*time.Hour))

	hs := func(user *uuid.UUID, repo *uuid.UUID, harness string, kinds []string) *time.Time {
		var r pgtype.UUID
		if repo != nil {
			r = pgtype.UUID{Bytes: *repo, Valid: true}
		}
		var h pgtype.Text
		if harness != "" {
			h = pgtype.Text{String: harness, Valid: true}
		}
		var v pgtype.Timestamptz
		var err error
		if user != nil {
			v, err = q.AnalyticsOwnerHistoryStart(ctx, AnalyticsOwnerHistoryStartParams{UserID: *user, RepoID: r, Harness: h, Kinds: kinds})
		} else {
			v, err = q.AnalyticsAllHistoryStart(ctx, AnalyticsAllHistoryStartParams{RepoID: r, Harness: h, Kinds: kinds})
		}
		if err != nil {
			t.Fatal(err)
		}
		if !v.Valid {
			return nil
		}
		return &v.Time
	}
	eq := func(name string, got *time.Time, want time.Time) {
		t.Helper()
		if got == nil || !got.Equal(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	allKinds := []string{"issue", "self_improve", "chat"}
	eq("owner all kinds", hs(&owner, nil, "", allKinds), base)
	eq("owner delivery only", hs(&owner, nil, "", []string{"issue", "self_improve"}), base.Add(24*time.Hour))
	eq("owner repo2 issue", hs(&owner, &repo2, "", []string{"issue"}), base.Add(72*time.Hour))
	eq("owner claude issue+ci_fix", hs(&owner, nil, "claude", []string{"issue", "self_improve"}), base.Add(48*time.Hour))
	eq("owner codex", hs(&owner, nil, "codex", allKinds), base.Add(24*time.Hour))
	eq("all scope sees other user's run", hs(nil, &otherRepo, "claude", []string{"issue"}), base.Add(-240*time.Hour))
	eq("all scope repo2", hs(nil, &repo2, "", allKinds), base.Add(72*time.Hour))
	// Empty scopes are NULL, not an error.
	unknown := uuid.New()
	if got := hs(&owner, nil, "", []string{"job"}); got != nil {
		t.Errorf("owner empty kinds scope: %v", got)
	}
	if got := hs(&owner, &otherRepo, "", allKinds); got != nil {
		t.Errorf("owner foreign repo scope: %v", got)
	}
	if got := hs(nil, &unknown, "", allKinds); got != nil {
		t.Errorf("all unknown repo scope: %v", got)
	}
}
