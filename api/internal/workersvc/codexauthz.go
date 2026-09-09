package workersvc

import (
	"bytes"
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

// Codex auth modes (the runs.codex_auth_mode CHECK values, migration 00202). Named
// here so this file never spells a bare literal that could drift from the schema.
const (
	codexAuthModeSubscription = "subscription"
	codexAuthModeAPIKey       = "api_key"
)

// codexCoordQuarantined is the codex_provider_account.coord_state value (migration 00199)
// meaning a refresh left the account parked for reconciliation: no token may be handed out
// until it is reconciled or re-logged-in.
const codexCoordQuarantined = "quarantined"

// codexActivelyClaimedStatuses is the CLOSED set of run statuses under which a Codex
// credential operation may be authorized (PRD #1147 M2, B5 check 4). It is the
// "actively claimed by a live worker" set: the worker holds the run and is (or is
// about to be) executing.
//
// parked states ARE included on purpose — 'limit_wait', 'recovery_wait' (issue #1197) and
// the awaiting_* parks are a worker still holding the run, and D4 requires
// persist-before-park, so a park must be able to persist recovery material. recovery_wait
// is a MID-EXECUTION park like limit_wait (a running worker parked the run on a transient
// recovery), so it belongs here — unlike the pre-execution 'pool_wait' below. The following
// are deliberately EXCLUDED, all on the
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
	"recovery_wait":     true,
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
	// ErrCodexAccountQuarantined: the run's subscription account is parked in the
	// 'quarantined' coord_state (a refresh commit failed into the recovery slot). No
	// access token may be released or refresh started until it is reconciled or
	// re-logged-in — handing back its stale sealed_login would return an expired token as
	// a success. Deliberately DISTINCT from a revoke (ErrCodexAccountRevisionStale): the
	// account identity is unchanged, it is merely paused.
	ErrCodexAccountQuarantined = errors.New("codex account is quarantined; reconcile or re-login required")
	// ErrCodexKindModeMismatch: the alias's user_secrets.kind contradicts the run's frozen
	// auth mode (a 'subscription' run must be bound to a codex_auth alias, an 'api_key' run
	// to an openai_api_key alias). Rejected so an api_key release path can never be pointed
	// at a codex_auth login blob (which carries a refresh_token), and vice versa.
	ErrCodexKindModeMismatch = errors.New("codex alias kind contradicts the run's auth mode")
	// ErrCodexBindingConflict: a write-once freeze was refused because the run already
	// carries a DIFFERENT frozen binding/identity (the store guard affected 0 rows for a
	// run that DOES exist and is owned). Distinct from errRunVanished (a genuinely
	// gone/foreign run): an identical retry (same secret/tuple) is idempotent success, only
	// a conflicting re-point is refused.
	ErrCodexBindingConflict = errors.New("codex run binding is already frozen to a different credential")
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

// codexAccountKey serializes an identity tuple exactly as migration 00202 documents:
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

// codexReleaseInputs is the pure, database-free snapshot the release predicate decides
// over: the run's FROZEN binding facts alongside the CURRENT alias/account facts, so the
// predicate compares the two without touching a store. Every nullable column carries an
// explicit *Valid bool (rather than a pgtype) so the predicate stays a plain,
// exhaustively-unit-testable value function. The subscription-only fields are ignored for
// an api_key run (which has no account behind the LEFT join).
type codexReleaseInputs struct {
	authMode  string // runs.codex_auth_mode
	boundKind string // the alias's user_secrets.kind (owner-scoped join)
	status    string // runs.status

	frozenMaterialRev       int64 // runs.codex_material_revision
	frozenMaterialRevValid  bool
	currentMaterialRev      int64 // codex_credential_state.material_revision
	currentMaterialRevValid bool

	// subscription only.
	frozenAccountKey      string // runs.codex_account_key (the frozen identity tuple)
	frozenAccountKeyValid bool

	currentProviderUserID          string // codex_provider_account.provider_user_id
	currentProviderUserIDValid     bool
	currentWorkspaceAccountID      string // codex_provider_account.workspace_account_id
	currentWorkspaceAccountIDValid bool

	frozenAccountRev          int64 // runs.codex_account_revision
	frozenAccountRevValid     bool
	currentCredentialRev      int64 // codex_provider_account.credential_revision
	currentCredentialRevValid bool

	coordState      string // codex_provider_account.coord_state
	coordStateValid bool
}

// codexReleaseInputsFromAuthRow projects a GetRunCodexAuthContext row into the pure
// predicate inputs. It is the ONE place the row shape is mapped, so the release predicate
// and every caller decide over the identical snapshot. current_material_revision is a
// NOT-NULL column on the state row (the INNER join guarantees the row), so it is always
// valid.
func codexReleaseInputsFromAuthRow(row store.GetRunCodexAuthContextRow) codexReleaseInputs {
	return codexReleaseInputs{
		authMode:  row.CodexAuthMode.String,
		boundKind: row.BoundKind,
		status:    row.Status,

		frozenMaterialRev:       row.CodexMaterialRevision.Int64,
		frozenMaterialRevValid:  row.CodexMaterialRevision.Valid,
		currentMaterialRev:      row.CurrentMaterialRevision,
		currentMaterialRevValid: true,

		frozenAccountKey:      row.CodexAccountKey.String,
		frozenAccountKeyValid: row.CodexAccountKey.Valid,

		currentProviderUserID:          row.ProviderUserID.String,
		currentProviderUserIDValid:     row.ProviderUserID.Valid,
		currentWorkspaceAccountID:      row.WorkspaceAccountID.String,
		currentWorkspaceAccountIDValid: row.WorkspaceAccountID.Valid,

		frozenAccountRev:          row.CodexAccountRevision.Int64,
		frozenAccountRevValid:     row.CodexAccountRevision.Valid,
		currentCredentialRev:      row.CurrentCredentialRevision.Int64,
		currentCredentialRevValid: row.CurrentCredentialRevision.Valid,

		coordState:      row.CurrentCoordState.String,
		coordStateValid: row.CurrentCoordState.Valid,
	}
}

// codexCheckKindMode enforces the kind↔auth-mode consistency invariant: a 'subscription'
// run must be bound to a codex_auth alias, an 'api_key' run to an openai_api_key alias.
// Any other pairing (including an unknown mode) is ErrCodexKindModeMismatch.
func codexCheckKindMode(authMode, boundKind string) error {
	switch authMode {
	case codexAuthModeSubscription:
		if boundKind != store.KindCodexAuth {
			return ErrCodexKindModeMismatch
		}
	case codexAuthModeAPIKey:
		if boundKind != store.KindOpenAIAPIKey {
			return ErrCodexKindModeMismatch
		}
	default:
		return ErrCodexKindModeMismatch
	}
	return nil
}

// codexCheckActivelyClaimed rejects a run that is not in the actively-claimed set.
func codexCheckActivelyClaimed(status string) error {
	if !codexActivelyClaimedStatuses[status] {
		return ErrCodexRunNotActivelyClaimed
	}
	return nil
}

// codexCheckMaterialRev rejects a run whose frozen alias material_revision no longer
// equals the alias's current material_revision (a manual alias replace bumps it).
func codexCheckMaterialRev(in codexReleaseInputs) error {
	if !in.frozenMaterialRevValid || !in.currentMaterialRevValid ||
		in.frozenMaterialRev != in.currentMaterialRev {
		return ErrCodexMaterialRevisionStale
	}
	return nil
}

// codexCheckAccountTuple enforces that a subscription run's frozen identity tuple is
// present and still equals the account's current (provider_user_id, workspace_account_id).
// A NULL frozen tuple is ErrCodexAccountKeyUnfrozen (not-yet-runnable); a NULL current
// tuple or a differing tuple is ErrCodexAccountTupleMismatch (a replacement resolved
// elsewhere). Both sides encode through the single codexAccountKey encoder so the two can
// never serialize the same tuple differently.
func codexCheckAccountTuple(in codexReleaseInputs) error {
	if !in.frozenAccountKeyValid {
		return ErrCodexAccountKeyUnfrozen
	}
	if !in.currentProviderUserIDValid || !in.currentWorkspaceAccountIDValid {
		return ErrCodexAccountTupleMismatch
	}
	currentKey, err := codexAccountKey(in.currentProviderUserID, in.currentWorkspaceAccountID)
	if err != nil {
		return fmt.Errorf("codex predicate: encode account key: %w", err)
	}
	if in.frozenAccountKey != currentKey {
		return ErrCodexAccountTupleMismatch
	}
	return nil
}

// evalCodexReleasePredicate is the SINGLE SOURCE OF TRUTH for "may this run be handed /
// act on its credential right now" — the full authority set the release-family scopes
// (release-access-token, start-refresh) and the claim-payload open both require (PRD #1147
// audit #3/#3a/#6). It is a pure function so the whole matrix is unit-testable without a
// database. The checks run in order, each returning its distinct sentinel:
//
//  1. kind↔mode consistency (subscription↔codex_auth, api_key↔openai_api_key);
//  2. actively-claimed status;
//  3. per-alias material_revision (frozen == current);
//  4. subscription only: frozen identity tuple present + equal, account credential
//     revision (frozen == current), and coord_state != 'quarantined'.
//
// api_key rows skip the account tuple/revision/quarantine checks — a static key has no
// account behind it.
func evalCodexReleasePredicate(in codexReleaseInputs) error {
	if err := codexCheckKindMode(in.authMode, in.boundKind); err != nil {
		return err
	}
	if err := codexCheckActivelyClaimed(in.status); err != nil {
		return err
	}
	if err := codexCheckMaterialRev(in); err != nil {
		return err
	}
	if in.authMode == codexAuthModeSubscription {
		if err := codexCheckAccountTuple(in); err != nil {
			return err
		}
		if !in.frozenAccountRevValid || !in.currentCredentialRevValid ||
			in.frozenAccountRev != in.currentCredentialRev {
			return ErrCodexAccountRevisionStale
		}
		if in.coordStateValid && in.coordState == codexCoordQuarantined {
			return ErrCodexAccountQuarantined
		}
	}
	return nil
}

// evalCodexPersistRecoveryPredicate is the DELIBERATELY REDUCED authority set for
// ScopePersistRecovery (PRD #1147 audit #3, directive). It keeps ownership-adjacent
// authority — actively-claimed status, per-alias material_revision, and (subscription) the
// identity tuple match — but DELIBERATELY SKIPS credential_revision, quarantine, and the
// kind↔mode check that the full release predicate enforces.
//
// The asymmetry is the whole point: a run must still be able to persist recoverable
// material into the account's recovery slot BEFORE it parks (D4 persist-before-park), even
// after the account was revoked (credential_revision bumped) or while it is quarantined.
// Losing release/refresh permission must NOT also strip the authority to PROTECT material
// the run already holds — that would turn a revoke or a quarantine into data loss. The
// tuple match is kept because persist-recovery still acts on a SPECIFIC account and must
// not be redirected to a different one; only the "may I still USE it" gates are dropped.
func evalCodexPersistRecoveryPredicate(in codexReleaseInputs) error {
	if err := codexCheckActivelyClaimed(in.status); err != nil {
		return err
	}
	if err := codexCheckMaterialRev(in); err != nil {
		return err
	}
	if in.authMode == codexAuthModeSubscription {
		if err := codexCheckAccountTuple(in); err != nil {
			return err
		}
	}
	return nil
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
//  2. the run is currently owned by this worker (checked BEFORE the capability so a
//     non-owner cannot use the capability/epoch sentinels as an oracle for another
//     user's claim epoch);
//  3. the presented capability is tied to the CURRENT claim: its in-band epoch equals
//     the run's codex_claim_epoch (a prior-epoch capability is rejected even if its
//     secret hash-matches), and its secret's sha256 constant-time-equals the run's
//     stored codex_cap_hash;
//  4. the scope-family predicate holds (evalCodexReleasePredicate for the release family,
//     evalCodexPersistRecoveryPredicate for persist-recovery), covering: kind↔mode
//     consistency (release family only), actively-claimed status, per-alias
//     material_revision, and — subscription only — the frozen identity tuple match,
//     account credential_revision (release family only) and NOT-quarantined (release
//     family only). The persist-recovery family DELIBERATELY skips credential_revision,
//     quarantine and kind so a run can still protect material before parking after a
//     revoke or while quarantined (audit #3).
//
// The distinct sentinels 4-6 of the former inline form (ErrCodexRunNotActivelyClaimed,
// ErrCodexMaterialRevisionStale, ErrCodexAccountKeyUnfrozen/TupleMismatch/RevisionStale)
// are preserved, plus the audit-added ErrCodexKindModeMismatch and
// ErrCodexAccountQuarantined, so callers/tests still branch on WHY authority was refused.
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

	// (4-6) The scope-family predicate. This is the SINGLE SOURCE OF TRUTH for the
	// frozen-vs-current authority set, replacing the former inline checks 4-6 AND adding
	// the audit-flagged quarantine + kind↔mode checks. Two families, deliberately
	// asymmetric (PRD #1147 audit #3):
	//
	//   - the RELEASE family (release-access-token, start-refresh) runs the FULL predicate
	//     — actively-claimed, material_revision, and (subscription) tuple + credential_
	//     revision + NOT quarantined + kind↔mode consistent.
	//   - ScopePersistRecovery runs a REDUCED predicate that DELIBERATELY SKIPS
	//     credential_revision, quarantine and kind↔mode: a run must still be able to
	//     persist recoverable material before parking even after a revoke or while
	//     quarantined (D4 persist-before-park). Losing release permission must not lose the
	//     authority to protect material. See evalCodexPersistRecoveryPredicate.
	inputs := codexReleaseInputsFromAuthRow(row)
	switch scope {
	case ScopePersistRecovery:
		if perr := evalCodexPersistRecoveryPredicate(inputs); perr != nil {
			return CodexAuthContext{}, perr
		}
	case ScopeReleaseAccessToken, ScopeStartRefresh:
		if perr := evalCodexReleasePredicate(inputs); perr != nil {
			return CodexAuthContext{}, perr
		}
	default:
		// Unreachable: scope.valid() and appliesTo() above already constrained the scope.
		return CodexAuthContext{}, ErrCodexScopeNotApplicable
	}

	authCtx := CodexAuthContext{
		Scope:    scope,
		AuthMode: authMode,
		UserID:   wkr.UserID,
		RunID:    runID,
		SecretID: uuid.UUID(row.CodexSecretID.Bytes),
	}

	// Resolve the account id for a subscription run (owner-scoped via the state row). The
	// predicate above already verified the tuple, so the account link is expected present;
	// an unlinked link between the auth read and here is treated as not-runnable.
	if authMode == codexAuthModeSubscription {
		st, serr := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
			UserSecretID: authCtx.SecretID,
			UserID:       wkr.UserID,
		})
		if serr != nil {
			return CodexAuthContext{}, fmt.Errorf("codex authorize: resolve account: %w", serr)
		}
		if !st.ProviderAccountID.Valid {
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

	// Snapshot the alias label at bind time (the same reason 00086/00202 snapshot it:
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

	// Kind↔auth-mode consistency, enforced BEFORE any store write (audit #6): a
	// 'subscription' binding must name a codex_auth alias, an 'api_key' binding an
	// openai_api_key alias. Freezing an openai_api_key alias as 'subscription' (or a
	// codex_auth alias as 'api_key') is refused here so the mismatch can never be persisted
	// onto the run in the first place.
	if kerr := codexCheckKindMode(authMode, meta.Kind); kerr != nil {
		return kerr
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
		// The write-once guard affected 0 rows. Distinguish a genuine conflict (the run
		// exists and is owned but already carries a DIFFERENT frozen secret) from a
		// vanished/foreign run: only the former is a binding conflict. An identical retry
		// (same secret) would have matched the guard and affected ≥1 row, so it never
		// reaches here.
		return s.codexFreezeZeroRows(ctx, runID, userID)
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
			// Same distinction as the binding freeze: 0 rows on the write-once identity
			// guard is a conflict when the run exists and is owned (its identity is already
			// frozen to a different tuple), else a vanished run.
			return s.codexFreezeZeroRows(ctx, runID, userID)
		}
	}
	return nil
}

