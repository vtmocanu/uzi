package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Codex reconciliation statuses (PRD #1147 M1). These are the string values the
// codex_credential_state.status CHECK admits; named here so the reconcile path
// never spells a bare literal that could drift from the schema.
const (
	codexStatusStaging = "staging"
	codexStatusLinked  = "linked"
	codexStatusFailed  = "failed"
)

// Codex reconciliation sentinels. Mapped by the (later) HTTP surface; here they let
// the LiveDB tests assert the precise refusal without string-matching.
var (
	// ErrCodexStateMissing: the alias has no codex_credential_state row (or it is
	// not this user's). Reconciliation has nothing to act on.
	ErrCodexStateMissing = errors.New("codex credential state not found")
	// ErrCodexStateNotReconcilable: the state row exists but is not in a status
	// reconciliation may act on (only 'staging'/'failed' are). A 'linked' or
	// 'static' row is left untouched.
	ErrCodexStateNotReconcilable = errors.New("codex credential state is not awaiting reconciliation")
	// ErrCodexSecretKind: the referenced user_secret is not a codex_auth login, so
	// its ciphertext is not a codex login blob. Defensive; the state row's composite
	// FK already ties it to a codex-kind secret.
	ErrCodexSecretKind = errors.New("user secret is not a codex_auth login")
	// ErrCodexLoginBlob: the stored codex_auth ciphertext did not decode to a login
	// blob with a usable access token.
	ErrCodexLoginBlob = errors.New("stored codex login blob is unusable")
	// ErrCodexRelinkRaceLost: the alias's material_revision advanced between the
	// reconcile's state read and its CAS relink (a manual replace raced a slow discovery),
	// so LinkCodexCredentialState matched 0 rows. TRANSIENT and distinct from
	// ErrCodexStateMissing (the alias genuinely vanished): the replacement's own material
	// bump re-triggers reconcile on the NEW material, which will relink correctly. The
	// reconcile must NOT mark the alias failed on this — it is not a failure, it is a
	// superseded attempt.
	ErrCodexRelinkRaceLost = errors.New("codex relink lost a material-revision race")
)

// CodexIdentityClient is the injectable identity seam the reconciler depends on
// (PRD #1147 M1). *codexauth.Client satisfies it; tests supply an in-process fake.
//
// It is DELIBERATELY narrow: reconciliation establishes identity WITHOUT rotating,
// so it needs only DiscoverIdentity and structurally CANNOT call Refresh. That is
// the guarantee "identity-first, no pre-identity rotation" — a fake with a refresh
// counter can prove the reconciler never spent one because the method it would call
// is not even on this interface.
type CodexIdentityClient interface {
	DiscoverIdentity(ctx context.Context, accessToken string) (codexauth.Identity, error)
}

// codexCredStore is the narrow query surface the reconciler uses. *store.Queries
// satisfies it; tests may embed it. It is separate from workersvc.Store because the
// Codex queries are new and the reconciler is the only consumer so far.
type codexCredStore interface {
	GetCodexCredentialState(ctx context.Context, arg store.GetCodexCredentialStateParams) (store.CodexCredentialState, error)
	SetCodexCredentialStateStatus(ctx context.Context, arg store.SetCodexCredentialStateStatusParams) (int64, error)
	LinkCodexCredentialState(ctx context.Context, arg store.LinkCodexCredentialStateParams) (int64, error)
	GetCodexProviderAccountByTuple(ctx context.Context, arg store.GetCodexProviderAccountByTupleParams) (store.CodexProviderAccount, error)
	InsertCodexProviderAccount(ctx context.Context, arg store.InsertCodexProviderAccountParams) (store.CodexProviderAccount, error)
	// RefreshCodexAccountLogin installs a freshly-sealed, VERIFIED login on an account and
	// advances its generation — the verified re-login restore for a quarantined (dead-login)
	// account whose canonical login must be replaced from a freshly imported blob (audit #5).
	RefreshCodexAccountLogin(ctx context.Context, arg store.RefreshCodexAccountLoginParams) (int64, error)
	// GetUserSecretCiphertextByID opens the alias's stored login blob, owner-scoped
	// (the same by-id primitive openAnthropic resolves through). Returns the row's
	// kind + sealed_with so the reconciler decrypts on the shared vault path.
	GetUserSecretCiphertextByID(ctx context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error)
}

// codexLoginBlob is the shape of a codex_auth secret's decrypted ciphertext: a JSON
// login carrying the provider tokens. It is UNTRUSTED imported material — a user
// pasted it — so every field is validated before use and nothing here is logged.
type codexLoginBlob struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// CodexReconciler establishes a freshly imported Codex login's canonical identity
// and binds its per-alias state row to the authoritative provider account (PRD
// #1147 M1, ships DARK).
//
// It is identity-first with NO pre-identity rotation: it opens the alias's stored
// login, reads its identity with a single nonrotating call, and reconciles by the
// full (user, provider_user_id, workspace_account_id) tuple — converging aliases
// that resolve to the same account onto one row and never spending a refresh to get
// there. The coordinated refresher (lease/intent/generation) is a later unit.
//
// The deps are injected so tests supply a fake identity client and (for the seal
// path) a real secretbox with a nil vault:
//   - q     the Codex + ciphertext queries;
//   - vlt   the per-user vault (nil ⇒ seal under the master box, the pre-vault path);
//   - box   the master box (the nil-vault seal/open fallback);
//   - ident the identity client (fake in tests).
type CodexReconciler struct {
	q     codexCredStore
	vlt   *vault.Vault
	box   *secretbox.Box
	ident CodexIdentityClient
}

// NewCodexReconciler builds a reconciler over the store, the vault (may be nil), the
// master box and the identity client.
func NewCodexReconciler(q codexCredStore, vlt *vault.Vault, box *secretbox.Box, ident CodexIdentityClient) *CodexReconciler {
	return &CodexReconciler{q: q, vlt: vlt, box: box, ident: ident}
}

// ReconcileCodexAuthIdentity establishes the identity of one staged codex_auth alias
// and binds it to its authoritative provider account (PRD #1147 M1). All work is
// owner-scoped.
//
// Steps:
//  1. Read the alias's codex_credential_state (must exist, status staging/failed) and
//     open its stored login blob via the shared vault-open path (by id, owner-scoped).
//  2. DiscoverIdentity — NONROTATING. On any discovery failure (incomplete identity,
//     an auth error, a transport error) mark the state 'failed' with last_error and
//     return, having made ZERO oauth/refresh calls. Refresh is NEVER called here: if
//     the access token is expired the import stays failed and the user re-logs in.
//  3. Reconcile by the full tuple: an existing account for the tuple is linked to
//     (its sealed_login untouched); otherwise the login blob is sealed under the DEK
//     and a new account (generation 0) is inserted, then linked. Linking flips the
//     state to 'linked'.
func (r *CodexReconciler) ReconcileCodexAuthIdentity(ctx context.Context, userID, userSecretID uuid.UUID) error {
	st, err := r.q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: userSecretID,
		UserID:       userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: alias %s", ErrCodexStateMissing, userSecretID)
		}
		return fmt.Errorf("codex reconcile: read state: %w", err)
	}
	if st.Status != codexStatusStaging && st.Status != codexStatusFailed {
		return fmt.Errorf("%w: status %q", ErrCodexStateNotReconcilable, st.Status)
	}

	// Open the alias's stored login blob, owner-scoped. GetUserSecretCiphertextByID's
	// predicate is (id AND user_id), and we re-check the returned owner — the same
	// defense-in-depth OpenByID applies — so a foreign or wrong-kind secret never
	// reaches the decrypt.
	row, err := r.q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
		ID:     userSecretID,
		UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: alias %s", ErrCodexStateMissing, userSecretID)
		}
		return fmt.Errorf("codex reconcile: read login: %w", err)
	}
	if row.UserID != userID {
		return fmt.Errorf("%w: alias %s", ErrCodexStateMissing, userSecretID)
	}
	if row.Kind != store.KindCodexAuth {
		return fmt.Errorf("%w: kind %q", ErrCodexSecretKind, row.Kind)
	}

	plain, err := secretopen.OpenSealed(r.vlt, r.box, userID, row.Kind, row.SealedWith, row.Ciphertext)
	if err != nil {
		// A locked vault is transient — never mark failed on it, the caller retries
		// after the next unlock (the same contract secretopen gives every opener).
		if errors.Is(err, secretopen.ErrVaultLocked) {
			return err
		}
		// Undecryptable material is terminal for this import.
		r.markFailed(ctx, userID, userSecretID, "stored codex login could not be decrypted")
		return fmt.Errorf("codex reconcile: open login: %w", err)
	}

	var blob codexLoginBlob
	if err := json.Unmarshal(plain, &blob); err != nil {
		r.markFailed(ctx, userID, userSecretID, "stored codex login is not valid JSON")
		return fmt.Errorf("%w: %v", ErrCodexLoginBlob, err)
	}
	if blob.AccessToken == "" {
		r.markFailed(ctx, userID, userSecretID, "stored codex login has no access token")
		return fmt.Errorf("%w: missing access token", ErrCodexLoginBlob)
	}

	// NONROTATING identity read. On ANY failure: mark failed with the reason and
	// return, having made zero oauth/refresh calls. Refresh is never invoked here.
	id, err := r.ident.DiscoverIdentity(ctx, blob.AccessToken)
	if err != nil {
		r.markFailed(ctx, userID, userSecretID, discoveryFailureReason(err))
		return fmt.Errorf("codex reconcile: discover identity: %w", err)
	}

	// Reconcile by the full identity tuple, owner-scoped.
	acct, err := r.q.GetCodexProviderAccountByTuple(ctx, store.GetCodexProviderAccountByTupleParams{
		UserID:             userID,
		ProviderUserID:     id.ProviderUserID,
		WorkspaceAccountID: id.WorkspaceAccountID,
	})
	switch {
	case err == nil:
		// Existing account for this tuple. The imported blob's identity is freshly VERIFIED
		// (that is how we resolved THIS account), so:
		//   - if the account is QUARANTINED (a dead login the coordinated refresher could not
		//     roll forward), RESTORE the canonical login from this verified blob BEFORE
		//     linking (audit #5): RefreshCodexAccountLogin installs it, advances the
		//     generation, and clears the quarantine, so the account is usable again;
		//   - if the account is HEALTHY (idle/committed), keep the no-overwrite converge — it
		//     is authoritative and another alias may already hold the canonical material.
		// Order: verify identity → (if quarantined) restore login → CAS-link.
		if acct.CoordState == codexCoordQuarantined {
			sealed, sealedWith, serr := r.sealLogin(userID, plain)
			if serr != nil {
				return fmt.Errorf("codex reconcile: seal re-login: %w", serr)
			}
			if _, rerr := r.q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{
				Sealed:     sealed,
				SealedWith: sealedWith,
				ID:         acct.ID,
				UserID:     userID,
			}); rerr != nil {
				return fmt.Errorf("codex reconcile: restore re-login: %w", rerr)
			}
		}
		if lerr := r.link(ctx, userID, userSecretID, acct.ID, st.MaterialRevision); lerr != nil {
			return lerr
		}
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		// No account for this tuple yet: seal the login blob under the DEK (AAD
		// user_id||codex_auth, the same vault Seal path the handler uses) and insert
		// the authoritative account at generation 0, then link.
		sealed, sealedWith, serr := r.sealLogin(userID, plain)
		if serr != nil {
			return fmt.Errorf("codex reconcile: seal login: %w", serr)
		}
		acct, ierr := r.q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID:             userID,
			ProviderUserID:     id.ProviderUserID,
			WorkspaceAccountID: id.WorkspaceAccountID,
			SealedLogin:        sealed,
			SealedWith:         sealedWith,
		})
		if ierr != nil {
			// Concurrent enrollment of the same tuple: another alias won the
			// insert between our tuple read and this write, tripping
			// codex_provider_account_tuple_key (23505). That is the convergence
			// case, not a failure — re-resolve the now-existing account and link
			// to it rather than creating a second copy. Any other error is real.
			if uniqueViolationOn(ierr, "codex_provider_account_tuple_key") {
				existing, gerr := r.q.GetCodexProviderAccountByTuple(ctx, store.GetCodexProviderAccountByTupleParams{
					UserID:             userID,
					ProviderUserID:     id.ProviderUserID,
					WorkspaceAccountID: id.WorkspaceAccountID,
				})
				if gerr != nil {
					return fmt.Errorf("codex reconcile: resolve after insert race: %w", gerr)
				}
				return r.link(ctx, userID, userSecretID, existing.ID, st.MaterialRevision)
			}
			return fmt.Errorf("codex reconcile: insert account: %w", ierr)
		}
		if lerr := r.link(ctx, userID, userSecretID, acct.ID, st.MaterialRevision); lerr != nil {
			return lerr
		}
		return nil
	default:
		return fmt.Errorf("codex reconcile: resolve tuple: %w", err)
	}
}

