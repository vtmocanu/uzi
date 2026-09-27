package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1732 M4, the read surfaces on the real schema: the owner and admin rate-limit reads
// omit disabled credentials (D1, D9), Codex account labels roll up enabled aliases only
// (D6), and a Codex reading from before a disable is not current after the re-enable (D13).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// TestAnthropicRateLimitReadsOmitDisabledTokensLiveDB: the owner read lists enabled tokens
// only, and the admin read gives a disabled token no row; a user whose tokens are all
// disabled folds to the token-less shape (one row, NULL secret) instead of vanishing.
func TestAnthropicRateLimitReadsOmitDisabledTokensLiveDB(t *testing.T) {
	ctx, _, q, user := rateLimitEnablementDB(t)
	def := enablementToken(ctx, t, q, user, "default", true)
	spare := enablementToken(ctx, t, q, user, "spare", false)
	if n := fencedUpsert(ctx, t, q, user, spare, 0, 40); n != 1 {
		t.Fatalf("seed spare reading = %d rows, want 1", n)
	}

	ownerIDs := func() []uuid.UUID {
		t.Helper()
		rows, err := q.ListRateLimitsForUser(ctx, user)
		if err != nil {
			t.Fatalf("ListRateLimitsForUser: %v", err)
		}
		ids := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.UserSecretID)
		}
		return ids
	}
	adminRows := func() []store.ListRateLimitsRow {
		t.Helper()
		rows, err := q.ListRateLimits(ctx)
		if err != nil {
			t.Fatalf("ListRateLimits: %v", err)
		}
		var mine []store.ListRateLimitsRow
		for _, r := range rows {
			if r.UserID == user {
				mine = append(mine, r)
			}
		}
		return mine
	}

	if got := ownerIDs(); len(got) != 2 {
		t.Fatalf("control: owner rows = %v, want both tokens", got)
	}
	if got := adminRows(); len(got) != 2 {
		t.Fatalf("control: admin rows = %d, want 2", len(got))
	}

	setEnabled(ctx, t, q, user, spare, false)
	if got := ownerIDs(); len(got) != 1 || got[0] != def {
		t.Fatalf("owner rows after disabling spare = %v, want only the default %s", got, def)
	}
	if got := adminRows(); len(got) != 1 || !got[0].UserSecretID.Valid || uuid.UUID(got[0].UserSecretID.Bytes) != def {
		t.Fatalf("admin rows after disabling spare = %+v, want only the default", got)
	}

	// The last one too (the default is cleared by the handler; the read does not care).
	setEnabled(ctx, t, q, user, def, false)
	if got := ownerIDs(); len(got) != 0 {
		t.Fatalf("owner rows with every token disabled = %v, want none", got)
	}
	got := adminRows()
	if len(got) != 1 || got[0].UserSecretID.Valid {
		t.Fatalf("admin rows with every token disabled = %+v, want the one token-less row", got)
	}

	// Re-enable: the token is back, and its pre-disable reading is not (M3a's rev match).
	setEnabled(ctx, t, q, user, spare, true)
	rows, err := q.ListRateLimitsForUser(ctx, user)
	if err != nil || len(rows) != 1 || rows[0].UserSecretID != spare || rows[0].SyncedAt.Valid {
		t.Fatalf("owner rows after re-enable = %+v (err %v), want spare with no current reading", rows, err)
	}
}

// codexOwnerRow returns the owner read's row for account, or ok=false when it is not listed.
func codexOwnerRow(ctx context.Context, t *testing.T, q *store.Queries, user, account uuid.UUID) (store.GetCodexAccountRateLimitsForUserRow, bool) {
	t.Helper()
	rows, err := q.GetCodexAccountRateLimitsForUser(ctx, user)
	if err != nil {
		t.Fatalf("GetCodexAccountRateLimitsForUser: %v", err)
	}
	for _, r := range rows {
		if r.ProviderAccountID == account {
			return r, true
		}
	}
	return store.GetCodexAccountRateLimitsForUserRow{}, false
}

// codexAdminRow is codexOwnerRow for the admin read.
func codexAdminRow(ctx context.Context, t *testing.T, q *store.Queries, user, account uuid.UUID) (store.ListCodexAccountRateLimitsRow, bool) {
	t.Helper()
	rows, err := q.ListCodexAccountRateLimits(ctx)
	if err != nil {
		t.Fatalf("ListCodexAccountRateLimits: %v", err)
	}
	for _, r := range rows {
		if r.UserID == user && r.ProviderAccountID == account {
			return r, true
		}
	}
	return store.ListCodexAccountRateLimitsRow{}, false
}

func codexLabel(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var label string
	if err := pool.QueryRow(ctx, `SELECT label FROM user_secrets WHERE id = $1`, id).Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	return label
}