// codexFreezeZeroRows maps a 0-row write-once freeze result to the right sentinel: a
// conflict (ErrCodexBindingConflict) when the run still exists and is owned by the user
// (so the 0 rows came from the immutability guard, not a missing run), else errRunVanished
// (a genuinely gone/foreign run). Kept as one helper so the binding freeze and the
// identity freeze classify a 0-row result identically.
func (s *Service) codexFreezeZeroRows(ctx context.Context, runID, userID uuid.UUID) error {
	_, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	switch {
	case err == nil:
		return ErrCodexBindingConflict
	case errors.Is(err, pgx.ErrNoRows):
		return errRunVanished
	default:
		return fmt.Errorf("codex freeze: classify zero-row freeze: %w", err)
	}
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

	// Enforce the FULL release predicate BEFORE minting a capability or opening anything
	// (PRD #1147 audit #3a): a run whose binding is stale, quarantined or kind-mismatched
	// must open NOTHING and mint NOTHING. This mirrors what a later
	// AuthorizeCodexCredentialOp(release) would decide, so the claim can never hand out a
	// credential the authority check would refuse. Ownership is asserted directly against
	// the auth-context row (the presenting worker must currently own the run).
	authRow, aerr := q.GetRunCodexAuthContext(ctx, run.ID)
	if aerr != nil {
		if errors.Is(aerr, pgx.ErrNoRows) {
			return nil, ErrCodexRunNotBound
		}
		return nil, fmt.Errorf("codex claim: read auth context: %w", aerr)
	}
	if !authRow.WorkerID.Valid || uuid.UUID(authRow.WorkerID.Bytes) != wkr.ID {
		return nil, ErrCodexWorkerMismatch
	}
	if perr := evalCodexReleasePredicate(codexReleaseInputsFromAuthRow(authRow)); perr != nil {
		return nil, perr
	}

	// Mint the per-claim capability and store its hash worker-scoped, bumping the epoch.
	// 0 rows means this worker no longer owns the run (a requeue reclaimed it between the
	// claim read and here) — treat it as the run vanishing under us.
	plaintext, hash := mintCodexCapability()
	epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash:     hash,
		ID:       run.ID,
		WorkerID: pgconv.UUID(wkr.ID),
	})
	if err != nil {
		// The :one mint returns pgx.ErrNoRows when no row matched — this worker no longer
		// owns the run (a requeue reclaimed it between the claim read and here); treat it
		// as the run vanishing under us.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errRunVanished
		}
		return nil, fmt.Errorf("codex claim: mint capability: %w", err)
	}
	// Wire the capability off the PERSISTED post-bump epoch the mint returned, not a
	// re-derived value: the stored epoch is authoritative.
	wireCap := formatCodexCapability(epoch, plaintext)

	// BARRIER A (PRD #1147 M2 defect-2): re-authorize against a FRESH snapshot taken AFTER
	// the mint, before opening anything. The pre-mint predicate (l777-789) read a single
	// unlocked snapshot; a quarantine/revoke/alias-swap/requeue that landed between that
	// read and the mint would otherwise still open and ship a now-wrong credential. This
	// re-check re-asserts worker-ownership + the full release predicate so a run whose
	// authority lapsed in that window opens NOTHING.
	if rerr := s.reauthorizeCodexRelease(ctx, q, run.ID, wkr, epoch, hash, false); rerr != nil {
		return nil, rerr
	}

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
		// Kind-guarded open (audit #6): a mis-bound codex_auth alias must NEVER disclose
		// its login blob (which carries a refresh_token) through the api_key path. The
		// release predicate above already rejects a kind mismatch; this is the deeper,
		// non-disclosing layer that holds even if that gate were ever bypassed — a
		// non-openai_api_key row returns the not-found sentinel and NO bytes.
		tok, oerr := secretopen.OpenByIDOfKind(ctx, s.q, s.vlt, s.box, run.UserID, secretID, store.KindOpenAIAPIKey)
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

	// BARRIER B (PRD #1147 M2 defect-2): the plaintext is now decrypted but NOT yet
	// returned. Re-authorize one last time against a FRESH snapshot AND bind the decision
	// to the capability THIS call minted: release the plaintext only if the release
	// predicate still holds, this worker still owns the run, and the run's live
	// codex_claim_epoch / codex_cap_hash are still exactly the ones the mint installed. The
	// decrypt itself opens a window (the subscription branch reads state + account and
	// decrypts; the api_key branch opens strictly by secretID with no revision compare), so
	// a same-worker concurrent RE-MINT (which advances codex_claim_epoch and clears/rewrites
	// cap_hash while leaving status 'claimed' and worker_id unchanged, thus passing ownership
	// + predicate) or an alias/account mutation that landed during the open is caught here
	// before any plaintext leaves the function. (A true requeue moves status to 'queued' and
	// is already rejected by the actively-claimed predicate above — Barrier A / this read's
	// evalCodexReleasePredicate — never reaching the epoch/hash compare.)
	if rerr := s.reauthorizeCodexRelease(ctx, q, run.ID, wkr, epoch, hash, true); rerr != nil {
		return nil, rerr
	}

	return &ClaimCodexSecrets{AccessToken: accessToken, Capability: wireCap}, nil
}