// link binds the alias's state row to the resolved account (status → 'linked'),
// CAS-guarded on the material_revision the reconcile OBSERVED at its top read (audit #4).
// observedMaterialRevision is that revision, threaded from ReconcileCodexAuthIdentity.
//
// A 0-row result is NOT "the alias vanished": the CAS now also matches material_revision,
// so 0 rows means the alias was REPLACED under us (BumpCodexMaterialRevision advanced the
// revision and dropped the old link) between the state read and this write. Surfacing
// ErrCodexRelinkRaceLost — not ErrCodexStateMissing, not markFailed — keeps that transient:
// the replacement's own bump re-triggers reconcile on the NEW material, which relinks it
// correctly. (On the new-account path a lost CAS may leave an orphan account row with no
// alias pointing at it; that is acceptable — it is unreferenced and harmless.)
func (r *CodexReconciler) link(ctx context.Context, userID, userSecretID, accountID uuid.UUID, observedMaterialRevision int64) error {
	n, err := r.q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(accountID),
		UserSecretID:      userSecretID,
		UserID:            userID,
		MaterialRevision:  observedMaterialRevision,
	})
	if err != nil {
		return fmt.Errorf("codex reconcile: link state: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: alias %s", ErrCodexRelinkRaceLost, userSecretID)
	}
	return nil
}

