package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests exercise ReconcileCodexAuthIdentity end to end against a REAL Postgres
// (PRD #1147 M1, ships DARK) with an IN-PROCESS fake identity client — no HTTP. They
// prove the reconciler's tuple convergence and its identity-first, no-pre-identity-
// rotation contract on the actual schema (the composite FKs, the status CHECK, the
// provider-account tuple key), which the pure-Go unit tests structurally cannot.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE
// the uzi- namespace, per the store live-DB harness). All fixture identity values are
// derived from fresh UUIDs so a single-process `-run 'LiveDB$'` sweep never collides.

// fakeCodexIdentity is the in-process identity client. It maps a presented access
// token to an Identity (or an error), counts DiscoverIdentity calls, and carries a
// refresh counter that PROVES the reconciler never rotates: the reconciler holds a
// CodexIdentityClient, whose surface excludes Refresh, so refreshCalls can only stay
// 0. (Refresh is implemented here anyway, so the type is a full codexauth-shaped
// client and the zero is a measured fact, not an absent method.)
type fakeCodexIdentity struct {
	idByToken  map[string]codexauth.Identity
	errByToken map[string]error

	discoverCalls int
	refreshCalls  int
}

func newFakeCodexIdentity() *fakeCodexIdentity {
	return &fakeCodexIdentity{
		idByToken:  map[string]codexauth.Identity{},
		errByToken: map[string]error{},
	}
}

func (f *fakeCodexIdentity) DiscoverIdentity(_ context.Context, accessToken string) (codexauth.Identity, error) {
	f.discoverCalls++
	if err, ok := f.errByToken[accessToken]; ok {
		return codexauth.Identity{}, err
	}
	if id, ok := f.idByToken[accessToken]; ok {
		return id, nil
	}
	return codexauth.Identity{}, fmt.Errorf("fake: no identity registered for token")
}

// Refresh is never reached through the reconciler's narrow interface; its presence
// makes the refreshCalls==0 assertion meaningful rather than vacuous.
func (f *fakeCodexIdentity) Refresh(_ context.Context, _ string) (codexauth.RefreshResult, error) {
	f.refreshCalls++
	return codexauth.RefreshResult{}, nil
}

// codexTestEnv bundles the live-DB scaffolding one test needs.
type codexTestEnv struct {
	ctx  context.Context
	pool *pgxpool.Pool
	q    *store.Queries
	box  *secretbox.Box
	exec func(sql string, args ...any)
}

// codexToken assembles a token-shaped fixture string from parts at runtime, so no
// contiguous token-looking literal lives in tracked source.
func codexToken(prefix string) string {
	return strings.Join([]string{prefix, uuid.NewString(), "fixture", "not", "real"}, "-")
}

func setupCodexLiveDB(t *testing.T) codexTestEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	// A real master box: with a nil vault the reconciler seals/open on the master path.
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	return codexTestEnv{ctx: ctx, pool: pool, q: store.New(pool), box: box, exec: exec}
}

// seedUser inserts a fresh user and returns its id.
func (e codexTestEnv) seedUser(t *testing.T) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("codex-%s@e2e", userID))
	return userID
}

