package store_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestIssueTrackerConnectionLiveDB covers the issue_tracker_connections table and its queries
// (PRD #4173): upsert rotates in place, reads and deletes are owner-scoped, the unique key and the
// https CHECK hold, and deleting the user cascades.
func TestIssueTrackerConnectionLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "issue_tracker_conn_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)

	owner, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, other} {
		mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", id, id.String()+"@example.com")
	}
	params := func(user uuid.UUID, base string, ct string) store.UpsertIssueTrackerConnectionParams {
		return store.UpsertIssueTrackerConnectionParams{
			UserID: user, Kind: "youtrack", BaseUrl: base,
			AccountID: "1-1", AccountLogin: "bot", TokenCiphertext: []byte(ct),
		}
	}

	first, err := q.UpsertIssueTrackerConnection(ctx, params(owner, "https://issues.example.com", "cipher-one"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	t.Run("rotate keeps the row id and replaces the ciphertext", func(t *testing.T) {
		p := params(owner, "https://issues.example.com", "cipher-two")
		p.AccountLogin = "renamed"
		second, err := q.UpsertIssueTrackerConnection(ctx, p)
		if err != nil {
			t.Fatalf("upsert again: %v", err)
		}
		if second.ID != first.ID {
			t.Errorf("rotate changed the row id: %s -> %s", first.ID, second.ID)
		}
		if !bytes.Equal(second.TokenCiphertext, []byte("cipher-two")) || second.AccountLogin != "renamed" {
			t.Errorf("rotate did not update the row: %+v", second)
		}
		if !second.CreatedAt.Time.Equal(first.CreatedAt.Time) {
			t.Error("rotate changed created_at")
		}
		if second.LastVerifiedAt.Time.Before(first.LastVerifiedAt.Time) {
			t.Error("rotate moved last_verified_at backwards")
		}
		list, err := q.ListIssueTrackerConnectionsByUser(ctx, owner)
		if err != nil || len(list) != 1 {
			t.Errorf("list = %d rows, %v; want exactly one", len(list), err)
		}
	})

	t.Run("a different base URL is a second connection", func(t *testing.T) {
		second, err := q.UpsertIssueTrackerConnection(ctx, params(owner, "https://other.example.com", "c"))
		if err != nil {
			t.Fatal(err)
		}
		if second.ID == first.ID {
			t.Error("distinct base URL reused the row")
		}
		list, err := q.ListIssueTrackerConnectionsByUser(ctx, owner)
		if err != nil || len(list) != 2 || list[0].ID != first.ID {
			t.Errorf("list = %d rows, %v; want two ordered by creation", len(list), err)
		}
	})

	t.Run("reads and deletes are owner scoped", func(t *testing.T) {
		if _, err := q.GetIssueTrackerConnectionForUser(ctx, store.GetIssueTrackerConnectionForUserParams{ID: first.ID, UserID: other}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("get as another user = %v, want ErrNoRows", err)
		}
		n, err := q.DeleteIssueTrackerConnectionForUser(ctx, store.DeleteIssueTrackerConnectionForUserParams{ID: first.ID, UserID: other})
		if err != nil || n != 0 {
			t.Errorf("delete as another user = %d, %v; want 0 rows", n, err)
		}
		if _, err := q.GetIssueTrackerConnectionForUser(ctx, store.GetIssueTrackerConnectionForUserParams{ID: first.ID, UserID: owner}); err != nil {
			t.Errorf("owner lost the row after a foreign delete: %v", err)
		}
		list, err := q.ListIssueTrackerConnectionsByUser(ctx, other)
		if err != nil || len(list) != 0 {
			t.Errorf("another user lists %d rows, %v", len(list), err)
		}
	})

	t.Run("the same target for another user is its own row", func(t *testing.T) {
		row, err := q.UpsertIssueTrackerConnection(ctx, params(other, "https://issues.example.com", "c"))
		if err != nil || row.ID == first.ID {
			t.Errorf("upsert for another user = %v, %v", row.ID, err)
		}
	})

	t.Run("the unique key is enforced", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO issue_tracker_connections(user_id,kind,base_url,account_id,account_login,token_ciphertext) VALUES($1,'youtrack','https://issues.example.com','a','b','c')`, owner)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Errorf("duplicate insert = %v, want unique violation", err)
		}
	})

	t.Run("the https CHECK refuses other schemes", func(t *testing.T) {
		for _, base := range []string{"http://issues.example.com", "ftp://x", "issues.example.com"} {
			_, err := q.UpsertIssueTrackerConnection(ctx, params(owner, base, "c"))
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Errorf("base_url %q = %v, want check violation", base, err)
			}
		}
	})

	t.Run("the kind CHECK refuses unknown kinds", func(t *testing.T) {
		p := params(owner, "https://k.example.com", "c")
		p.Kind = "jira"
		_, err := q.UpsertIssueTrackerConnection(ctx, p)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("kind jira = %v, want check violation", err)
		}
	})

	t.Run("owner delete removes the row", func(t *testing.T) {
		n, err := q.DeleteIssueTrackerConnectionForUser(ctx, store.DeleteIssueTrackerConnectionForUserParams{ID: first.ID, UserID: owner})
		if err != nil || n != 1 {
			t.Errorf("delete = %d, %v; want 1", n, err)
		}
	})

	t.Run("deleting the user cascades", func(t *testing.T) {
		mustExec(ctx, t, pool, "DELETE FROM users WHERE id=$1", other)
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM issue_tracker_connections WHERE user_id=$1", other).Scan(&n); err != nil || n != 0 {
			t.Errorf("rows after user delete = %d, %v; want 0", n, err)
		}
	})
}
