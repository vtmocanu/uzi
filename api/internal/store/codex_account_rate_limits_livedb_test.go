package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// mkLinkedCodexAccount inserts a provider account owned by `user` plus a codex_auth alias
// linked to it, and returns the account id and the alias (secret) id. `isDefault` sets the
// alias's is_default flag (the codex default slot is shared, so at most one per user).
func mkLinkedCodexAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, user uuid.UUID, label string, isDefault bool) (accountID, secretID uuid.UUID) {
	t.Helper()
	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID:             user,
		ProviderUserID:     "provider-" + uuid.NewString(),
		WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin:        []byte("sealed-login"),
		SealedWith:         store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return acc.ID, addLinkedAlias(ctx, t, pool, q, user, acc.ID, label, isDefault)
}

// addLinkedAlias inserts a codex_auth alias for `user` linked to an existing `account`,
// returning the alias id. Lets a test point several aliases at one account (the dedup case).
func addLinkedAlias(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, user, account uuid.UUID, label string, isDefault bool) uuid.UUID {
	t.Helper()
	secret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, label+"-"+uuid.NewString(), isDefault)
	if err != nil {
		t.Fatalf("insert codex_auth secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert credential state: %v", err)
	}
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: secret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: account, Valid: true},
		MaterialRevision:  0,
	}); err != nil || n != 1 {
		t.Fatalf("link credential state = (%d, %v), want (1, nil)", n, err)
	}
	return secret
}