// TestCodexRateLimitReadsUseEnabledAliasesLiveDB (D6, D9): an account's labels and default
// flag roll up its enabled aliases only; an account with no enabled alias has no owner row
// and no admin row; re-enabling one brings it back.
func TestCodexRateLimitReadsUseEnabledAliasesLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	acc, first := mkLinkedCodexAccount(ctx, t, pool, q, user, "first", true)
	second := addLinkedAlias(ctx, t, pool, q, user, acc, "second", false)
	firstLabel, secondLabel := codexLabel(ctx, t, pool, first), codexLabel(ctx, t, pool, second)

	row, ok := codexOwnerRow(ctx, t, q, user, acc)
	if !ok || len(row.Aliases) != 2 || !row.IsDefault {
		t.Fatalf("control: owner row = %+v (listed %v), want both aliases and the default flag", row, ok)
	}

	// Disable the default alias (the handler would hand the default off; here the flag is
	// cleared directly so the read, not the handler, is what is measured).
	mustExec(ctx, t, pool, `UPDATE user_secrets SET is_default = false WHERE id = $1`, first)
	setEnabled(ctx, t, q, user, first, false)
	row, ok = codexOwnerRow(ctx, t, q, user, acc)
	if !ok || len(row.Aliases) != 1 || row.Aliases[0] != secondLabel {
		t.Fatalf("owner row with one alias disabled = %+v (listed %v), want only %q", row, ok, secondLabel)
	}
	admin, ok := codexAdminRow(ctx, t, q, user, acc)
	if !ok || len(admin.Aliases) != 1 || admin.Aliases[0] != secondLabel {
		t.Fatalf("admin row with one alias disabled = %+v (listed %v), want only %q", admin, ok, secondLabel)
	}
	for _, a := range append(row.Aliases, admin.Aliases...) {
		if a == firstLabel {
			t.Fatalf("a disabled alias's label %q names the meter", firstLabel)
		}
	}

	setEnabled(ctx, t, q, user, second, false)
	if row, ok := codexOwnerRow(ctx, t, q, user, acc); ok {
		t.Fatalf("owner read lists an account with no enabled alias: %+v", row)
	}
	if admin, ok := codexAdminRow(ctx, t, q, user, acc); ok {
		t.Fatalf("admin read lists an account with no enabled alias: %+v", admin)
	}

	setEnabled(ctx, t, q, user, second, true)
	if row, ok := codexOwnerRow(ctx, t, q, user, acc); !ok || len(row.Aliases) != 1 || row.Aliases[0] != secondLabel {
		t.Fatalf("owner row after re-enable = %+v (listed %v), want only %q", row, ok, secondLabel)
	}
}