// markFailed records status='failed' with a human reason. Best-effort: the caller
// is already returning the underlying error, and a failed status write that itself
// fails must not mask it.
func (r *CodexReconciler) markFailed(ctx context.Context, userID, userSecretID uuid.UUID, reason string) {
	_, _ = r.q.SetCodexCredentialStateStatus(ctx, store.SetCodexCredentialStateStatusParams{
		Status:       codexStatusFailed,
		LastError:    pgconv.TextOrNull(reason),
		UserSecretID: userSecretID,
		UserID:       userID,
	})
}

// sealLogin encrypts the login blob for storage on the provider account, mirroring
// the handler's sealUserSecret: with a vault it seals under the user's DEK
// (sealed_with='dek', AAD user_id||codex_auth); without one (tests) it falls back to
// the master box (sealed_with='master').
func (r *CodexReconciler) sealLogin(userID uuid.UUID, plaintext []byte) (sealed []byte, sealedWith string, err error) {
	if r.vlt != nil {
		sealed, err = r.vlt.Seal(userID, store.KindCodexAuth, plaintext)
		return sealed, store.SealedWithDEK, err
	}
	sealed, err = r.box.Seal(plaintext)
	return sealed, store.SealedWithMaster, err
}

// discoveryFailureReason maps a DiscoverIdentity error to a short, secret-free
// last_error string. It names WHY the identity could not be established so a user
// reading the alias state knows whether to re-log-in (auth) or retry (transient),
// without echoing any token or response body.
func discoveryFailureReason(err error) string {
	switch {
	case errors.Is(err, codexauth.ErrIdentityIncomplete):
		return "provider did not return a complete identity (missing user or account id)"
	}
	var authErr *codexauth.AuthError
	if errors.As(err, &authErr) {
		if authErr.Unauthorized() {
			return "provider rejected the login token; re-login required"
		}
		return fmt.Sprintf("provider identity lookup failed with status %d", authErr.StatusCode)
	}
	return "provider identity lookup failed"
}