// seedStagingAlias inserts a codex_auth user_secret whose ciphertext is the given
// login blob (master-sealed), plus its staging codex_credential_state row, and
// returns the alias id. label must be unique per (user, kind).
func (e codexTestEnv) seedStagingAlias(t *testing.T, userID uuid.UUID, label string, blob codexLoginBlob) uuid.UUID {
	t.Helper()
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	sealed, err := e.box.Seal(raw)
	if err != nil {
		t.Fatalf("seal blob: %v", err)
	}
	secretID := uuid.New()
	e.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
	        VALUES ($1, $2, 'codex_auth', $3, $4, 'master')`, secretID, userID, label, sealed)
	if _, err := e.q.InsertCodexCredentialState(e.ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretID,
		UserID:       userID,
		Status:       "staging",
	}); err != nil {
		t.Fatalf("insert state: %v", err)
	}
	return secretID
}

// countProviderAccounts returns how many codex_provider_account rows a user holds.
func (e codexTestEnv) countProviderAccounts(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM codex_provider_account WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	return n
}

// (i) An import whose discovery returns an incomplete/absent identity (or an expired/
// 401 token) stays failed and NEVER rotates: the fake's refresh counter stays 0 and
// no provider account is created.
func TestReconcileCodexIncompleteIdentityStaysFailedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	incompleteTok := codexToken("access-incomplete")
	expiredTok := codexToken("access-expired")
	blobIncomplete := codexLoginBlob{AccessToken: incompleteTok, RefreshToken: codexToken("refresh")}
	blobExpired := codexLoginBlob{AccessToken: expiredTok, RefreshToken: codexToken("refresh")}

	aliasIncomplete := env.seedStagingAlias(t, userID, "codex-incomplete", blobIncomplete)
	aliasExpired := env.seedStagingAlias(t, userID, "codex-expired", blobExpired)

	fake := newFakeCodexIdentity()
	fake.errByToken[incompleteTok] = codexauth.ErrIdentityIncomplete
	fake.errByToken[expiredTok] = &codexauth.AuthError{Op: "discover_identity", StatusCode: 401}
	r := NewCodexReconciler(env.q, nil, env.box, fake)

	for _, alias := range []uuid.UUID{aliasIncomplete, aliasExpired} {
		err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias)
		if err == nil {
			t.Fatalf("alias %s: want a discovery error, got nil", alias)
		}
		st, gerr := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: userID})
		if gerr != nil {
			t.Fatalf("read state: %v", gerr)
		}
		// Not linked, no account bound, an error recorded.
		if st.Status != "failed" {
			t.Fatalf("alias %s status = %q, want failed", alias, st.Status)
		}
		if st.ProviderAccountID.Valid {
			t.Fatalf("alias %s must have no provider account bound", alias)
		}
		if !st.LastError.Valid || st.LastError.String == "" {
			t.Fatalf("alias %s must record a last_error", alias)
		}
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("refresh counter = %d, want 0 (no pre-identity rotation)", fake.refreshCalls)
	}
	if n := env.countProviderAccounts(t, userID); n != 0 {
		t.Fatalf("provider accounts = %d, want 0 for failed imports", n)
	}
}

// (ii) Two aliases whose discovery yields the SAME tuple converge to ONE
// codex_provider_account: the second links, it does not create a second row. Also
// covers (iv): a successful reconcile flips staging→linked with provider_account_id
// pointing at the account, and no rotation occurs.
func TestReconcileCodexSameTupleConvergesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	providerUser := "user-" + uuid.NewString()
	workspace := "acct-" + uuid.NewString()

	tok1 := codexToken("access-1")
	tok2 := codexToken("access-2")
	alias1 := env.seedStagingAlias(t, userID, "codex-a", codexLoginBlob{AccessToken: tok1, RefreshToken: codexToken("r")})
	alias2 := env.seedStagingAlias(t, userID, "codex-b", codexLoginBlob{AccessToken: tok2, RefreshToken: codexToken("r")})

	fake := newFakeCodexIdentity()
	sameID := codexauth.Identity{ProviderUserID: providerUser, WorkspaceAccountID: workspace}
	fake.idByToken[tok1] = sameID
	fake.idByToken[tok2] = sameID
	r := NewCodexReconciler(env.q, nil, env.box, fake)

	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias1); err != nil {
		t.Fatalf("reconcile alias1: %v", err)
	}
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias2); err != nil {
		t.Fatalf("reconcile alias2: %v", err)
	}

	st1 := mustState(t, env, userID, alias1)
	st2 := mustState(t, env, userID, alias2)
	// (iv): staging → linked, account bound.
	if st1.Status != "linked" || st2.Status != "linked" {
		t.Fatalf("statuses = %q,%q, want linked,linked", st1.Status, st2.Status)
	}
	if !st1.ProviderAccountID.Valid || !st2.ProviderAccountID.Valid {
		t.Fatalf("both aliases must bind a provider account")
	}
	// Convergence: both point at the SAME account, and exactly one row exists.
	if st1.ProviderAccountID.Bytes != st2.ProviderAccountID.Bytes {
		t.Fatalf("aliases converged to different accounts: %x vs %x", st1.ProviderAccountID.Bytes, st2.ProviderAccountID.Bytes)
	}
	if n := env.countProviderAccounts(t, userID); n != 1 {
		t.Fatalf("provider accounts = %d, want 1 (second alias must link, not create)", n)
	}
	// The bound account really carries the reconciled tuple.
	acct, err := env.q.GetCodexProviderAccountByID(env.ctx, store.GetCodexProviderAccountByIDParams{
		UserID: userID,
		ID:     uuid.UUID(st1.ProviderAccountID.Bytes),
	})
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	if acct.ProviderUserID != providerUser || acct.WorkspaceAccountID != workspace {
		t.Fatalf("account tuple = (%s,%s), want (%s,%s)", acct.ProviderUserID, acct.WorkspaceAccountID, providerUser, workspace)
	}
	if acct.Generation != 0 {
		t.Fatalf("new account generation = %d, want 0", acct.Generation)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("refresh counter = %d, want 0", fake.refreshCalls)
	}
}

// (iii) Same workspace_account_id, DIFFERENT provider_user_id under one user produce
// TWO separate accounts.
func TestReconcileCodexDifferentUserSameWorkspaceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	workspace := "acct-" + uuid.NewString()
	providerUserA := "user-a-" + uuid.NewString()
	providerUserB := "user-b-" + uuid.NewString()

	tokA := codexToken("access-a")
	tokB := codexToken("access-b")
	aliasA := env.seedStagingAlias(t, userID, "codex-wsa", codexLoginBlob{AccessToken: tokA, RefreshToken: codexToken("r")})
	aliasB := env.seedStagingAlias(t, userID, "codex-wsb", codexLoginBlob{AccessToken: tokB, RefreshToken: codexToken("r")})

	fake := newFakeCodexIdentity()
	fake.idByToken[tokA] = codexauth.Identity{ProviderUserID: providerUserA, WorkspaceAccountID: workspace}
	fake.idByToken[tokB] = codexauth.Identity{ProviderUserID: providerUserB, WorkspaceAccountID: workspace}
	r := NewCodexReconciler(env.q, nil, env.box, fake)

	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, aliasA); err != nil {
		t.Fatalf("reconcile aliasA: %v", err)
	}
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, aliasB); err != nil {
		t.Fatalf("reconcile aliasB: %v", err)
	}

	stA := mustState(t, env, userID, aliasA)
	stB := mustState(t, env, userID, aliasB)
	if stA.ProviderAccountID.Bytes == stB.ProviderAccountID.Bytes {
		t.Fatalf("different provider_user_id must NOT converge to one account")
	}
	if n := env.countProviderAccounts(t, userID); n != 2 {
		t.Fatalf("provider accounts = %d, want 2", n)
	}
	if fake.refreshCalls != 0 {
		t.Fatalf("refresh counter = %d, want 0", fake.refreshCalls)
	}
}

func mustState(t *testing.T, env codexTestEnv, userID, alias uuid.UUID) store.CodexCredentialState {
	t.Helper()
	st, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: userID})
	if err != nil {
		t.Fatalf("read state %s: %v", alias, err)
	}
	return st
}

// Guard: the reconciler refuses a non-reconcilable status without touching anything.
func TestReconcileCodexRefusesLinkedStatusLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID := env.seedUser(t)

	tok := codexToken("access")
	alias := env.seedStagingAlias(t, userID, "codex-linked", codexLoginBlob{AccessToken: tok, RefreshToken: codexToken("r")})

	fake := newFakeCodexIdentity()
	fake.idByToken[tok] = codexauth.Identity{ProviderUserID: "u-" + uuid.NewString(), WorkspaceAccountID: "a-" + uuid.NewString()}
	r := NewCodexReconciler(env.q, nil, env.box, fake)

	// First reconcile links it.
	if err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	discoverAfterFirst := fake.discoverCalls
	// A second reconcile on a now-'linked' row must refuse, making no discovery call.
	err := r.ReconcileCodexAuthIdentity(env.ctx, userID, alias)
	if !errors.Is(err, ErrCodexStateNotReconcilable) {
		t.Fatalf("err = %v, want ErrCodexStateNotReconcilable", err)
	}
	if fake.discoverCalls != discoverAfterFirst {
		t.Fatalf("refused reconcile must not call discovery (calls %d → %d)", discoverAfterFirst, fake.discoverCalls)
	}
}
