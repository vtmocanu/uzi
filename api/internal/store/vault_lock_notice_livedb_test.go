package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for the PRD #890 M1 vault-lock-notice queries. sqlc's type
// inference is not Postgres's, so a green `sqlc generate` does not prove any of the
// three queries actually run — this executes each against real Postgres (the
// sqlc-inference gate named in .claude/rules/go.md):
//
//   - ListUsersNeedingVaultLockNotice: a seeded eligible user (vault row +
//     confirmed Slack + a queued run) comes back with the right pending-work counts,
//     exercising the JOIN, the slack-deliverable gate, the pending-work EXISTS pair,
//     and the two closed-int COUNT subqueries.
//   - ClaimVaultLockNotice: the first claim returns the row, an immediate second
//     returns pgx.ErrNoRows (the atomic RETURNING dedup that makes N booting pods
//     send one DM), and once claimed the user drops out of the eligibility list.
//   - ClearVaultLockNotice: the unlock re-arm nulls lock_notified_at so a re-claim
//     once again returns the row — the episode cycle.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.
func TestVaultLockNoticeQueriesLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	// Seed an eligible user: confirmed, opted-in Slack delivery target + a vault row
	// (lock_notified_at defaults NULL) + a queued run (lock-blockable work).
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash, slack_notify, slack_resolved_id, slack_link_confirmed_at)
		 VALUES ($1, $2, 'x', true, $3, now())`,
		userID, fmt.Sprintf("vlt-%s@e2e", userID), "U"+userID.String()[:8])
	mustExec(ctx, t, pool,
		`INSERT INTO user_vaults (user_id, kek_salt, wrapped_dek) VALUES ($1, $2, $3)`,
		userID, []byte("saltsaltsaltsalt"), []byte("wrapped-dek-bytes"))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, "bot-vlt", 8801, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 8801, 'g/vlt', 'https://forge.e2e/g/vlt', true)`,
		repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 4242, 't', 'd', 'queued')`,
		uuid.New(), userID, repoID)

	// (1) The eligible user surfaces with the right pending-work counts.
	rows, err := q.ListUsersNeedingVaultLockNotice(ctx)
	if err != nil {
		t.Fatalf("ListUsersNeedingVaultLockNotice: %v", err)
	}
	got := findNoticeRow(rows, userID)
	if got == nil {
		t.Fatalf("eligible user %s not returned by ListUsersNeedingVaultLockNotice; got %d rows", userID, len(rows))
	}
	if got.PendingRuns != 1 {
		t.Fatalf("pending_runs = %d, want 1 (one queued run)", got.PendingRuns)
	}
	if got.PendingSchedules != 0 {
		t.Fatalf("pending_schedules = %d, want 0 (no active schedule seeded)", got.PendingSchedules)
	}
	if !got.SlackResolvedID.Valid || got.SlackResolvedID.String != "U"+userID.String()[:8] {
		t.Fatalf("slack_resolved_id = %+v, want the seeded resolved id", got.SlackResolvedID)
	}

	// (2) The first claim returns the user's row; a second immediate claim returns
	// pgx.ErrNoRows — the atomic RETURNING dedup.
	claimed, err := q.ClaimVaultLockNotice(ctx, userID)
	if err != nil {
		t.Fatalf("ClaimVaultLockNotice (first): %v", err)
	}
	if claimed != userID {
		t.Fatalf("first claim returned %s, want %s", claimed, userID)
	}
	if _, err := q.ClaimVaultLockNotice(ctx, userID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim err = %v, want pgx.ErrNoRows (already marked)", err)
	}

	// After the claim the user drops out of the eligibility list (lock_notified_at set).
	rows, err = q.ListUsersNeedingVaultLockNotice(ctx)
	if err != nil {
		t.Fatalf("ListUsersNeedingVaultLockNotice (post-claim): %v", err)
	}
	if findNoticeRow(rows, userID) != nil {
		t.Fatalf("claimed user %s still returned by the eligibility list; the mark must exclude it", userID)
	}

	// (3) Clearing re-arms the notice; the user re-claims once more (episode cycle).
	if err := q.ClearVaultLockNotice(ctx, userID); err != nil {
		t.Fatalf("ClearVaultLockNotice: %v", err)
	}
	reclaimed, err := q.ClaimVaultLockNotice(ctx, userID)
	if err != nil {
		t.Fatalf("ClaimVaultLockNotice (after clear): %v", err)
	}
	if reclaimed != userID {
		t.Fatalf("re-claim after clear returned %s, want %s", reclaimed, userID)
	}

	// ClearVaultLockNotice on an already-clear row is a no-op (the IS NOT NULL guard),
	// and errors nowhere.
	if err := q.ClearVaultLockNotice(ctx, uuid.New()); err != nil {
		t.Fatalf("ClearVaultLockNotice on an unmarked/absent user should be a no-op, got: %v", err)
	}
}

func findNoticeRow(rows []store.ListUsersNeedingVaultLockNoticeRow, id uuid.UUID) *store.ListUsersNeedingVaultLockNoticeRow {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}