// TestCodexReadingNotCurrentAfterReenableLiveDB (D13, M4): the Codex reading is stamped with
// the enablement list it was polled under. After the account's last enabled alias is
// disabled and re-enabled, the old reading is not current in either read (no buckets, no
// last success, so the meter reads pending), until a fresh poll lands one. A failed first
// poll after the re-enable surfaces as an attempt with no reading instead of staying hidden.
// A sibling disabled while the account stayed live does not hide a current reading, but a
// newly linked enabled alias does until the next successful poll (a failed poll in between
// drops it). A row written before the stamp existed (NULL) stays current while every alias
// is at rev 0, including a newly linked one.
func TestCodexReadingNotCurrentAfterReenableLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	upsert := func(acc uuid.UUID) {
		t.Helper()
		n, err := q.UpsertCodexAccountRateLimits(ctx, store.UpsertCodexAccountRateLimitsParams{
			UserID: user, ProviderAccountID: acc, Buckets: []byte(`[{"id":"primary"}]`), AttemptStatus: "ok",
			EnablementSig: codexEnablementSig(ctx, t, q, user, acc),
		})
		if err != nil || n != 1 {
			t.Fatalf("UpsertCodexAccountRateLimits = (%d, %v), want (1, nil)", n, err)
		}
	}
	fail := func(acc uuid.UUID) {
		t.Helper()
		n, err := q.RecordCodexAccountPollFailure(ctx, store.RecordCodexAccountPollFailureParams{
			UserID: user, ProviderAccountID: acc, AttemptStatus: "transient", AttemptError: "x",
			EnablementSig: codexEnablementSig(ctx, t, q, user, acc),
		})
		if err != nil || n != 1 {
			t.Fatalf("RecordCodexAccountPollFailure = (%d, %v), want (1, nil)", n, err)
		}
	}
	current := func(acc uuid.UUID) (success, attempt bool) {
		t.Helper()
		row, ok := codexOwnerRow(ctx, t, q, user, acc)
		if !ok {
			t.Fatalf("account %s not listed by the owner read", acc)
		}
		admin, ok := codexAdminRow(ctx, t, q, user, acc)
		if !ok {
			t.Fatalf("account %s not listed by the admin read", acc)
		}
		if row.LastSuccessAt.Valid != admin.LastSuccessAt.Valid || row.LastAttemptAt.Valid != admin.LastAttemptAt.Valid ||
			(len(row.Buckets) > 0) != (len(admin.Buckets) > 0) {
			t.Fatalf("owner and admin reads disagree: owner %+v admin %+v", row, admin)
		}
		if row.LastSuccessAt.Valid != (len(row.Buckets) > 0) {
			t.Fatalf("reading half-hidden: last_success_at %v, buckets %q", row.LastSuccessAt, row.Buckets)
		}
		return row.LastSuccessAt.Valid, row.LastAttemptAt.Valid
	}

	// --- the only alias: disable and re-enable hides the pre-disable reading ---
	solo, alias := mkLinkedCodexAccount(ctx, t, pool, q, user, "solo", false)
	upsert(solo)
	if s, _ := current(solo); !s {
		t.Fatal("control: a fresh reading is not current")
	}
	setEnabled(ctx, t, q, user, alias, false)
	setEnabled(ctx, t, q, user, alias, true)
	if s, a := current(solo); s || a {
		t.Fatalf("pre-disable reading after re-enable: success=%v attempt=%v, want neither (pending)", s, a)
	}
	fail(solo)
	if s, a := current(solo); s || !a {
		t.Fatalf("after a failed post-re-enable poll: success=%v attempt=%v, want an attempt and no reading", s, a)
	}
	var buckets []byte
	if err := pool.QueryRow(ctx, `SELECT buckets FROM codex_account_rate_limits WHERE user_id = $1 AND provider_account_id = $2`,
		user, solo).Scan(&buckets); err != nil || buckets != nil {
		t.Fatalf("stale reading kept after the failure re-stamp: buckets %q (err %v), want NULL", buckets, err)
	}
	upsert(solo)
	if s, _ := current(solo); !s {
		t.Fatal("a reading polled after the re-enable is not current")
	}

	// --- an enabled sibling keeps the account live: its reading stays current ---
	pair, a := mkLinkedCodexAccount(ctx, t, pool, q, user, "pair-a", false)
	addLinkedAlias(ctx, t, pool, q, user, pair, "pair-b", false)
	upsert(pair)
	setEnabled(ctx, t, q, user, a, false)
	if s, _ := current(pair); !s {
		t.Fatal("a sibling disable hid the reading of an account that stayed live")
	}
	// A failure then keeps that still-current reading (the documented failure contract).
	fail(pair)
	if s, at := current(pair); !s || !at {
		t.Fatalf("failure over a current reading: success=%v attempt=%v, want both", s, at)
	}
	// Re-enabling the sibling moves its revision past the stamp: not current until repolled.
	setEnabled(ctx, t, q, user, a, true)
	if s, _ := current(pair); s {
		t.Fatal("the reading is current across the sibling's re-enable")
	}

	// --- a newly linked enabled alias (conservative, N1): its (id, rev) is not in the stamp,
	// so the reading is not current until the next successful poll; a failed poll in that
	// window drops it, and a successful poll restores it ---
	linked, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "linked-a", false)
	upsert(linked)
	if s, _ := current(linked); !s {
		t.Fatal("control: a fresh reading is not current")
	}
	addLinkedAlias(ctx, t, pool, q, user, linked, "linked-b", false)
	if s, _ := current(linked); s {
		t.Fatal("the reading is current across a newly linked enabled alias")
	}
	fail(linked)
	if s, at := current(linked); s || !at {
		t.Fatalf("after a failed poll following the new link: success=%v attempt=%v, want an attempt and no reading", s, at)
	}
	var linkedBuckets []byte
	if err := pool.QueryRow(ctx, `SELECT buckets FROM codex_account_rate_limits WHERE user_id = $1 AND provider_account_id = $2`,
		user, linked).Scan(&linkedBuckets); err != nil || linkedBuckets != nil {
		t.Fatalf("stale reading kept after the new-link failure: buckets %q (err %v), want NULL", linkedBuckets, err)
	}
	upsert(linked)
	if s, _ := current(linked); !s {
		t.Fatal("a reading polled after the new link is not current")
	}

	// --- a row stamped before the column existed (NULL) ---
	legacy, legacyAlias := mkLinkedCodexAccount(ctx, t, pool, q, user, "legacy", false)
	mustExec(ctx, t, pool, `INSERT INTO codex_account_rate_limits
		(user_id, provider_account_id, buckets, observed_generation, observed_credential_revision,
		 last_success_at, last_attempt_at, attempt_status)
		VALUES ($1, $2, '[{"id":"primary"}]'::jsonb, 0, 0, now(), now(), 'ok')`, user, legacy)
	if s, _ := current(legacy); !s {
		t.Fatal("a pre-column (NULL-stamped) reading at revision 0 is not current")
	}
	// Unlike a stamped row, a NULL row tolerates a newly linked revision-0 alias.
	addLinkedAlias(ctx, t, pool, q, user, legacy, "legacy-b", false)
	if s, _ := current(legacy); !s {
		t.Fatal("a NULL-stamped reading is hidden by a newly linked revision-0 alias")
	}
	setEnabled(ctx, t, q, user, legacyAlias, false)
	setEnabled(ctx, t, q, user, legacyAlias, true)
	if s, _ := current(legacy); s {
		t.Fatal("a NULL-stamped reading is current after its alias's disable and re-enable")
	}
}
