package workersvc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Codex per-run credential-operation authority + claim binding (PRD #1147 M2, B5/B7),
// ships DARK. This is the SERVICE half over the m2-A store primitives: it mints the
// per-claim capability, decides whether a presented capability may perform a scoped
// credential operation over the run's frozen binding, and freezes a run's binding at
// creation. The coordinated-refresh state machine (lease/intent/generation) is a
// SEPARATE later unit — the store primitives for it exist but are NOT driven here.

// Codex auth modes (the runs.codex_auth_mode CHECK values, migration 00201). Named
// here so this file never spells a bare literal that could drift from the schema.
const (
	codexAuthModeSubscription = "subscription"
	codexAuthModeAPIKey       = "api_key"
)

// codexActivelyClaimedStatuses is the CLOSED set of run statuses under which a Codex
// credential operation may be authorized (PRD #1147 M2, B5 check 4). It is the
// "actively claimed by a live worker" set: the worker holds the run and is (or is
// about to be) executing.
//
// parked states ARE included on purpose — 'limit_wait' and the awaiting_* parks are a
// worker still holding the run, and D4 requires persist-before-park, so a park must be
// able to persist recovery material. The following are deliberately EXCLUDED, all on the
// same principle (no worker is actively executing the run, so no live capability should
// be honored): 'queued' — the requeue gap where the run was handed back and no worker
// owns it (the capability is revoked and the epoch bumped on that transition);
// 'pool_wait' — reached only when claim assembly aborts on an empty auto-pool BEFORE the
// codex branch mints a capability, so it is a not-yet-executing wait like 'queued', not a
// worker-attached park (a codex run therefore never carries a live cap in this state, and
// excluding it keeps the predicate correct if codex is later wired onto the auto-pool
// lane); and the terminal states ('completed'/'failed'/'cancelled') — a finished run
// spends nothing.
var codexActivelyClaimedStatuses = map[string]bool{
	"claimed":           true,
	"running":           true,
	"awaiting_approval": true,
	"awaiting_input":    true,
	"awaiting_followup": true,
	"limit_wait":        true,
}

// CodexOpScope is one of the three DISTINCT credential-operation scopes a per-claim
// capability may be authorized for (PRD #1147 M2, B5). Holding a capability does NOT
// grant every operation: the scope is a required parameter of the authority check and
// each scope is validated independently (see AuthorizeCodexCredentialOp), so authority
// for one scope never implies another. The zero value is deliberately invalid so an
// unset scope can never authorize anything.
type CodexOpScope int

const (
	// ScopePersistRecovery: persist a previously-good login into the account recovery
	// slot before parking (D4 persist-before-park). Subscription only — an api_key
	// alias has no rotating login to protect.
	ScopePersistRecovery CodexOpScope = iota + 1
	// ScopeReleaseAccessToken: release the run's currently-usable access token (the
	// subscription access_token, or the static api_key). Valid for BOTH auth modes.
	ScopeReleaseAccessToken
	// ScopeStartRefresh: begin a coordinated refresh of the account's login.
	// Subscription only — a static api_key is not refreshable.
	ScopeStartRefresh
)

// valid reports whether the scope is one of the three known scopes.
func (s CodexOpScope) valid() bool {
	return s >= ScopePersistRecovery && s <= ScopeStartRefresh
}

func (s CodexOpScope) String() string {
	switch s {
	case ScopePersistRecovery:
		return "persist_recovery"
	case ScopeReleaseAccessToken:
		return "release_access_token"
	case ScopeStartRefresh:
		return "start_refresh"
	default:
		return "unknown"
	}
}

// appliesTo reports whether this scope may be authorized for the given auth mode. This
// is what makes the three scopes independent per operation rather than one bit: a
// capability that authorizes ScopeReleaseAccessToken on an api_key run does NOT
// authorize ScopeStartRefresh on it (a static key cannot be refreshed), so one scope's
// authorization can never be reused for another.
func (s CodexOpScope) appliesTo(authMode string) bool {
	switch s {
	case ScopeReleaseAccessToken:
		// A token to release exists in both modes.
		return authMode == codexAuthModeSubscription || authMode == codexAuthModeAPIKey
	case ScopePersistRecovery, ScopeStartRefresh:
		// Recovery/refresh act on a rotating subscription login; a static api_key has
		// neither.
		return authMode == codexAuthModeSubscription
	default:
		return false
	}
}

// Codex credential-operation authority sentinels (PRD #1147 M2). Each reject reason is
// a distinct exported sentinel so a caller (and the tests) can assert WHY authority was
// refused without string-matching, mirroring the ErrCodex* reconciliation sentinels.
var (
	// ErrCodexRunNotBound: the run is not Codex-bound (codex_secret_id NULL, so the
	// auth-context join returns no row) or carries no/invalid frozen auth mode.
	ErrCodexRunNotBound = errors.New("run is not codex-bound")
	// ErrCodexScopeNotApplicable: the requested scope is unknown, or does not apply to
	// the run's auth mode (e.g. start-refresh / persist-recovery on a static api_key).
	ErrCodexScopeNotApplicable = errors.New("codex operation scope not applicable to this run")
	// ErrCodexCapabilityMismatch: the presented capability is malformed, or its secret
	// does not match the run's stored capability hash (constant-time compared), or the
	// run holds no capability hash.
	ErrCodexCapabilityMismatch = errors.New("codex capability does not match")
	// ErrCodexCapabilityEpoch: the presented capability was minted under a prior claim
	// epoch (a requeue/re-mint has since bumped codex_claim_epoch), so it is stale even
	// if its secret would otherwise hash-match.
	ErrCodexCapabilityEpoch = errors.New("codex capability is from a prior claim epoch")
	// ErrCodexWorkerMismatch: the run is not currently owned by the presenting worker.
	ErrCodexWorkerMismatch = errors.New("run is not owned by this worker")
	// ErrCodexRunNotActivelyClaimed: the run is not in an actively-claimed status
	// (queued — the requeue gap — or terminal).
	ErrCodexRunNotActivelyClaimed = errors.New("run is not in an actively-claimed state")
	// ErrCodexMaterialRevisionStale: the alias's material_revision has advanced since
	// the run froze it (the alias was manually replaced), invalidating the run.
	ErrCodexMaterialRevisionStale = errors.New("codex alias material revision is stale")
	// ErrCodexAccountKeyUnfrozen: a subscription run has no frozen account identity
	// tuple yet (NULL codex_account_key) — not-yet-runnable. Rejected so a later
	// replacement that resolves to a different account cannot retarget the run.
	ErrCodexAccountKeyUnfrozen = errors.New("codex run account identity is not frozen")
	// ErrCodexAccountTupleMismatch: the run's frozen account identity tuple no longer
	// equals the account's current (provider_user_id, workspace_account_id) — a
	// replacement resolved to a different account.
	ErrCodexAccountTupleMismatch = errors.New("codex account identity tuple has changed")
	// ErrCodexAccountRevisionStale: the account's credential_revision has advanced since
	// the run froze it — a genuine account-level revoke.
	ErrCodexAccountRevisionStale = errors.New("codex account credential revision is stale")
	// errCodexStoreUnavailable: the service's store does not expose the Codex query
	// surface (a test store, or a misconfiguration). Never happens with *store.Queries.
	errCodexStoreUnavailable = errors.New("codex query surface unavailable")
)

// codexAuthzStore is the narrow query surface the Codex authority check, binding freeze
// and claim-payload assembly use. *store.Queries satisfies it; the production Service's
// s.q (a Store) is a *store.Queries underneath, reached via codexStore() below. The
// separate interface keeps this dark m2 surface off the broad Store interface (which
// would ripple into every workersvc fake) — the same narrowing codexcred.go uses for
// the reconciler.
type codexAuthzStore interface {
	GetRunCodexAuthContext(ctx context.Context, id uuid.UUID) (store.GetRunCodexAuthContextRow, error)
	GetCodexCredentialState(ctx context.Context, arg store.GetCodexCredentialStateParams) (store.CodexCredentialState, error)
	GetCodexProviderAccountByID(ctx context.Context, arg store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error)
	SetRunCodexClaimCapability(ctx context.Context, arg store.SetRunCodexClaimCapabilityParams) (int64, error)
	FreezeRunCodexBinding(ctx context.Context, arg store.FreezeRunCodexBindingParams) (int64, error)
	SetRunCodexFrozenIdentity(ctx context.Context, arg store.SetRunCodexFrozenIdentityParams) (int64, error)
}

// codexStore adapts the Service's Store to the narrow Codex query surface. It succeeds
// with the production *store.Queries and fails (false) with a store that lacks the
// codex queries, so the caller degrades safely rather than panicking.
func (s *Service) codexStore() (codexAuthzStore, bool) {
	q, ok := s.q.(codexAuthzStore)
	return q, ok
}

// mintCodexCapability mints a fresh per-claim credential-operation capability (PRD
// #1147 M2, B5): a high-entropy secret whose plaintext rides the claim payload and
// whose sha256 is all the server stores (runs.codex_cap_hash). The plaintext is the
// bare random secret; the epoch is stamped onto the wire form separately
// (formatCodexCapability) so mint stays a pure secret generator.
//
// A crypto/rand read failure is unrecoverable (the process cannot produce secure
// randomness) and must never degrade to a weak or empty capability, so it panics — the
// same posture the standard library documents for rand.Read consumers.
func mintCodexCapability() (plaintext string, hash []byte) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("workersvc: crypto/rand failed minting codex capability: %v", err))
	}
	plaintext = base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, hashCodexCapability(plaintext)
}

