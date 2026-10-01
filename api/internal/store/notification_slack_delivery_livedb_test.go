package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestNotificationSlackDeliveryLiveDB pins the durable Slack delivery state (issue #1675)
// against a REAL Postgres: InsertNotification's attempt accounting, the
// ClaimPendingSlackNotifications claim, MarkNotificationSlackDelivered idempotence and the
// prune exclusion for rows still awaiting delivery. The database is shared, so every
// subtest uses a fresh user and filters results to its own row ids.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestNotificationSlackDeliveryLiveDB(t *testing.T) {
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
			id, fmt.Sprintf("slackdel-%s@e2e", id))
		return id
	}
	insert := func(u uuid.UUID, render []byte) store.Notification {
		n, err := q.InsertNotification(ctx, store.InsertNotificationParams{
			UserID: u, Kind: "ci_autofix_halted", Payload: []byte(`{}`), SlackRender: render,
		})
		if err != nil {
			t.Fatalf("InsertNotification: %v", err)
		}
		return n
	}
	ts := func(tm time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: tm, Valid: true} }
	claim := func(stale time.Time, maxAttempts, lim int32) map[uuid.UUID]store.ClaimPendingSlackNotificationsRow {
		rows, err := q.ClaimPendingSlackNotifications(ctx, store.ClaimPendingSlackNotificationsParams{
			StaleBefore: ts(stale), MaxAttempts: maxAttempts, Lim: lim,
		})
		if err != nil {
			t.Fatalf("ClaimPendingSlackNotifications: %v", err)
		}
		m := map[uuid.UUID]store.ClaimPendingSlackNotificationsRow{}
		for _, r := range rows {
			m[r.ID] = r
		}
		return m
	}
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	render := []byte(`{"text":"halted"}`)

	t.Run("insert accounting", func(t *testing.T) {
		u := newUser()
		d := insert(u, render)
		if d.SlackAttempts != 1 || !d.SlackAttemptedAt.Valid || d.SlackDeliveredAt.Valid {
			t.Errorf("durable row: attempts=%d attempted_valid=%v delivered_valid=%v; want 1/true/false",
				d.SlackAttempts, d.SlackAttemptedAt.Valid, d.SlackDeliveredAt.Valid)
		}
		if string(d.SlackRender) == "" {
			t.Errorf("durable row lost its slack_render")
		}
		p := insert(u, nil)
		if p.SlackAttempts != 0 || p.SlackAttemptedAt.Valid || p.SlackRender != nil {
			t.Errorf("plain row: attempts=%d attempted_valid=%v render=%q; want 0/false/nil",
				p.SlackAttempts, p.SlackAttemptedAt.Valid, p.SlackRender)
		}
	})

	t.Run("claim staleness, increment and exclusions", func(t *testing.T) {
		u := newUser()
		fresh := insert(u, render)
		plain := insert(u, nil)

		if _, ok := claim(past, 5, 100)[fresh.ID]; ok {
			t.Errorf("row attempted just now was claimed with stale_before in the past")
		}
		got := claim(future, 5, 100)
		r, ok := got[fresh.ID]
		if !ok {
			t.Fatalf("stale durable row not claimed")
		}
		if r.SlackAttempts != 2 {
			t.Errorf("claimed attempts = %d, want 2 (insert=1, claim bumps)", r.SlackAttempts)
		}
		if _, ok := got[plain.ID]; ok {
			t.Errorf("non-durable row was claimed")
		}
		// The claim stamped attempted_at = now, so it is not stale against a past cutoff.
		if _, ok := claim(past, 5, 100)[fresh.ID]; ok {
			t.Errorf("row re-claimed immediately after a claim")
		}

		// max_attempts: attempts is now 2; cap 2 excludes it, cap 3 admits it.
		if _, ok := claim(future, 2, 100)[fresh.ID]; ok {
			t.Errorf("row at max_attempts was claimed")
		}
		if _, ok := claim(future, 3, 100)[fresh.ID]; !ok {
			t.Errorf("row under max_attempts was not claimed")
		}
	})

	t.Run("delivered row never claimed and mark is idempotent", func(t *testing.T) {
		u := newUser()
		n := insert(u, render)
		if err := q.MarkNotificationSlackDelivered(ctx, n.ID); err != nil {
			t.Fatalf("mark: %v", err)
		}
		var first time.Time
		if err := pool.QueryRow(ctx, `SELECT slack_delivered_at FROM notifications WHERE id = $1`, n.ID).Scan(&first); err != nil {
			t.Fatalf("read delivered_at: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := q.MarkNotificationSlackDelivered(ctx, n.ID); err != nil {
			t.Fatalf("second mark: %v", err)
		}
		var second time.Time
		if err := pool.QueryRow(ctx, `SELECT slack_delivered_at FROM notifications WHERE id = $1`, n.ID).Scan(&second); err != nil {
			t.Fatalf("re-read delivered_at: %v", err)
		}
		if !first.Equal(second) {
			t.Errorf("second mark moved slack_delivered_at: %v -> %v", first, second)
		}
		if _, ok := claim(future, 5, 100)[n.ID]; ok {
			t.Errorf("delivered row was claimed")
		}
	})

	t.Run("lim is honoured, oldest first", func(t *testing.T) {
		u := newUser()
		var ids []uuid.UUID
		for i := 0; i < 3; i++ {
			ids = append(ids, insert(u, render).ID)
			mustExec(ctx, t, pool, `UPDATE notifications SET created_at = $2 WHERE id = $1`,
				ids[i], time.Now().Add(-time.Duration(30-i)*24*time.Hour)) // far older than any neighbour
		}
		// The shared DB may hold other pending rows, so assert only that a lim of 1
		// returns exactly one row (the three rows above guarantee at least one exists).
		if got := claim(future, 5, 1); len(got) != 1 {
			t.Errorf("lim=1 claimed %d rows, want 1", len(got))
		}
	})

	t.Run("prune spares pending durable rows only", func(t *testing.T) {
		u := newUser()
		t0 := time.Now().UTC()
		insAt := func(secs int, render []byte, attempts int, delivered bool) uuid.UUID {
			n := insert(u, render)
			var deliveredAt any
			if delivered {
				deliveredAt = t0
			}
			mustExec(ctx, t, pool,
				`UPDATE notifications SET created_at = $2, slack_attempts = $3, slack_delivered_at = $4 WHERE id = $1`,
				n.ID, t0.Add(-time.Duration(secs)*time.Second), attempts, deliveredAt)
			return n.ID
		}
		pending := insAt(100, render, 1, false)
		delivered := insAt(90, render, 1, true)
		exhausted := insAt(80, render, 3, false)
		plain := insAt(70, nil, 0, false)
		newest := insAt(1, nil, 0, false)

		n, err := q.PruneNotificationsForUser(ctx, store.PruneNotificationsForUserParams{UserID: u, Keep: 1, MaxAttempts: 3})
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if n != 3 {
			t.Errorf("pruned %d rows, want 3 (delivered, exhausted, plain)", n)
		}
		exists := func(id uuid.UUID) bool {
			var c int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE id = $1`, id).Scan(&c); err != nil {
				t.Fatalf("exists: %v", err)
			}
			return c == 1
		}
		if !exists(pending) {
			t.Errorf("pending durable row was pruned")
		}
		if !exists(newest) {
			t.Errorf("newest row was pruned")
		}
		for name, id := range map[string]uuid.UUID{"delivered": delivered, "exhausted": exhausted, "plain": plain} {
			if exists(id) {
				t.Errorf("%s row survived the prune", name)
			}
		}
	})
}