// TestCodexAccountRateLimitsFKLiveDB pins 00238's table shape: the composite FK rejects a
// cross-owner snapshot insert, and ON DELETE CASCADE removes the snapshot when the account
// is deleted.
func TestCodexAccountRateLimitsFKLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		other, "codex-crl-fk-"+other.String()+"@e2e")

	// A snapshot keyed (other, acc.ID) has no (user_id, id) match in codex_provider_account
	// — the composite FK refuses it (23503), so a snapshot can never describe another user's
	// account.
	if _, err := pool.Exec(ctx,
		`INSERT INTO codex_account_rate_limits (user_id, provider_account_id) VALUES ($1, $2)`,
		other, acc.ID); pgCode(err) != "23503" {
		t.Fatalf("cross-owner snapshot insert returned %v (code %q), want a foreign_key_violation (23503)", err, pgCode(err))
	}

	// The owner's snapshot inserts fine, then cascades away with the account.
	mustExec(ctx, t, pool,
		`INSERT INTO codex_account_rate_limits (user_id, provider_account_id) VALUES ($1, $2)`, user, acc.ID)
	mustExec(ctx, t, pool, `DELETE FROM codex_provider_account WHERE id=$1`, acc.ID)
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM codex_account_rate_limits WHERE provider_account_id=$1`, acc.ID).Scan(&cnt); err != nil {
		t.Fatalf("count after account delete: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("snapshot rows after account delete = %d, want 0 — ON DELETE CASCADE did not fire", cnt)
	}
}

// getUserSnapshot returns the owner-read row for one account, or fails if it is absent.
func getUserSnapshot(ctx context.Context, t *testing.T, q *store.Queries, user, account uuid.UUID) store.GetCodexAccountRateLimitsForUserRow {
	t.Helper()
	rows, err := q.GetCodexAccountRateLimitsForUser(ctx, user)
	if err != nil {
		t.Fatalf("GetCodexAccountRateLimitsForUser: %v", err)
	}
	for _, r := range rows {
		if r.ProviderAccountID == account {
			return r
		}
	}
	t.Fatalf("account %s not present in owner read", account)
	return store.GetCodexAccountRateLimitsForUserRow{}
}

// TestUpsertCodexAccountRateLimitsLiveDB pins the revision-fenced success write (PRD #1209
// M1): the happy path writes, and the write returns 0 rows (and leaves any existing row
// untouched) when the generation moved, the credential_revision moved, or the last linked
// alias was removed between observation and write.
func TestUpsertCodexAccountRateLimitsLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	upsert := func(acc uuid.UUID, gen, rev int64, buckets []byte, status string) int64 {
		t.Helper()
		n, err := q.UpsertCodexAccountRateLimits(ctx, store.UpsertCodexAccountRateLimitsParams{
			UserID: user, ProviderAccountID: acc, Buckets: buckets,
			ObservedGeneration: gen, ObservedCredentialRevision: rev, AttemptStatus: status,
		})
		if err != nil {
			t.Fatalf("UpsertCodexAccountRateLimits: %v", err)
		}
		return n
	}

	// --- happy path ---
	happy, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "happy", false)
	if n := upsert(happy, 0, 0, []byte(`{"reading":1}`), "ok"); n != 1 {
		t.Fatalf("happy-path upsert affected %d rows, want 1", n)
	}
	row := getUserSnapshot(ctx, t, q, user, happy)
	if string(row.Buckets) != `{"reading": 1}` && string(row.Buckets) != `{"reading":1}` {
		t.Fatalf("stored buckets = %q, want the written reading", row.Buckets)
	}
	if !row.LastSuccessAt.Valid || row.AttemptStatus.String != "ok" || row.AttemptError.Valid {
		t.Fatalf("after success: last_success_valid=%v status=%q error_valid=%v, want (true, ok, false)",
			row.LastSuccessAt.Valid, row.AttemptStatus.String, row.AttemptError.Valid)
	}

	// --- stale generation → 0 rows, existing reading untouched ---
	staleGen, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "stalegen", false)
	if n := upsert(staleGen, 0, 0, []byte(`{"first":1}`), "ok"); n != 1 {
		t.Fatalf("seed upsert affected %d rows, want 1", n)
	}
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET generation=1 WHERE id=$1`, staleGen)
	if n := upsert(staleGen, 0, 0, []byte(`{"stale":1}`), "ok"); n != 0 {
		t.Fatalf("stale-generation upsert affected %d rows, want 0 — the generation fence let it through", n)
	}
	if got := string(getUserSnapshot(ctx, t, q, user, staleGen).Buckets); got != `{"first": 1}` && got != `{"first":1}` {
		t.Fatalf("stale-generation upsert clobbered the reading to %q", got)
	}

	// --- stale credential_revision → 0 rows ---
	staleRev, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "sterev", false)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET credential_revision=1 WHERE id=$1`, staleRev)
	if n := upsert(staleRev, 0, 0, []byte(`{"x":1}`), "ok"); n != 0 {
		t.Fatalf("stale-credential_revision upsert affected %d rows, want 0", n)
	}

	// --- last linked alias removed → 0 rows (no subscription to meter) ---
	unlinked, secret := mkLinkedCodexAccount(ctx, t, pool, q, user, "unlinked", false)
	mustExec(ctx, t, pool, `DELETE FROM user_secrets WHERE id=$1`, secret) // cascades the state row
	if n := upsert(unlinked, 0, 0, []byte(`{"x":1}`), "ok"); n != 0 {
		t.Fatalf("upsert after the last linked alias was removed affected %d rows, want 0", n)
	}
}

// TestRecordCodexAccountPollFailureLiveDB pins the health-only failure write (PRD #1209
// M1): a failure after a success PRESERVES the prior reading + last_success_at while
// recording the attempt; a first-ever failure represents health with NULL buckets; and it
// shares the upsert's fence (0 rows on a moved generation or a removed alias).
func TestRecordCodexAccountPollFailureLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	fail := func(acc uuid.UUID, gen, rev int64, status, errMsg string) int64 {
		t.Helper()
		n, err := q.RecordCodexAccountPollFailure(ctx, store.RecordCodexAccountPollFailureParams{
			UserID: user, ProviderAccountID: acc, AttemptStatus: status, AttemptError: errMsg,
			ObservedGeneration: gen, ObservedCredentialRevision: rev,
		})
		if err != nil {
			t.Fatalf("RecordCodexAccountPollFailure: %v", err)
		}
		return n
	}

	// --- failure AFTER a success preserves buckets + last_success_at ---
	acc, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "aftersuccess", false)
	if _, err := q.UpsertCodexAccountRateLimits(ctx, store.UpsertCodexAccountRateLimitsParams{
		UserID: user, ProviderAccountID: acc, Buckets: []byte(`{"good":1}`),
		ObservedGeneration: 0, ObservedCredentialRevision: 0, AttemptStatus: "ok",
	}); err != nil {
		t.Fatalf("seed success: %v", err)
	}
	before := getUserSnapshot(ctx, t, q, user, acc)
	if n := fail(acc, 0, 0, "error", "boom"); n != 1 {
		t.Fatalf("failure-after-success affected %d rows, want 1", n)
	}
	after := getUserSnapshot(ctx, t, q, user, acc)
	if string(after.Buckets) != string(before.Buckets) {
		t.Fatalf("failure clobbered buckets: before=%q after=%q — it must preserve the last reading", before.Buckets, after.Buckets)
	}
	if !after.LastSuccessAt.Valid || after.LastSuccessAt.Time != before.LastSuccessAt.Time {
		t.Fatalf("failure changed last_success_at: before=%v after=%v — it must preserve it", before.LastSuccessAt, after.LastSuccessAt)
	}
	if after.AttemptStatus.String != "error" || after.AttemptError.String != "boom" {
		t.Fatalf("failure did not record the attempt: status=%q error=%q", after.AttemptStatus.String, after.AttemptError.String)
	}

	// --- first-ever poll is a failure: health with NULL buckets, NULL last_success_at ---
	first, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "firstfail", false)
	if n := fail(first, 0, 0, "error", "first"); n != 1 {
		t.Fatalf("first-poll failure affected %d rows, want 1", n)
	}
	fr := getUserSnapshot(ctx, t, q, user, first)
	if fr.Buckets != nil || fr.LastSuccessAt.Valid {
		t.Fatalf("first-poll failure: buckets=%q last_success_valid=%v, want (nil, false)", fr.Buckets, fr.LastSuccessAt.Valid)
	}
	if fr.AttemptStatus.String != "error" || fr.AttemptError.String != "first" {
		t.Fatalf("first-poll failure attempt = (%q, %q), want (error, first)", fr.AttemptStatus.String, fr.AttemptError.String)
	}

	// --- fence: a moved generation → 0 rows ---
	staleGen, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "failstalegen", false)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET generation=1 WHERE id=$1`, staleGen)
	if n := fail(staleGen, 0, 0, "error", "x"); n != 0 {
		t.Fatalf("failure with a stale generation affected %d rows, want 0", n)
	}

	// --- fence: the last linked alias removed → 0 rows ---
	unlinked, secret := mkLinkedCodexAccount(ctx, t, pool, q, user, "failunlinked", false)
	mustExec(ctx, t, pool, `DELETE FROM user_secrets WHERE id=$1`, secret)
	if n := fail(unlinked, 0, 0, "error", "x"); n != 0 {
		t.Fatalf("failure after the last linked alias was removed affected %d rows, want 0", n)
	}
}

