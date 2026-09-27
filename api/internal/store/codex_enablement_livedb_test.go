package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1732 M3b, the Codex half of the polling fences, on the real schema: the D6
// worklists (an enabled staging alias reconciles; an account polls only while an ENABLED
// linked alias resolves to it; recovery promotion is gated the same way) and the D13
// late-write fence on the account's reading and failure writes.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// codexEnablementSig is the enablement list the poll listing captured for one account, read
// through the production owner listing (ListLinkedCodexAccountsForUser), or "" when the
// account is not listed (no enabled linked alias).
func codexEnablementSig(ctx context.Context, t *testing.T, q *store.Queries, user, account uuid.UUID) string {
	t.Helper()
	rows, err := q.ListLinkedCodexAccountsForUser(ctx, user)
	if err != nil {
		t.Fatalf("ListLinkedCodexAccountsForUser: %v", err)
	}
	for _, r := range rows {
		if r.ProviderAccountID == account {
			return r.EnablementSig
		}
	}
	return ""
}

// polledAccounts returns the accounts of user the factory-wide tick listing and the poke
// listing each return.
func polledAccounts(ctx context.Context, t *testing.T, q *store.Queries, user uuid.UUID) (tick, poke map[uuid.UUID]bool) {
	t.Helper()
	all, err := q.ListLinkedCodexAccountsToPoll(ctx)
	if err != nil {
		t.Fatalf("ListLinkedCodexAccountsToPoll: %v", err)
	}
	tick = map[uuid.UUID]bool{}
	for _, r := range all {
		if r.UserID == user {
			tick[r.ProviderAccountID] = true
		}
	}
	mine, err := q.ListLinkedCodexAccountsForUser(ctx, user)
	if err != nil {
		t.Fatalf("ListLinkedCodexAccountsForUser: %v", err)
	}
	poke = map[uuid.UUID]bool{}
	for _, r := range mine {
		poke[r.ProviderAccountID] = true
	}
	return tick, poke
}

func enabledLinkedCount(ctx context.Context, t *testing.T, q *store.Queries, user, account uuid.UUID) int64 {
	t.Helper()
	n, err := q.CountEnabledLinkedAliasesForCodexAccount(ctx, store.CountEnabledLinkedAliasesForCodexAccountParams{
		UserID: user, ProviderAccountID: pgtype.UUID{Bytes: account, Valid: true},
	})
	if err != nil {
		t.Fatalf("CountEnabledLinkedAliasesForCodexAccount: %v", err)
	}
	return n
}

// TestCodexStagedListingSkipsDisabledAliasLiveDB: reconciliation is per alias (D6). An
// enabled staging alias is listed by both the tick and the poke listing; a disabled one is
// listed by neither, and comes back on re-enable.
func TestCodexStagedListingSkipsDisabledAliasLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	staged := func(label string) uuid.UUID {
		id, err := insertSecret(ctx, pool, user, store.KindCodexAuth, label+"-"+uuid.NewString(), false)
		if err != nil {
			t.Fatalf("insert staged secret: %v", err)
		}
		if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
			UserSecretID: id, UserID: user, Status: "staging",
		}); err != nil {
			t.Fatalf("insert staged state: %v", err)
		}
		return id
	}
	on, off := staged("on"), staged("off")
	setEnabled(ctx, t, q, user, off, false)

	listed := func() (tick, poke map[uuid.UUID]bool) {
		all, err := q.ListStagedCodexAliases(ctx)
		if err != nil {
			t.Fatalf("ListStagedCodexAliases: %v", err)
		}
		tick = map[uuid.UUID]bool{}
		for _, r := range all {
			if r.UserID == user {
				tick[r.UserSecretID] = true
			}
		}
		mine, err := q.ListStagedCodexAliasesForUser(ctx, user)
		if err != nil {
			t.Fatalf("ListStagedCodexAliasesForUser: %v", err)
		}
		poke = map[uuid.UUID]bool{}
		for _, r := range mine {
			poke[r.UserSecretID] = true
		}
		return tick, poke
	}
	tick, poke := listed()
	if !tick[on] || !poke[on] {
		t.Fatalf("enabled staging alias listed tick=%v poke=%v, want both", tick[on], poke[on])
	}
	if tick[off] || poke[off] {
		t.Fatalf("disabled staging alias listed tick=%v poke=%v, want neither", tick[off], poke[off])
	}

	setEnabled(ctx, t, q, user, off, true)
	if tick, poke = listed(); !tick[off] || !poke[off] {
		t.Fatalf("re-enabled staging alias listed tick=%v poke=%v, want both", tick[off], poke[off])
	}
}

