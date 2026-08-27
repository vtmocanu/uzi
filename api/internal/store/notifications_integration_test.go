package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestNotificationsPruneLiveDB exercises PruneNotificationsForUser against a REAL
// Postgres (PRD #46 M6 carry-forward). The M2 unit tests only pin the call-level
// ordering (notifysvc prunes after inserting); they can't prove the SQL actually
// deletes the right rows. This does: under-cap = no deletion, over-cap = keep exactly
// the newest cap, the created_at-tie boundary residual (the prune subquery has no id
// tiebreaker and deletes with a strict `<`, so rows tied at the boundary's created_at
// are ALL kept — a small keep-slightly-more, documented in queries/notifications.sql),
// per-user isolation, and an InsertNotification→Prune→Count round-trip proving rows
// are genuinely removed through the generated write seam.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// e2e/run-store-it.sh).
func TestNotificationsPruneLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	newUser := func() uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			id, fmt.Sprintf("prune-%s@e2e", id))
		return id
	}
	// insAt inserts a notification for u with an EXACT created_at, so ties are exact
	// (separate now() calls would differ by microseconds and never tie).
	insAt := func(u uuid.UUID, at time.Time) {
		mustExec(ctx, t, pool,
			`INSERT INTO notifications (user_id, kind, payload, created_at) VALUES ($1, 'judge_review', '{}', $2)`,
			u, at)
	}
	countFor := func(u uuid.UUID) int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1`, u).Scan(&n); err != nil {
			t.Fatalf("count for %v: %v", u, err)
		}
		return n
	}
	prune := func(u uuid.UUID, keep int32) int64 {
		n, err := q.PruneNotificationsForUser(ctx, store.PruneNotificationsForUserParams{UserID: u, Keep: keep})
		if err != nil {
			t.Fatalf("PruneNotificationsForUser: %v", err)
		}
		return n
	}
	// A fixed clock so every row's created_at is deterministic and orderable.
	t0 := time.Now().UTC()
	ago := func(secs int) time.Time { return t0.Add(-time.Duration(secs) * time.Second) }

	t.Run("under cap keeps everything (no deletion at or below cap)", func(t *testing.T) {
		u := newUser()
		insAt(u, ago(30))
		insAt(u, ago(20))
		insAt(u, ago(10))
		if got := prune(u, 5); got != 0 {
			t.Errorf("deleted %d, want 0 (3 rows, cap 5)", got)
		}
		if got := countFor(u); got != 3 {
			t.Errorf("remaining %d, want 3", got)
		}
		// Exactly-at-cap also deletes nothing.
		if got := prune(u, 3); got != 0 {
			t.Errorf("deleted %d at exactly cap, want 0", got)
		}
		if got := countFor(u); got != 3 {
			t.Errorf("remaining %d after at-cap prune, want 3", got)
		}
	})

	t.Run("over cap keeps exactly the newest cap", func(t *testing.T) {
		u := newUser()
		for _, s := range []int{50, 40, 30, 20, 10} { // 5 distinct ages
			insAt(u, ago(s))
		}
		if got := prune(u, 3); got != 2 {
			t.Errorf("deleted %d, want 2 (5 rows, cap 3)", got)
		}
		if got := countFor(u); got != 3 {
			t.Errorf("remaining %d, want 3", got)
		}
		// The kept rows are the NEWEST 3 (10/20/30s ago); the two oldest (40/50s) are
		// gone — no remaining row is older than the 35s boundary between them.
		var older int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM notifications WHERE user_id = $1 AND created_at < $2`, u, ago(35)).Scan(&older); err != nil {
			t.Fatalf("count older: %v", err)
		}
		if older != 0 {
			t.Errorf("%d rows older than the newest-3 boundary survived; the two oldest should be pruned", older)
		}
	})

	t.Run("boundary tie keeps all rows sharing the boundary created_at", func(t *testing.T) {
		u := newUser()
		boundary := ago(20)
		insAt(u, ago(10))  // newest
		insAt(u, boundary) // tie A (the cap-th newest)
		insAt(u, boundary) // tie B (same created_at — no id tiebreaker in the prune subquery)
		insAt(u, ago(30))  // oldest
		// Keep=2: the newest 2 are {10s, one of the two 20s rows}; the boundary is 20s
		// ago, and the DELETE is strict `<` boundary, so BOTH 20s rows survive. Only the
		// 30s row is deleted → 3 remain, one MORE than the cap (the documented residual).
		if got := prune(u, 2); got != 1 {
			t.Errorf("deleted %d, want 1 (only the row strictly older than the tied boundary)", got)
		}
		if got := countFor(u); got != 3 {
			t.Errorf("remaining %d, want 3 (cap 2 + 1 tied-at-boundary residual)", got)
		}
	})

	t.Run("prune is scoped per user", func(t *testing.T) {
		uA, uB := newUser(), newUser()
		for _, s := range []int{50, 40, 30, 20, 10} {
			insAt(uA, ago(s))
		}
		for _, s := range []int{40, 30, 20, 10} {
			insAt(uB, ago(s))
		}
		if got := prune(uA, 2); got != 3 {
			t.Errorf("deleted %d from A, want 3", got)
		}
		if got := countFor(uA); got != 2 {
			t.Errorf("A remaining %d, want 2", got)
		}
		if got := countFor(uB); got != 4 {
			t.Errorf("B remaining %d, want 4 (untouched by A's prune)", got)
		}
	})

	t.Run("InsertNotification then Prune actually deletes rows (write-seam round-trip)", func(t *testing.T) {
		u := newUser()
		for i := 0; i < 4; i++ {
			if _, err := q.InsertNotification(ctx, store.InsertNotificationParams{
				UserID: u, Kind: "judge_review", Payload: []byte(`{}`),
			}); err != nil {
				t.Fatalf("InsertNotification: %v", err)
			}
			time.Sleep(2 * time.Millisecond) // distinct created_at so the cap is unambiguous
		}
		if got, err := q.CountNotificationsForUser(ctx, u); err != nil || got != 4 {
			t.Fatalf("CountNotificationsForUser = %d, %v; want 4 inserted", got, err)
		}
		if got := prune(u, 2); got != 2 {
			t.Errorf("deleted %d, want 2", got)
		}
		if got, err := q.CountNotificationsForUser(ctx, u); err != nil || got != 2 {
			t.Fatalf("CountNotificationsForUser = %d, %v; want 2 after prune (rows genuinely removed)", got, err)
		}
	})
}