// hashCodexCapability returns the sha256 of a capability's plaintext secret — the only
// form the server persists or compares. It hashes the bare secret, NOT the epoch-tagged
// wire form (parse that with parseCodexCapability first).
func hashCodexCapability(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// formatCodexCapability builds the wire capability the worker receives and later
// presents: "<epoch>.<secret>". The epoch is carried in-band so the authority check can
// reject a capability minted under a prior claim epoch (a requeue bumps the epoch)
// independently of the hash compare, giving a distinct rejection reason.
func formatCodexCapability(epoch int64, plaintext string) string {
	return strconv.FormatInt(epoch, 10) + "." + plaintext
}

// parseCodexCapability splits a presented wire capability into its epoch and secret.
// ok is false for any malformed input (no separator, non-numeric epoch), which the
// caller maps to a capability mismatch.
func parseCodexCapability(presented string) (epoch int64, secret string, ok bool) {
	head, rest, found := strings.Cut(presented, ".")
	if !found || rest == "" {
		return 0, "", false
	}
	ep, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return ep, rest, true
}

// codexAccountKey serializes an identity tuple exactly as migration 00201 documents:
// a two-element JSON array TEXT, json.Marshal([]string{provider_user_id,
// workspace_account_id}). Both the frozen run column and the authority check's
// current-tuple comparison go through this one encoder, so the two sides can never
// serialize the same tuple differently.
func codexAccountKey(providerUserID, workspaceAccountID string) (string, error) {
	b, err := json.Marshal([]string{providerUserID, workspaceAccountID})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CodexAuthContext is the resolved authority a successful AuthorizeCodexCredentialOp
// grants: which scope it was authorized for, the run's auth mode, and enough to open
// the credential in the caller (the resolved provider account id for a subscription
// run, and always the alias secret id). The actual open/refresh/persist work is the
// caller's — this struct just proves the operation is authorized and hands over the
// resolved coordinates.
type CodexAuthContext struct {
	Scope     CodexOpScope
	AuthMode  string
	UserID    uuid.UUID
	RunID     uuid.UUID
	SecretID  uuid.UUID // the alias user_secrets id (opens the static key for api_key release)
	AccountID uuid.UUID // the resolved provider account id (subscription only; zero for api_key)
}

// AuthorizeCodexCredentialOp decides whether the presenting worker may perform a scoped
// credential operation over the run's frozen Codex binding (PRD #1147 M2, B5). It
// DERIVES the account/credential from the run binding alone: the request names only the
// worker, the run, the presented capability and the scope — never a user/secret/account
// id — so a worker cannot point an operation at a credential it was not handed.
//
// It returns a distinct sentinel unless ALL of the following hold (checked in order):
//  1. the run is Codex-bound with a valid auth mode, and the scope applies to that mode;
//  2. the presented capability is tied to the CURRENT claim: its in-band epoch equals
//     the run's codex_claim_epoch (a prior-epoch capability is rejected even if its
//     secret hash-matches), and its secret's sha256 constant-time-equals the run's
//     stored codex_cap_hash;
//  3. the run is currently owned by this worker;
//  4. the run is in an actively-claimed status (NOT queued, NOT terminal);
//  5. per-alias: the run's frozen material_revision still equals the alias's current
//     material_revision (a manual alias replace bumps it and invalidates the run);
//  6. subscription only, account-wide: the run's frozen account_revision equals the
//     account's current credential_revision AND the run's frozen identity tuple equals
//     the account's current (provider_user_id, workspace_account_id). A NULL frozen
//     tuple is not-yet-runnable and is rejected (so a replacement that resolves to a
//     different account cannot retarget the run).
func (s *Service) AuthorizeCodexCredentialOp(ctx context.Context, wkr store.Worker, runID uuid.UUID, presentedCapability string, scope CodexOpScope) (CodexAuthContext, error) {
	q, ok := s.codexStore()
	if !ok {
		return CodexAuthContext{}, errCodexStoreUnavailable
	}
	if !scope.valid() {
		return CodexAuthContext{}, ErrCodexScopeNotApplicable
	}

	row, err := q.GetRunCodexAuthContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row means the run is not Codex-bound (the state join found nothing).
			return CodexAuthContext{}, ErrCodexRunNotBound
		}
		return CodexAuthContext{}, fmt.Errorf("codex authorize: read auth context: %w", err)
	}

	// (1) bound + valid mode + scope applies to the mode.
	if !row.CodexSecretID.Valid || !row.CodexAuthMode.Valid {
		return CodexAuthContext{}, ErrCodexRunNotBound
	}
	authMode := row.CodexAuthMode.String
	if authMode != codexAuthModeSubscription && authMode != codexAuthModeAPIKey {
		return CodexAuthContext{}, ErrCodexRunNotBound
	}
	if !scope.appliesTo(authMode) {
		return CodexAuthContext{}, ErrCodexScopeNotApplicable
	}

	// (2) currently-owning worker — checked BEFORE the capability so a caller who does
	// not own the run cannot use the distinct capability/epoch sentinels as an oracle to
	// learn another user's claim epoch (GetRunCodexAuthContext reads by run id, so the
	// row belongs to whatever run the guessed id names; the ownership gate is the tenant
	// boundary and must precede any capability-specific reject).
	if !row.WorkerID.Valid || uuid.UUID(row.WorkerID.Bytes) != wkr.ID {
		return CodexAuthContext{}, ErrCodexWorkerMismatch
	}

	// (3) capability: epoch first (a stale-epoch capability is rejected even if its
	// hash was cleared or would otherwise match), then the constant-time hash compare.
	presentedEpoch, secret, parsed := parseCodexCapability(presentedCapability)
	if !parsed {
		return CodexAuthContext{}, ErrCodexCapabilityMismatch
	}
	if presentedEpoch != row.CodexClaimEpoch {
		return CodexAuthContext{}, ErrCodexCapabilityEpoch
	}
	if len(row.CodexCapHash) == 0 {
		return CodexAuthContext{}, ErrCodexCapabilityMismatch
	}
	if subtle.ConstantTimeCompare(hashCodexCapability(secret), row.CodexCapHash) != 1 {
		return CodexAuthContext{}, ErrCodexCapabilityMismatch
	}

	// (4) actively-claimed status (NOT queued, NOT terminal).
	if !codexActivelyClaimedStatuses[row.Status] {
		return CodexAuthContext{}, ErrCodexRunNotActivelyClaimed
	}

	// (5) per-alias material revision (protects staging aliases and static api keys,
	// which have no account row).
	if !row.CodexMaterialRevision.Valid || row.CodexMaterialRevision.Int64 != row.CurrentMaterialRevision {
		return CodexAuthContext{}, ErrCodexMaterialRevisionStale
	}

	authCtx := CodexAuthContext{
		Scope:    scope,
		AuthMode: authMode,
		UserID:   wkr.UserID,
		RunID:    runID,
		SecretID: uuid.UUID(row.CodexSecretID.Bytes),
	}

	// (6) account-wide checks, subscription only. An api_key alias has no account row
	// (the LEFT join is NULL), so there is nothing account-level to compare.
	if authMode == codexAuthModeSubscription {
		if !row.CodexAccountKey.Valid {
			// NULL-frozen tuple: not-yet-runnable. Rejecting here is what stops a later
			// replacement from silently retargeting the run onto a different account.
			return CodexAuthContext{}, ErrCodexAccountKeyUnfrozen
		}
		if !row.ProviderUserID.Valid || !row.WorkspaceAccountID.Valid {
			// The account behind the alias vanished or the alias no longer resolves to
			// one — a replacement resolved elsewhere.
			return CodexAuthContext{}, ErrCodexAccountTupleMismatch
		}
		currentKey, kerr := codexAccountKey(row.ProviderUserID.String, row.WorkspaceAccountID.String)
		if kerr != nil {
			return CodexAuthContext{}, fmt.Errorf("codex authorize: encode account key: %w", kerr)
		}
		if row.CodexAccountKey.String != currentKey {
			return CodexAuthContext{}, ErrCodexAccountTupleMismatch
		}
		if !row.CodexAccountRevision.Valid || !row.CurrentCredentialRevision.Valid ||
			row.CodexAccountRevision.Int64 != row.CurrentCredentialRevision.Int64 {
			return CodexAuthContext{}, ErrCodexAccountRevisionStale
		}
		// Resolve the account id for the caller (owner-scoped via the state row).
		st, serr := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
			UserSecretID: authCtx.SecretID,
			UserID:       wkr.UserID,
		})
		if serr != nil {
			return CodexAuthContext{}, fmt.Errorf("codex authorize: resolve account: %w", serr)
		}
		if !st.ProviderAccountID.Valid {
			// Linked-then-unlinked between the auth read and here: treat as not-runnable.
			return CodexAuthContext{}, ErrCodexAccountKeyUnfrozen
		}
		authCtx.AccountID = uuid.UUID(st.ProviderAccountID.Bytes)
	}

	return authCtx, nil
}