// TestCodexPollListingNeedsEnabledLinkedAliasLiveDB: liveness is per account (D6). An account
// with one disabled alias and an enabled sibling stays in both poll listings; once no linked
// alias is enabled it leaves both; re-enabling one brings it back. The captured enablement
// list moves on every transition.
func TestCodexPollListingNeedsEnabledLinkedAliasLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	acc, first := mkLinkedCodexAccount(ctx, t, pool, q, user, "first", false)
	second := addLinkedAlias(ctx, t, pool, q, user, acc, "second", false)

	sig0 := codexEnablementSig(ctx, t, q, user, acc)
	if sig0 == "" {
		t.Fatal("an account with two enabled linked aliases is not listed")
	}

	setEnabled(ctx, t, q, user, first, false)
	if tick, poke := polledAccounts(ctx, t, q, user); !tick[acc] || !poke[acc] {
		t.Fatalf("account with an enabled sibling listed tick=%v poke=%v, want both", tick[acc], poke[acc])
	}
	if n := enabledLinkedCount(ctx, t, q, user, acc); n != 1 {
		t.Fatalf("enabled linked aliases = %d, want 1", n)
	}
	sig1 := codexEnablementSig(ctx, t, q, user, acc)
	if sig1 == sig0 {
		t.Fatalf("enablement list did not move on a sibling disable: %q", sig1)
	}

	setEnabled(ctx, t, q, user, second, false)
	if tick, poke := polledAccounts(ctx, t, q, user); tick[acc] || poke[acc] {
		t.Fatalf("account with no enabled alias listed tick=%v poke=%v, want neither", tick[acc], poke[acc])
	}
	if n := enabledLinkedCount(ctx, t, q, user, acc); n != 0 {
		t.Fatalf("enabled linked aliases = %d, want 0", n)
	}
	// Sidebar membership (D8) still counts the account: stored preferences survive disable.
	if n, err := q.CountLinkedAliasesForCodexAccount(ctx, store.CountLinkedAliasesForCodexAccountParams{
		UserID: user, ProviderAccountID: pgtype.UUID{Bytes: acc, Valid: true},
	}); err != nil || n != 2 {
		t.Fatalf("CountLinkedAliasesForCodexAccount = (%d, %v), want (2, nil)", n, err)
	}

	setEnabled(ctx, t, q, user, second, true)
	if tick, poke := polledAccounts(ctx, t, q, user); !tick[acc] || !poke[acc] {
		t.Fatalf("account after re-enable listed tick=%v poke=%v, want both", tick[acc], poke[acc])
	}
	if sig := codexEnablementSig(ctx, t, q, user, acc); sig == sig0 || sig == sig1 {
		t.Fatalf("enablement list after re-enable = %q, want a new value", sig)
	}
}

// TestCodexReadingEnablementFenceLiveDB is the D13 late-write fence on both account writes.
// A poll captured the enablement list when it started; it writes nothing once the last
// enabled alias is disabled, nothing after a disable and re-enable (ABA), and a poll started
// after the re-enable writes. With an enabled sibling left, a fresh poll writes while an
// in-flight one from before the sibling's disable is discarded.
func TestCodexReadingEnablementFenceLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	upsert := func(acc uuid.UUID, sig string) int64 {
		t.Helper()
		n, err := q.UpsertCodexAccountRateLimits(ctx, store.UpsertCodexAccountRateLimitsParams{
			UserID: user, ProviderAccountID: acc, Buckets: []byte(`{"r":1}`), AttemptStatus: "ok",
			EnablementSig: sig,
		})
		if err != nil {
			t.Fatalf("UpsertCodexAccountRateLimits: %v", err)
		}
		return n
	}
	fail := func(acc uuid.UUID, sig string) int64 {
		t.Helper()
		n, err := q.RecordCodexAccountPollFailure(ctx, store.RecordCodexAccountPollFailureParams{
			UserID: user, ProviderAccountID: acc, AttemptStatus: "transient", AttemptError: "x",
			EnablementSig: sig,
		})
		if err != nil {
			t.Fatalf("RecordCodexAccountPollFailure: %v", err)
		}
		return n
	}

	// --- the last enabled alias: disable, then disable + re-enable ---
	solo, alias := mkLinkedCodexAccount(ctx, t, pool, q, user, "solo", false)
	started := codexEnablementSig(ctx, t, q, user, solo)
	if n := upsert(solo, started); n != 1 {
		t.Fatalf("write at the captured list = %d rows, want 1", n)
	}
	setEnabled(ctx, t, q, user, alias, false)
	if n := upsert(solo, started); n != 0 {
		t.Fatalf("reading after the last alias was disabled = %d rows, want 0", n)
	}
	if n := fail(solo, started); n != 0 {
		t.Fatalf("failure after the last alias was disabled = %d rows, want 0", n)
	}
	setEnabled(ctx, t, q, user, alias, true)
	if n := upsert(solo, started); n != 0 {
		t.Fatalf("reading started before the disable, finished after the re-enable = %d rows, want 0", n)
	}
	if n := fail(solo, started); n != 0 {
		t.Fatalf("failure started before the disable, finished after the re-enable = %d rows, want 0", n)
	}
	fresh := codexEnablementSig(ctx, t, q, user, solo)
	if n := upsert(solo, fresh); n != 1 {
		t.Fatalf("reading started after the re-enable = %d rows, want 1", n)
	}
	if n := fail(solo, fresh); n != 1 {
		t.Fatalf("failure started after the re-enable = %d rows, want 1", n)
	}

	// --- a disabled alias with an enabled sibling: the account stays live ---
	pair, a := mkLinkedCodexAccount(ctx, t, pool, q, user, "pair-a", false)
	addLinkedAlias(ctx, t, pool, q, user, pair, "pair-b", false)
	inflight := codexEnablementSig(ctx, t, q, user, pair)
	setEnabled(ctx, t, q, user, a, false)
	if n := upsert(pair, inflight); n != 0 {
		t.Fatalf("in-flight reading across a sibling disable = %d rows, want 0 (conservative discard)", n)
	}
	if n := upsert(pair, codexEnablementSig(ctx, t, q, user, pair)); n != 1 {
		t.Fatalf("fresh reading with an enabled sibling = %d rows, want 1", n)
	}
}