// TestCodexLinkedEnumerationsLiveDB pins CountLinkedAliasesForCodexAccount, the linked-poll
// listings' dedup (two users + duplicate aliases → one row per canonical account), and the
// staged enumerations returning only 'staging' rows (PRD #1209 M1).
func TestCodexLinkedEnumerationsLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	// user: account A with TWO linked aliases (the dedup case), plus a staged alias.
	accA, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "a1", false)
	addLinkedAlias(ctx, t, pool, q, user, accA, "a2", false)
	stagedSecret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "staged-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert staged secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: stagedSecret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert staged state: %v", err)
	}

	// other user: account B with one linked alias.
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		other, "codex-enum-"+other.String()+"@e2e")
	accB, _ := mkLinkedCodexAccount(ctx, t, pool, q, other, "b1", false)

	// CountLinkedAliasesForCodexAccount: A has two linked aliases.
	if n, err := q.CountLinkedAliasesForCodexAccount(ctx, store.CountLinkedAliasesForCodexAccountParams{
		UserID: user, ProviderAccountID: pgtype.UUID{Bytes: accA, Valid: true},
	}); err != nil || n != 2 {
		t.Fatalf("CountLinkedAliasesForCodexAccount(A) = (%d, %v), want (2, nil)", n, err)
	}
	// An unknown / foreign account counts 0 (owner-scoped, no oracle).
	if n, err := q.CountLinkedAliasesForCodexAccount(ctx, store.CountLinkedAliasesForCodexAccountParams{
		UserID: user, ProviderAccountID: pgtype.UUID{Bytes: accB, Valid: true},
	}); err != nil || n != 0 {
		t.Fatalf("CountLinkedAliasesForCodexAccount(B under user) = (%d, %v), want (0, nil)", n, err)
	}

	// ListLinkedCodexAccountsToPoll: one row per canonical account despite A's two aliases.
	poll, err := q.ListLinkedCodexAccountsToPoll(ctx)
	if err != nil {
		t.Fatalf("ListLinkedCodexAccountsToPoll: %v", err)
	}
	countA, countB := 0, 0
	for _, r := range poll {
		switch r.ProviderAccountID {
		case accA:
			countA++
		case accB:
			countB++
		}
	}
	if countA != 1 || countB != 1 {
		t.Fatalf("poll listing rows: A=%d B=%d, want 1 each (dedup by canonical account across aliases/users)", countA, countB)
	}

	// ListLinkedCodexAccountsForUser: only A (owner-scoped, still deduped).
	forUser, err := q.ListLinkedCodexAccountsForUser(ctx, user)
	if err != nil {
		t.Fatalf("ListLinkedCodexAccountsForUser: %v", err)
	}
	if len(forUser) != 1 || forUser[0].ProviderAccountID != accA {
		t.Fatalf("owner poll listing = %d rows, want exactly [A]", len(forUser))
	}

	// Staged enumerations return only 'staging' rows.
	stagedAll, err := q.ListStagedCodexAliases(ctx)
	if err != nil {
		t.Fatalf("ListStagedCodexAliases: %v", err)
	}
	for _, r := range stagedAll {
		if r.UserSecretID == accA {
			t.Fatal("linked account id leaked into staged listing")
		}
	}
	foundStaged := false
	for _, r := range stagedAll {
		if r.UserSecretID == stagedSecret {
			foundStaged = true
		}
	}
	if !foundStaged {
		t.Fatal("the staging alias is missing from ListStagedCodexAliases")
	}

	stagedForUser, err := q.ListStagedCodexAliasesForUser(ctx, user)
	if err != nil {
		t.Fatalf("ListStagedCodexAliasesForUser: %v", err)
	}
	if len(stagedForUser) != 1 || stagedForUser[0].UserSecretID != stagedSecret {
		t.Fatalf("owner staged listing = %d rows, want exactly [stagedSecret]", len(stagedForUser))
	}
}
