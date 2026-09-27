package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// rateLimitEnablementDB opens the live DB for the PRD #1732 M3a polling-fence tests
// and returns one fresh user.
func rateLimitEnablementDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries, uuid.UUID) {
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
	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user, fmt.Sprintf("rl-enable-%s@e2e", user))
	return ctx, pool, store.New(pool), user
}

func enablementToken(ctx context.Context, t *testing.T, q *store.Queries, user uuid.UUID, label string, isDefault bool) uuid.UUID {
	t.Helper()
	row, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
		Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert secret %q: %v", label, err)
	}
	return row.ID
}

func setEnabled(ctx context.Context, t *testing.T, q *store.Queries, user, id uuid.UUID, enabled bool) int64 {
	t.Helper()
	row, err := q.SetSecretEnablement(ctx, store.SetSecretEnablementParams{ID: id, UserID: user, Enabled: enabled})
	if err != nil {
		t.Fatalf("set enabled=%v: %v", enabled, err)
	}
	return row.EnablementRev
}

func fencedUpsert(ctx context.Context, t *testing.T, q *store.Queries, user, id uuid.UUID, rev int64, pct int16) int64 {
	t.Helper()
	n, err := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
		UserSecretID: id, UserID: user, EnablementRev: rev,
		FiveHourPct: pgtype.Int2{Int16: pct, Valid: true},
		SevenDayPct: pgtype.Int2{Int16: pct, Valid: true},
		Source:      pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		t.Fatalf("upsert rev %d: %v", rev, err)
	}
	return n
}

// TestPollListingSkipsDisabledTokensLiveDB: neither the tick's listing nor the
// poke's resolve returns a disabled token (D1), and both carry the revision the
// poll is fenced on (D13).
func TestPollListingSkipsDisabledTokensLiveDB(t *testing.T) {
	ctx, _, q, user := rateLimitEnablementDB(t)
	def := enablementToken(ctx, t, q, user, "default", true)
	spare := enablementToken(ctx, t, q, user, "spare", false)
	rev := setEnabled(ctx, t, q, user, spare, false)

	rows, err := q.ListAnthropicTokensToPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed []uuid.UUID
	for _, r := range rows {
		if r.UserID == user {
			listed = append(listed, r.ID)
		}
	}
	if len(listed) != 1 || listed[0] != def {
		t.Fatalf("tick listing = %v, want only the enabled default %s", listed, def)
	}

	byID := func(id uuid.UUID) (store.GetAnthropicTokenToPollRow, error) {
		return q.GetAnthropicTokenToPoll(ctx, store.GetAnthropicTokenToPollParams{UserID: user, SecretID: pgtype.UUID{Bytes: id, Valid: true}})
	}
	if _, err := byID(spare); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("poke resolved a disabled token: err=%v", err)
	}
	got, err := q.GetAnthropicTokenToPoll(ctx, store.GetAnthropicTokenToPollParams{UserID: user})
	if err != nil || got.ID != def || string(got.Ciphertext) != "ct-default" {
		t.Fatalf("default poke = %+v err=%v, want the default %s", got, err, def)
	}
	if _, err := q.GetAnthropicTokenToPoll(ctx, store.GetAnthropicTokenToPollParams{UserID: uuid.New(), SecretID: pgtype.UUID{Bytes: def, Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("poke resolved another owner's token: err=%v", err)
	}

	if back := setEnabled(ctx, t, q, user, spare, true); back != rev+1 {
		t.Fatalf("re-enable rev = %d, want %d", back, rev+1)
	}
	got, err = byID(spare)
	if err != nil || got.ID != spare || got.EnablementRev != rev+1 {
		t.Fatalf("re-enabled poke = %+v err=%v, want %s at rev %d", got, err, spare, rev+1)
	}
	rows, err = q.ListAnthropicTokensToPoll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == spare && r.EnablementRev != rev+1 {
			t.Fatalf("listing rev = %d, want %d", r.EnablementRev, rev+1)
		}
	}
}