// TestCodexReadingFenceSerializesWithTransitionLiveDB: the fence is not a stale snapshot. A
// reading that reaches the database while the disable of the account's last enabled alias
// holds the alias row (the enablement handler's FOR UPDATE, then the transition) waits for
// it and then writes nothing.
func TestCodexReadingFenceSerializesWithTransitionLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	acc, alias := mkLinkedCodexAccount(ctx, t, pool, q, user, "serial", false)
	started := codexEnablementSig(ctx, t, q, user, acc)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := q.WithTx(tx)
	holder := backendPID(ctx, t, tx)
	if _, err := tq.GetUserSecretForUpdate(ctx, store.GetUserSecretForUpdateParams{ID: alias, UserID: user}); err != nil {
		t.Fatal(err)
	}
	if _, err := tq.SetSecretEnablement(ctx, store.SetSecretEnablementParams{ID: alias, UserID: user, Enabled: false}); err != nil {
		t.Fatal(err)
	}

	done := make(chan int64, 1)
	go func() {
		n, uerr := q.UpsertCodexAccountRateLimits(ctx, store.UpsertCodexAccountRateLimitsParams{
			UserID: user, ProviderAccountID: acc, Buckets: []byte(`{"r":1}`), AttemptStatus: "ok",
			EnablementSig: started,
		})
		if uerr != nil {
			n = -1
		}
		done <- n
	}()
	waitBlockedBy(ctx, t, pool, holder, "INSERT INTO codex_account_rate_limits", done)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := <-done; n != 0 {
		t.Fatalf("reading racing the disable = %d rows, want 0", n)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_account_rate_limits WHERE provider_account_id = $1`, acc).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("a reading landed behind the disable: %d rows", rows)
	}
}

// TestCodexSurvivorScanGatesRecoveryOnEnabledAliasLiveDB: the survivor scan's recovery arm
// (promotion, an upstream identity call) lists an account only while it has an enabled
// linked alias (D6), while the expired-lease arm lists it regardless: reaping a lease is
// durable completion of a refresh that already started (D7).
func TestCodexSurvivorScanGatesRecoveryOnEnabledAliasLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	scanned := func(acc uuid.UUID) bool {
		t.Helper()
		rows, err := q.ListUnresolvedCodexRefreshAccounts(ctx, pgtype.Timestamptz{Time: time.Now(), Valid: true})
		if err != nil {
			t.Fatalf("ListUnresolvedCodexRefreshAccounts: %v", err)
		}
		for _, r := range rows {
			if r.ID == acc {
				return true
			}
		}
		return false
	}

	recovering, alias := mkLinkedCodexAccount(ctx, t, pool, q, user, "recovering", false)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state = 'quarantined',
		recovery_sealed = 'material', recovery_sealed_with = 'master', recovery_generation = generation
		WHERE id = $1`, recovering)
	if !scanned(recovering) {
		t.Fatal("an enabled account with recovery material is not a survivor candidate")
	}
	setEnabled(ctx, t, q, user, alias, false)
	if scanned(recovering) {
		t.Fatal("an account with no enabled alias is still a recovery candidate")
	}
	setEnabled(ctx, t, q, user, alias, true)
	if !scanned(recovering) {
		t.Fatal("re-enabling the alias did not restore the recovery candidate")
	}

	wedged, wedgedAlias := mkLinkedCodexAccount(ctx, t, pool, q, user, "wedged", false)
	setEnabled(ctx, t, q, user, wedgedAlias, false)
	mustExec(ctx, t, pool, `UPDATE codex_provider_account SET coord_state = 'in_progress',
		coord_operation_id = gen_random_uuid(), lease_deadline = now() - interval '1 hour' WHERE id = $1`, wedged)
	if !scanned(wedged) {
		t.Fatal("an expired lease on an account with no enabled alias must still be reaped (D7)")
	}
}
