package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexLiveDB is the shared harness for the Codex credentials LiveDB suite (PRD
// #1147 M1, STORE/SCHEMA). It skips unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (./e2e/run-store-it.sh provides one and sweeps this package for
// the LiveDB suffix), migrates, and hands back a pool + a fresh user id so each
// caller's fixtures are unique in a single-process `-run 'LiveDB$'` sweep.
func codexLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries, uuid.UUID) {
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
	q := store.New(pool)

	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user, fmt.Sprintf("codex-%s@e2e", user))
	return ctx, pool, q, user
}

// pgCode returns the Postgres SQLSTATE of err, or "" if err is not a *pgconn.PgError.
// 23505 = unique_violation, 23514 = check_violation, 23503 = foreign_key_violation.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// insertSecret creates a user_secrets row directly (bypassing the InsertUserSecret
// first-token forcing, which is anthropic-specific) so a test can place a codex
// credential in exactly the (kind, is_default) state it means to probe. Returns the
// minted secret id and any error, so callers can assert on both a success and a
// constraint violation.
func insertSecret(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID, kind, label string, isDefault bool) (uuid.UUID, error) {
	id := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, $3, $4, $5, $6, 'master')`,
		id, user, kind, label, isDefault, []byte("sealed-"+label))
	return id, err
}

// TestCodexSecretKindsLiveDB pins 00197: the kind CHECK now admits both codex kinds
// and still rejects an unknown one, and the shared codex default index collapses the
// two codex kinds into one default slot while leaving the anthropic default alone.
func TestCodexSecretKindsLiveDB(t *testing.T) {
	ctx, pool, _, user := codexLiveDB(t)

	// The CHECK now accepts both new codex kinds.
	if _, err := insertSecret(ctx, pool, user, store.KindOpenAIAPIKey, "openai-"+uuid.NewString(), false); err != nil {
		t.Fatalf("openai_api_key rejected by kind CHECK: %v", err)
	}
	if _, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "codex-"+uuid.NewString(), false); err != nil {
		t.Fatalf("codex_auth rejected by kind CHECK: %v", err)
	}
	// And still rejects an unknown kind (check_violation).
	if _, err := insertSecret(ctx, pool, user, "not_a_real_kind", "bogus-"+uuid.NewString(), false); pgCode(err) != "23514" {
		t.Fatalf("unknown kind returned %v (code %q), want a check_violation (23514)", err, pgCode(err))
	}

	// --- user_secrets_codex_one_default_key: one codex default across BOTH kinds ---
	def := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		def, fmt.Sprintf("codex-def-%s@e2e", def))

	// A codex_auth default is fine on its own.
	if _, err := insertSecret(ctx, pool, def, store.KindCodexAuth, "codex-"+uuid.NewString(), true); err != nil {
		t.Fatalf("first codex default rejected: %v", err)
	}
	// A SECOND codex default of the OTHER codex kind collides — the partial index
	// spans both kinds, unlike 00077's per-kind one_default_key.
	if _, err := insertSecret(ctx, pool, def, store.KindOpenAIAPIKey, "openai-"+uuid.NewString(), true); pgCode(err) != "23505" {
		t.Fatalf("a second codex default returned %v (code %q), want a unique_violation (23505) — "+
			"the shared codex default index is not enforcing", err, pgCode(err))
	}
	// An anthropic default coexists with the codex default: they live in separate
	// default slots (00077's index vs 00197's).
	if _, err := insertSecret(ctx, pool, def, store.KindAnthropicToken, "anthropic-"+uuid.NewString(), true); err != nil {
		t.Fatalf("an anthropic default must coexist with a codex default, got: %v", err)
	}
}

// TestCodexProviderAccountTupleLiveDB pins 00198's identity tuple: identical tuples
// for one user collide, a same-workspace/different-principal tuple is a distinct
// account, and two uzi users may independently hold the same tuple.
func TestCodexProviderAccountTupleLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	providerA := "provider-" + uuid.NewString()
	providerB := "provider-" + uuid.NewString()
	workspace := "workspace-" + uuid.NewString()

	mk := func(u uuid.UUID, provider, ws string) error {
		_, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: u, ProviderUserID: provider, WorkspaceAccountID: ws,
			SealedLogin: []byte("sealed-login"), SealedWith: store.SealedWithMaster,
		})
		return err
	}

	if err := mk(user, providerA, workspace); err != nil {
		t.Fatalf("first account insert: %v", err)
	}
	// Identical tuple for the same user collides.
	if err := mk(user, providerA, workspace); pgCode(err) != "23505" {
		t.Fatalf("duplicate tuple returned %v (code %q), want a unique_violation (23505)", err, pgCode(err))
	}
	// SAME workspace, DIFFERENT provider principal → a distinct account, allowed.
	if err := mk(user, providerB, workspace); err != nil {
		t.Fatalf("same workspace with a different provider principal must be a distinct account, got: %v", err)
	}

	// A DIFFERENT uzi user may hold the very same tuple as `user`'s first account.
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		other, fmt.Sprintf("codex-other-%s@e2e", other))
	if err := mk(other, providerA, workspace); err != nil {
		t.Fatalf("two uzi users must be able to share a provider tuple, got: %v", err)
	}

	// The tuple resolver and the owner-scoped by-id read return the same row.
	byTuple, err := q.GetCodexProviderAccountByTuple(ctx, store.GetCodexProviderAccountByTupleParams{
		UserID: user, ProviderUserID: providerA, WorkspaceAccountID: workspace,
	})
	if err != nil {
		t.Fatalf("resolve by tuple: %v", err)
	}
	byID, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{
		UserID: user, ID: byTuple.ID,
	})
	if err != nil {
		t.Fatalf("resolve by id: %v", err)
	}
	if byID.ID != byTuple.ID {
		t.Fatalf("by-id resolved %s, want %s", byID.ID, byTuple.ID)
	}
	// Owner-scoping: `other` cannot read `user`'s account by its id — the owner-scoped
	// predicate returns no row (pgx.ErrNoRows), not that user's account.
	if _, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{
		UserID: other, ID: byTuple.ID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("by-id read of another user's account returned %v, want pgx.ErrNoRows — the predicate is not owner-scoped", err)
	}
	// Fresh-minted defaults are what a later refresher (m2) advances from.
	if byTuple.Generation != 0 || byTuple.CredentialRevision != 0 || byTuple.CoordState != "idle" {
		t.Fatalf("new account defaults = (gen=%d, rev=%d, coord=%q), want (0, 0, \"idle\")",
			byTuple.Generation, byTuple.CredentialRevision, byTuple.CoordState)
	}
}

// TestCodexCredentialStateLiveDB pins 00199: per-alias insert, link to an account,
// the material-revision bump that increments and un-links, and the owner-scoped
// composite FK that refuses a state row over a secret the user does not own.
func TestCodexCredentialStateLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	// A static openai_api_key: born 'static', no subscription account behind it.
	staticSecret, err := insertSecret(ctx, pool, user, store.KindOpenAIAPIKey, "static-"+uuid.NewString(), true)
	if err != nil {
		t.Fatalf("insert openai secret: %v", err)
	}
	staticState, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: staticSecret, UserID: user, Status: "static",
	})
	if err != nil {
		t.Fatalf("insert static state: %v", err)
	}
	if staticState.Status != "static" || staticState.ProviderAccountID.Valid || staticState.MaterialRevision != 0 {
		t.Fatalf("static state = (status=%q, account_valid=%v, rev=%d), want (\"static\", false, 0)",
			staticState.Status, staticState.ProviderAccountID.Valid, staticState.MaterialRevision)
	}

	// A codex_auth alias: born 'staging', then linked to a provider account.
	authSecret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "auth-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert codex_auth secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: authSecret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert staging state: %v", err)
	}
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "provider-" + uuid.NewString(),
		WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin:        []byte("sealed-login"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: authSecret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: account.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if n != 1 {
		t.Fatalf("link affected %d rows, want 1", n)
	}
	linked, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: authSecret, UserID: user,
	})
	if err != nil {
		t.Fatalf("read linked state: %v", err)
	}
	if linked.Status != "linked" || !linked.ProviderAccountID.Valid || uuid.UUID(linked.ProviderAccountID.Bytes) != account.ID {
		t.Fatalf("linked state = (status=%q, account_valid=%v), want (\"linked\", true) pointing at %s",
			linked.Status, linked.ProviderAccountID.Valid, account.ID)
	}

	// BumpCodexMaterialRevision: a material change invalidates the binding — it
	// increments the revision, sets the new status, and un-links the account.
	n, err = q.BumpCodexMaterialRevision(ctx, store.BumpCodexMaterialRevisionParams{
		UserSecretID: authSecret, UserID: user, Status: "staging",
	})
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if n != 1 {
		t.Fatalf("bump affected %d rows, want 1", n)
	}
	bumped, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: authSecret, UserID: user,
	})
	if err != nil {
		t.Fatalf("read bumped state: %v", err)
	}
	if bumped.MaterialRevision != linked.MaterialRevision+1 {
		t.Fatalf("material_revision = %d after bump, want %d", bumped.MaterialRevision, linked.MaterialRevision+1)
	}
	if bumped.ProviderAccountID.Valid {
		t.Fatal("bump did not un-link the provider account — a material change must invalidate the binding")
	}
	if bumped.Status != "staging" {
		t.Fatalf("bumped status = %q, want \"staging\"", bumped.Status)
	}

	// SetCodexCredentialStateStatus records a failure reason without touching the link.
	if _, err := q.SetCodexCredentialStateStatus(ctx, store.SetCodexCredentialStateStatusParams{
		UserSecretID: authSecret, UserID: user, Status: "failed",
		LastError: pgtype.Text{String: "provider refused the login", Valid: true},
	}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	failed, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: authSecret, UserID: user,
	})
	if err != nil {
		t.Fatalf("read failed state: %v", err)
	}
	if failed.Status != "failed" || failed.LastError.String != "provider refused the login" {
		t.Fatalf("failed state = (status=%q, last_error=%q), want (\"failed\", \"provider refused the login\")",
			failed.Status, failed.LastError.String)
	}

	// --- Composite-FK ownership: a state row must reference an OWNED secret ---
	//
	// A different user's id over `user`'s secret has no (user_id, id) match in
	// user_secrets, so the composite FK (00199, using 00077's user_secrets_user_id_id_key)
	// refuses it in the schema rather than leaving ownership to a Go check. The probe
	// secret is fresh and state-less so the composite FK — not the user_secret_id PK —
	// is what rejects the insert.
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		other, fmt.Sprintf("codex-fk-%s@e2e", other))
	probeSecret, err := insertSecret(ctx, pool, user, store.KindOpenAIAPIKey, "probe-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert probe secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: probeSecret, UserID: other, Status: "static",
	}); pgCode(err) != "23503" {
		t.Fatalf("a state row over another user's secret returned %v (code %q), want a "+
			"foreign_key_violation (23503) — the composite FK is not owner-scoped", err, pgCode(err))
	}
}

// TestLinkCodexCredentialStateRevisionCASLiveDB pins the material-revision CAS the
// security audit added to LinkCodexCredentialState (PRD #1147): a relink succeeds only
// when it presents the material_revision the alias currently carries. A relink that
// observed an OLD revision (a manual replace bumped it under a slow discovery) matches 0
// rows and cannot re-point the freshly replaced alias at the stale account it resolved.
func TestLinkCodexCredentialStateRevisionCASLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	secret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "cas-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert staging state: %v", err)
	}
	accountA, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert accountA: %v", err)
	}
	accountB, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert accountB: %v", err)
	}

	// Link at the current revision (0): succeeds.
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: secret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: accountA.ID, Valid: true}, MaterialRevision: 0,
	}); err != nil || n != 1 {
		t.Fatalf("LinkCodexCredentialState(rev 0 matches) = (%d,%v), want (1,nil)", n, err)
	}

	// A manual replace bumps material_revision to 1 and drops the link.
	if n, err := q.BumpCodexMaterialRevision(ctx, store.BumpCodexMaterialRevisionParams{
		UserSecretID: secret, UserID: user, Status: "staging",
	}); err != nil || n != 1 {
		t.Fatalf("BumpCodexMaterialRevision = (%d,%v), want (1,nil)", n, err)
	}

	// A slow discovery that observed revision 0 relinks blindly: the CAS refuses it.
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: secret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: accountA.ID, Valid: true}, MaterialRevision: 0,
	}); err != nil || n != 0 {
		t.Fatalf("LinkCodexCredentialState(stale rev 0) = (%d,%v), want (0,nil) — the CAS fence must reject a bumped revision", n, err)
	}
	// The alias is still un-linked after the refused stale relink.
	if st, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: secret, UserID: user}); err != nil {
		t.Fatalf("read state: %v", err)
	} else if st.ProviderAccountID.Valid {
		t.Fatal("a stale relink linked the alias — the material-revision CAS did not fence it")
	}

	// A relink presenting the CURRENT revision (1) succeeds and binds accountB.
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: secret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: accountB.ID, Valid: true}, MaterialRevision: 1,
	}); err != nil || n != 1 {
		t.Fatalf("LinkCodexCredentialState(rev 1 matches) = (%d,%v), want (1,nil)", n, err)
	}
	linked, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{UserSecretID: secret, UserID: user})
	if err != nil {
		t.Fatalf("read relinked state: %v", err)
	}
	if linked.Status != "linked" || !linked.ProviderAccountID.Valid || uuid.UUID(linked.ProviderAccountID.Bytes) != accountB.ID {
		t.Fatalf("relinked state = (status=%q account_valid=%v), want linked pointing at accountB",
			linked.Status, linked.ProviderAccountID.Valid)
	}
}

// TestLinkCodexCredentialStateCrossUserFKLiveDB pins the (user_id, provider_account_id)
// composite FK on codex_credential_state (T-A): linking `user`'s alias to an account
// owned by a DIFFERENT user has no matching (user_id, id) pair in codex_provider_account,
// so the FK refuses the relink with a foreign_key_violation (23503) rather than binding an
// alias across an ownership boundary.
func TestLinkCodexCredentialStateCrossUserFKLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	// `user`'s alias with a staging state row.
	secret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "xu-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert staging state: %v", err)
	}

	// A DIFFERENT user owns the provider account we try to (illegally) link to.
	other := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		other, fmt.Sprintf("codex-xu-%s@e2e", other))
	otherAccount, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: other, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert other's account: %v", err)
	}

	// Link as `user` to `other`'s account: (user, otherAccount) is not a valid pair in
	// codex_provider_account, so the composite FK rejects it (23503).
	if _, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: secret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: otherAccount.ID, Valid: true}, MaterialRevision: 0,
	}); pgCode(err) != "23503" {
		t.Fatalf("cross-user link returned %v (code %q), want a foreign_key_violation (23503) — "+
			"the (user_id, provider_account_id) composite FK is not owner-scoped", err, pgCode(err))
	}
}

// TestCodexCredentialStateOrphanTriggerLiveDB pins BOTH sides of the refined F4 orphan
// trigger (00199). (1) The FK cascade path: deleting a linked account nulls the alias's
// provider_account_id and — because the cascade leaves status untouched (NEW.status =
// OLD.status) — the trigger fires, demoting the row to 'failed' with an explanatory
// last_error. (2) The regression guard: a legitimate app UPDATE that nulls
// provider_account_id AND changes status in the same statement (BumpCodexMaterialRevision
// to 'staging') is SKIPPED by the NEW.status = OLD.status guard, so its intended status is
// preserved rather than clobbered to 'failed'.
func TestCodexCredentialStateOrphanTriggerLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	linkAlias := func() (uuid.UUID, uuid.UUID) {
		secret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "orph-"+uuid.NewString(), false)
		if err != nil {
			t.Fatalf("insert secret: %v", err)
		}
		if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
			UserSecretID: secret, UserID: user, Status: "staging",
		}); err != nil {
			t.Fatalf("insert staging state: %v", err)
		}
		acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
			SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert account: %v", err)
		}
		if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
			UserSecretID: secret, UserID: user,
			ProviderAccountID: pgtype.UUID{Bytes: acc.ID, Valid: true}, MaterialRevision: 0,
		}); err != nil || n != 1 {
			t.Fatalf("link = (%d,%v), want (1,nil)", n, err)
		}
		return secret, acc.ID
	}

	// --- (1) FK cascade demotes the orphaned row to 'failed'. ---
	cascadeSecret, cascadeAccount := linkAlias()
	mustExec(ctx, t, pool, `DELETE FROM codex_provider_account WHERE id = $1 AND user_id = $2`, cascadeAccount, user)
	orphaned, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: cascadeSecret, UserID: user,
	})
	if err != nil {
		t.Fatalf("read orphaned state: %v", err)
	}
	if orphaned.ProviderAccountID.Valid {
		t.Fatal("provider_account_id survived the account delete — the ON DELETE SET NULL cascade did not fire")
	}
	if orphaned.Status != "failed" || !orphaned.LastError.Valid || orphaned.LastError.String == "" {
		t.Fatalf("orphaned state = (status=%q last_error=%+v), want (\"failed\", non-empty) — the trigger must demote a cascade-orphaned row",
			orphaned.Status, orphaned.LastError)
	}

	// --- (2) Regression: BumpCodexMaterialRevision(status='staging') nulls the link AND
	//     changes status in one statement, so the trigger is skipped and 'staging' survives. ---
	bumpSecret, _ := linkAlias()
	if n, err := q.BumpCodexMaterialRevision(ctx, store.BumpCodexMaterialRevisionParams{
		UserSecretID: bumpSecret, UserID: user, Status: "staging",
	}); err != nil || n != 1 {
		t.Fatalf("BumpCodexMaterialRevision = (%d,%v), want (1,nil)", n, err)
	}
	restaged, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: bumpSecret, UserID: user,
	})
	if err != nil {
		t.Fatalf("read restaged state: %v", err)
	}
	if restaged.ProviderAccountID.Valid {
		t.Fatal("BumpCodexMaterialRevision did not un-link the account")
	}
	if restaged.Status != "staging" {
		t.Fatalf("restaged status = %q, want \"staging\" — the trigger wrongly fired on an app-driven re-stage (NEW.status = OLD.status guard is missing)",
			restaged.Status)
	}
}