// TestRateLimitRevisionFenceLiveDB: the D13 fence on the reading upsert. A poll
// that started before a disable writes nothing while disabled, and nothing after
// the re-enable either; the pre-disable reading it would have replaced is hidden
// from the owner and admin reads and from the early-reset prev read until a poll
// at the new revision lands.
func TestRateLimitRevisionFenceLiveDB(t *testing.T) {
	ctx, _, q, user := rateLimitEnablementDB(t)
	tok := enablementToken(ctx, t, q, user, "default", true)

	if n := fencedUpsert(ctx, t, q, user, tok, 0, 40); n != 1 {
		t.Fatalf("enabled current-rev write = %d rows, want 1", n)
	}
	if n := fencedUpsert(ctx, t, q, uuid.New(), tok, 0, 41); n != 0 {
		t.Fatalf("write under another owner = %d rows, want 0", n)
	}
	disabledRev := setEnabled(ctx, t, q, user, tok, false)
	if n := fencedUpsert(ctx, t, q, user, tok, 0, 50); n != 0 {
		t.Fatalf("write started before the disable = %d rows, want 0", n)
	}
	if n := fencedUpsert(ctx, t, q, user, tok, disabledRev, 51); n != 0 {
		t.Fatalf("write at the disabled revision = %d rows, want 0", n)
	}
	enabledRev := setEnabled(ctx, t, q, user, tok, true)
	if n := fencedUpsert(ctx, t, q, user, tok, 0, 60); n != 0 {
		t.Fatalf("write started before the disable, finished after the re-enable = %d rows, want 0", n)
	}

	// The stored row is still the rev-0 reading, and nothing reads it as current.
	row, err := q.GetRateLimitsForToken(ctx, store.GetRateLimitsForTokenParams{UserSecretID: tok, EnablementRev: 0})
	if err != nil {
		t.Fatalf("rev-0 row: %v", err)
	}
	pct, stamped := row.FiveHourPct.Int16, row.EnablementRev
	if pct != 40 || stamped != 0 {
		t.Fatalf("stored row = pct %d rev %d, want the pre-disable 40 at rev 0 (no fenced write landed)", pct, stamped)
	}
	if _, err := q.GetRateLimitsForToken(ctx, store.GetRateLimitsForTokenParams{UserSecretID: tok, EnablementRev: enabledRev}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("prev at the re-enabled revision: err=%v, want no row (a pre-disable reading is no alert basis)", err)
	}
	own, err := q.ListRateLimitsForUser(ctx, user)
	if err != nil || len(own) != 1 {
		t.Fatalf("owner read = %+v err=%v", own, err)
	}
	if own[0].SyncedAt.Valid {
		t.Fatalf("owner read shows the pre-disable reading as current: %+v", own[0])
	}
	admin, err := q.ListRateLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range admin {
		if r.UserSecretID.Valid && r.UserSecretID.Bytes == tok && r.SyncedAt.Valid {
			t.Fatalf("admin read shows the pre-disable reading as current: %+v", r)
		}
	}

	// A poll that started after the re-enable writes, and becomes current.
	if n := fencedUpsert(ctx, t, q, user, tok, enabledRev, 70); n != 1 {
		t.Fatalf("write at the re-enabled revision = %d rows, want 1", n)
	}
	own, err = q.ListRateLimitsForUser(ctx, user)
	if err != nil || len(own) != 1 || !own[0].SyncedAt.Valid || own[0].FiveHourPct.Int16 != 70 {
		t.Fatalf("owner read after the fresh poll = %+v err=%v, want the rev-%d reading", own, err, enabledRev)
	}
}