// FreezeCodexBinding freezes a run's Codex binding at creation (PRD #1147 M2, B6),
// owner-scoped. It resolves the alias's CURRENT material_revision and, for a linked
// subscription alias, the account's credential_revision and identity tuple, and pins
// them onto the run atomically via FreezeRunCodexBinding (+ SetRunCodexFrozenIdentity
// when the identity is known) so the authority check can later compare run-frozen vs
// current.
//
// This is an INTERNAL create path — no production path creates a Codex-bound run yet
// (m2 ships DARK), so it is exercised only by tests/fixtures and is deliberately kept
// out of the public CreateRun/schedule/chat paths.
func (s *Service) FreezeCodexBinding(ctx context.Context, userID, runID, secretID uuid.UUID, authMode string) error {
	if authMode != codexAuthModeSubscription && authMode != codexAuthModeAPIKey {
		return fmt.Errorf("%w: %q", ErrCodexRunNotBound, authMode)
	}
	q, ok := s.codexStore()
	if !ok {
		return errCodexStoreUnavailable
	}

	// Current alias material_revision (the value the run freezes at creation).
	st, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: secretID,
		UserID:       userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: alias %s", ErrCodexStateMissing, secretID)
		}
		return fmt.Errorf("codex freeze: read state: %w", err)
	}

	// Snapshot the alias label at bind time (the same reason 00086/00201 snapshot it:
	// the FK nulls the id on delete and a rename rewrites the label in place).
	meta, err := s.q.GetUserSecretMetaByID(ctx, store.GetUserSecretMetaByIDParams{
		ID:     secretID,
		UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: alias %s", ErrCodexStateMissing, secretID)
		}
		return fmt.Errorf("codex freeze: read alias meta: %w", err)
	}

	n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID:         secretID,
		AuthMode:         authMode,
		SecretLabel:      meta.Label,
		MaterialRevision: st.MaterialRevision,
		// The identity tuple/revision are frozen by SetRunCodexFrozenIdentity below when
		// known; a run whose subscription account is not yet linked stays unfrozen (and
		// so not-yet-runnable) until then.
		AccountKey:      pgtype.Text{},
		AccountRevision: pgtype.Int8{},
		ID:              runID,
		UserID:          userID,
	})
	if err != nil {
		return fmt.Errorf("codex freeze: freeze binding: %w", err)
	}
	if n == 0 {
		return errRunVanished
	}

	// For a linked subscription alias, freeze the identity tuple + account revision now
	// so the authority check has a frozen baseline. A staging alias (no account yet) and
	// every api_key alias leave the tuple NULL — which the authority check treats as
	// not-yet-runnable for subscription, and simply skips for api_key.
	if authMode == codexAuthModeSubscription && st.ProviderAccountID.Valid {
		acct, aerr := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{
			UserID: userID,
			ID:     uuid.UUID(st.ProviderAccountID.Bytes),
		})
		if aerr != nil {
			return fmt.Errorf("codex freeze: read account: %w", aerr)
		}
		key, kerr := codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID)
		if kerr != nil {
			return fmt.Errorf("codex freeze: encode account key: %w", kerr)
		}
		m, ierr := q.SetRunCodexFrozenIdentity(ctx, store.SetRunCodexFrozenIdentityParams{
			Key:    key,
			Rev:    acct.CredentialRevision,
			ID:     runID,
			UserID: userID,
		})
		if ierr != nil {
			return fmt.Errorf("codex freeze: freeze identity: %w", ierr)
		}
		if m == 0 {
			return errRunVanished
		}
	}
	return nil
}