// reauthorizeCodexRelease re-reads a FRESH GetRunCodexAuthContext snapshot and re-verifies
// that the run still holds release authority, so codexClaimSecrets releases plaintext only
// if the authorization that held at the pre-mint check STILL holds now (PRD #1147 M2
// defect-2). It re-runs the full release predicate and the worker-ownership check against
// the fresh row. When checkCapability is true it additionally binds the decision to the
// capability THIS claim minted: the run's live codex_claim_epoch must still equal
// mintedEpoch and its codex_cap_hash must still bytes-equal mintedHash — so a concurrent
// same-worker re-mint (which advances the epoch and clears/rewrites the hash while leaving
// status 'claimed' and worker_id unchanged, and therefore passes both ownership and the
// predicate) is still rejected. This epoch/hash guard is the ONLY thing that catches that
// case; a true requeue (status to 'queued', capability revoked) is caught a step earlier by
// the actively-claimed predicate, so do NOT weaken that predicate assuming this covers it.
//
// Error mapping mirrors the initial read and stays compatible with claim_assembly.go's
// routing: pgx.ErrNoRows → ErrCodexRunNotBound; a failed predicate returns its own
// distinct sentinel; a lost ownership returns ErrCodexWorkerMismatch; a superseded
// epoch/hash returns errRunVanished (the capability we minted is no longer the live one —
// a same-worker re-mint superseded it). None of these is errVaultLocked, so a stale-state
// rejection never masquerades as a transient vault-lock requeue.
func (s *Service) reauthorizeCodexRelease(ctx context.Context, q codexAuthzStore, runID uuid.UUID, wkr store.Worker, mintedEpoch int64, mintedHash []byte, checkCapability bool) error {
	row, err := q.GetRunCodexAuthContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCodexRunNotBound
		}
		return fmt.Errorf("codex claim: re-read auth context: %w", err)
	}
	if perr := evalCodexReleasePredicate(codexReleaseInputsFromAuthRow(row)); perr != nil {
		return perr
	}
	if !row.WorkerID.Valid || uuid.UUID(row.WorkerID.Bytes) != wkr.ID {
		return ErrCodexWorkerMismatch
	}
	if checkCapability {
		// The capability we minted must still be the live one. A same-worker re-mint (status
		// stays 'claimed') advances codex_claim_epoch and clears or rewrites codex_cap_hash, so
		// either differing means the capability was superseded after we minted — treat it as the
		// run vanishing under us. (A true requeue is caught earlier by the actively-claimed
		// predicate; this guard exists for the status-stays-claimed re-mint the predicate misses.)
		if row.CodexClaimEpoch != mintedEpoch || !bytes.Equal(row.CodexCapHash, mintedHash) {
			return errRunVanished
		}
	}
	return nil
}