// TestAutoSelectionIgnoresPreDisableReadingLiveDB: after a re-enable, auto-selection
// does not consume the token's pre-disable reading (D13). The re-enabled token's old
// reading shows far more headroom than its sibling's, so selecting on it would pick
// it; hidden, the token classifies no_reading and the sibling is picked.
func TestAutoSelectionIgnoresPreDisableReadingLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	stale := enablementToken(ctx, t, q, user, "stale", true)
	fresh := enablementToken(ctx, t, q, user, "fresh", false)
	mustExec(ctx, t, pool, `UPDATE user_secrets SET auto_eligible = true WHERE user_id = $1`, user)
	if n := fencedUpsert(ctx, t, q, user, stale, 0, 1); n != 1 {
		t.Fatalf("stale seed = %d rows", n)
	}
	if n := fencedUpsert(ctx, t, q, user, fresh, 0, 50); n != 1 {
		t.Fatalf("fresh seed = %d rows", n)
	}
	policy := autoselect.Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute}
	pick := func() (autoselect.Outcome, map[uuid.UUID]autoselect.Status) {
		rows, err := q.ListAutoSelectCandidates(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		cands := make([]autoselect.Candidate, 0, len(rows))
		status := map[uuid.UUID]autoselect.Status{}
		for _, r := range rows {
			c := autoselectrow.FromCandidateRow(r)
			cands = append(cands, c)
			status[c.SecretID] = autoselect.Classify(c, policy, time.Now()).Status
		}
		return autoselect.Select(cands, uuid.Nil, policy, time.Now()), status
	}
	if out, _ := pick(); !out.Picked || out.SecretID != stale {
		t.Fatalf("control: pick = %+v, want the higher-headroom %s", out, stale)
	}

	setEnabled(ctx, t, q, user, stale, false)
	setEnabled(ctx, t, q, user, stale, true)

	out, status := pick()
	if status[stale] != autoselect.StatusNoReading {
		t.Fatalf("re-enabled token classifies %q, want %q (its reading is pre-disable)", status[stale], autoselect.StatusNoReading)
	}
	if !out.Picked || out.SecretID != fresh {
		t.Fatalf("pick after re-enable = %+v, want %s (the pre-disable reading must not be consumed)", out, fresh)
	}
}

// TestRateLimitFenceSerializesWithTransitionLiveDB: the fence is not a stale snapshot
// read. A write whose poll started before a disable, and which reaches the database
// while the disable's transaction holds the credential row, waits for it and then
// writes nothing, instead of landing the old revision's reading behind its back.
func TestRateLimitFenceSerializesWithTransitionLiveDB(t *testing.T) {
	ctx, pool, q, user := rateLimitEnablementDB(t)
	tok := enablementToken(ctx, t, q, user, "default", true)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := q.WithTx(tx)
	holder := backendPID(ctx, t, tx)
	// The enablement handler's shape: lock the row, then transition it.
	if _, err := tq.GetUserSecretForUpdate(ctx, store.GetUserSecretForUpdateParams{ID: tok, UserID: user}); err != nil {
		t.Fatal(err)
	}
	if _, err := tq.SetSecretEnablement(ctx, store.SetSecretEnablementParams{ID: tok, UserID: user, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	done := make(chan int64, 1)
	go func() {
		n, uerr := q.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
			UserSecretID: tok, UserID: user, EnablementRev: 0,
			FiveHourPct: pgtype.Int2{Int16: 9, Valid: true},
			SevenDayPct: pgtype.Int2{Int16: 9, Valid: true},
			SyncedAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		})
		if uerr != nil {
			n = -1
		}
		done <- n
	}()
	// Wait until the write is blocked on THIS transition's row lock.
	waitBlockedBy(ctx, t, pool, holder, "INSERT INTO anthropic_rate_limits", done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := <-done; n != 0 {
		t.Fatalf("write racing the disable = %d rows, want 0", n)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM anthropic_rate_limits WHERE user_secret_id = $1`, tok).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a reading landed behind the disable: %d rows", rows)
	}
}

// backendPID is the server process id of tx's connection, the identity a lock probe
// scopes to.
func backendPID(ctx context.Context, t *testing.T, tx pgx.Tx) int32 {
	t.Helper()
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	return pid
}

// waitBlockedBy waits until a statement containing queryFragment is blocked by the
// backend holder (pg_blocking_pids), so the probe sees only this test's own waiter
// and never a concurrent test's statement on a shared database. A result on done
// before that means the statement finished without waiting, which fails the test.
func waitBlockedBy(ctx context.Context, t *testing.T, pool *pgxpool.Pool, holder int32, queryFragment string, done <-chan int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE $1::int = ANY(pg_blocking_pids(pid)) AND strpos(query, $2) > 0`, holder, queryFragment).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case n := <-done:
			t.Fatalf("the fenced write did not wait for the transition: wrote %d rows", n)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the fenced write never blocked on the transition")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