// codexClaimSecrets builds the Codex block of a claim payload for a Codex-bound run
// (PRD #1147 M2, B7). It mints a fresh per-claim capability (worker-scoped mint, so a
// worker that already lost the claim cannot mint), opens the run's usable credential
// for its auth mode — the subscription access_token extracted from the account's sealed
// merged login, or the static api_key — and returns both. It NEVER extracts or ships
// the refresh/login blob.
//
// Called only for a Codex-bound run (run.CodexSecretID.Valid); an ordinary Claude run
// skips this entirely and its claim JSON stays byte-identical.
func (s *Service) codexClaimSecrets(ctx context.Context, wkr store.Worker, run store.Run) (*ClaimCodexSecrets, error) {
	q, ok := s.codexStore()
	if !ok {
		return nil, errCodexStoreUnavailable
	}
	if !run.CodexAuthMode.Valid {
		return nil, ErrCodexRunNotBound
	}
	authMode := run.CodexAuthMode.String
	secretID := uuid.UUID(run.CodexSecretID.Bytes)

	// Mint the per-claim capability and store its hash worker-scoped, bumping the epoch.
	// 0 rows means this worker no longer owns the run (a requeue reclaimed it between the
	// claim read and here) — treat it as the run vanishing under us.
	plaintext, hash := mintCodexCapability()
	n, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash:     hash,
		ID:       run.ID,
		WorkerID: pgconv.UUID(wkr.ID),
	})
	if err != nil {
		return nil, fmt.Errorf("codex claim: mint capability: %w", err)
	}
	if n == 0 {
		return nil, errRunVanished
	}
	// The mint bumped the epoch by exactly one from the value on the loaded run row (this
	// worker owns the run, so no other writer advanced it in between).
	wireCap := formatCodexCapability(run.CodexClaimEpoch+1, plaintext)

	var accessToken string
	switch authMode {
	case codexAuthModeSubscription:
		st, serr := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
			UserSecretID: secretID,
			UserID:       run.UserID,
		})
		if serr != nil {
			return nil, fmt.Errorf("codex claim: read state: %w", serr)
		}
		if !st.ProviderAccountID.Valid {
			return nil, fmt.Errorf("%w: subscription alias not linked", errCredentialUnavailable)
		}
		acct, aerr := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{
			UserID: run.UserID,
			ID:     uuid.UUID(st.ProviderAccountID.Bytes),
		})
		if aerr != nil {
			return nil, fmt.Errorf("codex claim: read account: %w", aerr)
		}
		plain, oerr := secretopen.OpenSealed(s.vlt, s.box, run.UserID, store.KindCodexAuth, acct.SealedWith, acct.SealedLogin)
		if oerr != nil {
			if errors.Is(oerr, secretopen.ErrVaultLocked) {
				return nil, errVaultLocked
			}
			return nil, fmt.Errorf("%w: codex login could not be decrypted", errCredentialUnavailable)
		}
		var blob codexLoginBlob
		if jerr := json.Unmarshal(plain, &blob); jerr != nil {
			return nil, fmt.Errorf("%w: %v", ErrCodexLoginBlob, jerr)
		}
		if blob.AccessToken == "" {
			return nil, fmt.Errorf("%w: missing access token", ErrCodexLoginBlob)
		}
		accessToken = blob.AccessToken
	case codexAuthModeAPIKey:
		tok, oerr := secretopen.OpenByID(ctx, s.q, s.vlt, s.box, run.UserID, secretID)
		switch {
		case oerr == nil:
			accessToken = string(tok)
		case errors.Is(oerr, secretopen.ErrVaultLocked):
			return nil, errVaultLocked
		case errors.Is(oerr, secretopen.ErrNoSecret), errors.Is(oerr, secretopen.ErrUndecryptable):
			return nil, fmt.Errorf("%w: codex api key could not be opened", errCredentialUnavailable)
		default:
			return nil, oerr
		}
	default:
		return nil, ErrCodexRunNotBound
	}

	return &ClaimCodexSecrets{AccessToken: accessToken, Capability: wireCap}, nil
}
