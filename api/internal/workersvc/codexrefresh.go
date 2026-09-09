package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Codex COORDINATED-REFRESH state machine (PRD #1147 M2, B6), ships DARK. This is the
// SERVICE half over the m2-A store primitives (codex_binding.sql.go): it drives the
// lease/intent/generation dance that rotates ONE codex_provider_account's subscription
// login without ever letting two workers rotate in parallel, without ever returning an
// access token that is not durably committed, and without ever blindly re-spending a
// refresh token after a crash. The store primitives it stands on are the CAS-guarded
// AcquireCodexRefreshLease / CommitCodexRefresh, the durable codex_refresh_intent, and
// the SetCodexRecoverySlot / QuarantineExpiredCodexLease quarantine pair.
//
// m4 will PROVE this exhaustively with a fake provider and two concurrent clients; the
// tests here are representative (one client, a call-counting fake), not the full
// concurrency matrix.

// codexRefreshLeaseTTL bounds the complete API-side refresh lease. Pinned app-server has
// a fixed 10-second external-auth deadline; the worker reserves 1 second and gives its
// API request 8 seconds. A 7-second lease therefore leaves a real second for the HTTP
// response. Production provider calls are capped at 2.5 seconds each, leaving another
// second inside this lease for identity validation, the durable commit and authority recheck.
const codexRefreshLeaseTTL = 7 * time.Second

const (
	// Leave the handler enough time to encode and deliver the response after the service
	// returns. The handler's 7.5s context starts at request entry.
	codexRefreshResponseReserve = 500 * time.Millisecond
	// Stop provider work before the lease expires so identity verification, sealing, the
	// durable commit, and the final authority recheck retain a bounded slice.
	codexRefreshCommitReserve = time.Second
)

// Codex refresh-intent states (the codex_refresh_intent.state CHECK values, migration
// 00201). Named here so the state machine never spells a bare literal that could drift
// from the schema.
const (
	codexIntentRotating      = "rotating"
	codexIntentCommitted     = "committed"
	codexIntentUnrecoverable = "unrecoverable"
	codexIntentReconciled    = "reconciled"
	// codexIntentPending is an IN-MEMORY sentinel, NOT a DB state value (it is deliberately
	// the empty string so it can never satisfy the codex_refresh_intent.state CHECK). It is
	// returned by codexRefreshResolution for a still-LIVE, validly-leased, in-flight rotation
	// that the reconcile pass must LEAVE 'rotating' rather than resolve — the reconcile loop
	// recognizes it and skips the SetCodexRefreshIntentState write (PRD #1147 M4, defect 6).
	codexIntentPending = ""
)

// codexCoordInProgress is the codex_provider_account.coord_state value (migration 00199)
// meaning a live refresh lease is held: a rotation is in flight under coord_operation_id
// until lease_deadline. Named here so the reconcile pending predicate never spells a bare
// literal that could drift from the schema.
const codexCoordInProgress = "in_progress"

// Recovery-slot persistence resilience bounds (PRD #1147 M4, defect 4). A freshly-rotated
// (single-use) codex login must not be dropped because the REQUEST ctx was cancelled or a
// single transient DB write blipped: persistCodexRecoverySlot writes on a DETACHED context
// (context.WithoutCancel + a bounded WithTimeout) with a bounded synchronous retry, then
// hands continued retry off to s.background rather than declaring loss.
const (
	// codexRecoverySlotWriteTimeout bounds ONE SetCodexRecoverySlot attempt on the detached
	// context, so a wedged write cannot hang the caller (nor a background goroutine) forever.
	codexRecoverySlotWriteTimeout = 5 * time.Second
	// codexRecoverySlotSyncAttempts is how many times persistCodexRecoverySlot tries the
	// write synchronously (on the detached ctx) before handing off to the background retrier.
	codexRecoverySlotSyncAttempts = 2
	// codexRecoverySlotBgAttempts is how many times the background retrier tries before it
	// concludes loss is established and marks the intent unrecoverable.
	codexRecoverySlotBgAttempts = 6
	// codexRecoverySlotRetryBackoff spaces the retries; kept small so a synchronous test
	// s.background override completes promptly.
	codexRecoverySlotRetryBackoff = 20 * time.Millisecond
)

// errCodexRecoveryLost is the internal signal from persistCodexRecoverySlot that the
// recovery write's loss is ESTABLISHED synchronously (e.g. no background seam is wired to
// hand off to), so the caller must mark the intent unrecoverable. A transient/cancelled
// write NEVER produces it — it is handed off to the background retrier instead.
var errCodexRecoveryLost = errors.New("codex refresh: recovery material could not be persisted")

// errCodexRecoveryFenceLost means SetCodexRecoverySlot executed successfully but affected
// zero rows: the operation/generation fence moved, so no recovery copy was persisted and
// retrying the same stale write cannot become valid.
var errCodexRecoveryFenceLost = errors.New("codex refresh: recovery operation fence moved")

// CodexRefreshOutcome names how a CoordinatedCodexRefresh resolved. It is observability
// on top of the (result, error) contract: an ADVANCED/REPLAYED/RECONCILED outcome always
// rides a nil error and a usable access token; a CONTENDED/QUARANTINED outcome always
// rides its matching sentinel error and NO token (no token is ever released before a
// durable commit).
type CodexRefreshOutcome int

const (
	// CodexRefreshOutcomeUnknown is the zero value, never returned deliberately.
	CodexRefreshOutcomeUnknown CodexRefreshOutcome = iota
	// CodexRefreshAdvanced: this operation rotated the account to generation+1 and
	// returns the freshly-committed access token.
	CodexRefreshAdvanced
	// CodexRefreshReplayed: this operation already committed on a prior attempt (a
	// lost-reply); the committed access token is returned with NO new provider exchange.
	CodexRefreshReplayed
	// CodexRefreshReconciled: the caller was behind a completed rotation; the CURRENT
	// committed access token is returned with NO new provider exchange.
	CodexRefreshReconciled
	// CodexRefreshContended: another operation holds a live lease, or the account is
	// quarantined, or this operation's own prior attempt is still ambiguously in flight —
	// the caller must retry and reconcile rather than rotate in parallel. No token.
	CodexRefreshContended
	// CodexRefreshQuarantined: the provider exchange succeeded but the durable commit did
	// not; the new material was protected into the recovery slot and the account
	// quarantined for reconciliation. No token (it was never durably committed).
	CodexRefreshQuarantined
)

// Codex coordinated-refresh sentinels. Distinct so a caller (and the tests) can branch on
// the outcome without string-matching, mirroring the ErrCodex* authority sentinels.
var (
	// ErrCodexRefreshContended: the account is not this operation's to rotate right now
	// (a live lease, a quarantine, or an ambiguous prior attempt). Retryable: the caller
	// retries and reconciles to the committed generation. Rides CodexRefreshContended.
	ErrCodexRefreshContended = errors.New("codex refresh is contended; retry and reconcile")
	// ErrCodexRefreshQuarantined: the provider rotated but the commit could not be made
	// durable; the new material is protected in the recovery slot and the account is
	// quarantined. The caller must pause — NO access token is returned. Rides
	// CodexRefreshQuarantined.
	ErrCodexRefreshQuarantined = errors.New("codex refresh persisted to recovery and quarantined")
	// ErrCodexRefreshUnrecoverable: a prior attempt of this operation was resolved as
	// unrecoverable (the provider rotated, the commit was lost, and no recoverable copy
	// survives) — the account needs re-login. Never returns a token.
	ErrCodexRefreshUnrecoverable = errors.New("codex refresh operation is unrecoverable; re-login required")
	// ErrCodexRefreshNoClient: the service was not wired with a CodexRefreshClient (a
	// misconfiguration, or a test that reached the exchange without a fake). Never
	// touches the provider.
	ErrCodexRefreshNoClient = errors.New("codex refresh client is not configured")
	// ErrCodexRefreshNoToken: the account's sealed login carries no refresh token, so
	// there is nothing to exchange. Terminal for this attempt; never spends a call.
	ErrCodexRefreshNoToken = errors.New("codex account login has no refresh token to exchange")
	// errCodexObservedAhead: the caller's observed generation is HIGHER than the
	// account's current generation, which cannot happen for an honest caller (the account
	// is authoritative). Defensive; refuses to rotate on an impossible observation.
	errCodexObservedAhead = errors.New("observed generation is ahead of the account")
)

// CodexRefreshClient is the injectable oauth-exchange seam the coordinated refresher
// depends on (PRD #1147 M2). *codexauth.Client satisfies it; tests supply an in-process
// fake with a refresh counter and single-use rotating tokens.
//
// It EMBEDS CodexIdentityClient (codexcred.go) so the coordinated refresher can do BOTH
// halves of a hardened rotation: exchange the refresh token AND re-verify, with a
// nonrotating DiscoverIdentity, that the freshly-exchanged access token still resolves to
// the SAME account tuple it is rotating (PRD #1147 audit #2). *codexauth.Client already
// satisfies both methods, so this needs no codexauth edit and no new Service field. The
// interface lives here in workersvc rather than reaching into codexauth so the codexauth
// package needs no edit.
type CodexRefreshClient interface {
	CodexIdentityClient
	Refresh(ctx context.Context, refreshToken string) (codexauth.RefreshResult, error)
}

// CodexRefreshResult is the subscription-only outcome of a CoordinatedCodexRefresh /
// reconcile / replay. On success, ChatGPTAccountID is the provider-verified account id
// stored on the authoritative account row. It never comes from app-server's untrusted
// previousAccountId hint. AccessToken is non-empty ONLY when Outcome is
// ADVANCED/REPLAYED/RECONCILED and the returned error is nil; no secret or account data is
// set for a contended or quarantined outcome.
type CodexRefreshResult struct {
	AccessToken      string
	Generation       int64
	ChatGPTAccountID string
	Outcome          CodexRefreshOutcome
}

// CodexReleaseResult is a fresh authorized release of the run's selected credential.
// Subscription returns the committed generation and provider-verified ChatGPT account id;
// API-key mode leaves both absent. The handler encodes these as two disjoint wire shapes.
type CodexReleaseResult struct {
	AuthMode         string
	AccessToken      string
	Generation       *int64
	ChatGPTAccountID string
}

// codexRefreshStore is the narrow query surface the coordinated refresher + reconciler
// use over the m2-A primitives. *store.Queries satisfies it; tests may wrap it to force a
// CAS-lost or persistence-failure path. Kept off the broad Store interface for the same
// reason codexAuthzStore is (this dark surface would otherwise ripple into every fake).
type codexRefreshStore interface {
	GetCodexProviderAccountByID(ctx context.Context, arg store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error)
	GetCodexRefreshIntent(ctx context.Context, arg store.GetCodexRefreshIntentParams) (store.CodexRefreshIntent, error)
	InsertCodexRefreshIntent(ctx context.Context, arg store.InsertCodexRefreshIntentParams) (store.CodexRefreshIntent, error)
	SetCodexRefreshIntentState(ctx context.Context, arg store.SetCodexRefreshIntentStateParams) (int64, error)
	ListUnresolvedCodexRefreshIntents(ctx context.Context, arg store.ListUnresolvedCodexRefreshIntentsParams) ([]store.CodexRefreshIntent, error)
	AcquireCodexRefreshLease(ctx context.Context, arg store.AcquireCodexRefreshLeaseParams) (int64, error)
	CommitCodexRefresh(ctx context.Context, arg store.CommitCodexRefreshParams) (store.CommitCodexRefreshRow, error)
	ResetCodexCoordIdle(ctx context.Context, arg store.ResetCodexCoordIdleParams) (int64, error)
	SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error)
	QuarantineExpiredCodexLease(ctx context.Context, arg store.QuarantineExpiredCodexLeaseParams) (int64, error)
	// QuarantineCodexAccount parks an in-progress account that this op still owns WITHOUT
	// writing recovery material — the identity-mismatch path (audit #2), where the exchanged
	// material belongs to a DIFFERENT account and there is nothing trustworthy to protect.
	QuarantineCodexAccount(ctx context.Context, arg store.QuarantineCodexAccountParams) (int64, error)
	// PromoteCodexRecovery installs the identity-verified recovery material after it is
	// freshly re-sealed under the current key, CAS-guarded on from_generation.
	PromoteCodexRecovery(ctx context.Context, arg store.PromoteCodexRecoveryParams) (int64, error)
}

// codexRefreshQueries adapts the Service's Store to the narrow refresh surface, the same
// safe-degrade the authority check uses (false with a store that lacks the queries).
func (s *Service) codexRefreshQueries() (codexRefreshStore, bool) {
	q, ok := s.q.(codexRefreshStore)
	return q, ok
}

// codexNow returns the service clock (overridable in tests), falling back to time.Now
// when unset — the fixtures build a bare &Service{q,box} with no clock.
func (s *Service) codexNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// mergeCodexLogin builds the post-rotation login blob from the previous blob and a
// provider RefreshResult (PRD #1147 M2, B6 §6, rev-4 fix 1). The rules are exact and
// asymmetric:
//   - AccessToken is ALWAYS the freshly-exchanged one (Refresh guarantees it non-empty),
//     so a stale access token is never kept.
//   - RefreshToken is the newly-rotated one when the provider returned it (non-nil), ELSE
//     the PREVIOUS refresh token — the provider MAY omit a rotated refresh token, and
//     "keep the old one" is the correct, distinct answer to that (dropping it would brick
//     the account's ability to refresh again).
//
// A pure function so the merge rule is unit-testable without a database.
func mergeCodexLogin(prev codexLoginBlob, result codexauth.RefreshResult) codexLoginBlob {
	merged := codexLoginBlob{AccessToken: result.AccessToken, RefreshToken: prev.RefreshToken}
	if result.RefreshToken != nil {
		merged.RefreshToken = *result.RefreshToken
	}
	return merged
}

// openCodexSealed opens and decodes ANY codex login blob sealed on the shared vault path
// (AAD user_id||codex_auth) — the account's sealed_login OR its recovery_sealed, which are
// always produced by the same per-user seal. A locked vault surfaces as errVaultLocked
// (transient, retry); undecryptable/malformed material surfaces as errCredentialUnavailable
// / ErrCodexLoginBlob.
func (s *Service) openCodexSealed(userID uuid.UUID, sealed []byte, sealedWith string) (codexLoginBlob, error) {
	plain, err := secretopen.OpenSealed(s.vlt, s.box, userID, store.KindCodexAuth, sealedWith, sealed)
	if err != nil {
		if errors.Is(err, secretopen.ErrVaultLocked) {
			return codexLoginBlob{}, errVaultLocked
		}
		return codexLoginBlob{}, fmt.Errorf("%w: codex account login could not be decrypted", errCredentialUnavailable)
	}
	var blob codexLoginBlob
	if jerr := json.Unmarshal(plain, &blob); jerr != nil {
		return codexLoginBlob{}, fmt.Errorf("%w: %v", ErrCodexLoginBlob, jerr)
	}
	if blob.AccessToken == "" {
		return codexLoginBlob{}, fmt.Errorf("%w: missing access token", ErrCodexLoginBlob)
	}
	return blob, nil
}

// openCodexAccountLogin opens and decodes the account's CURRENT committed sealed_login.
func (s *Service) openCodexAccountLogin(userID uuid.UUID, acct store.CodexProviderAccount) (codexLoginBlob, error) {
	return s.openCodexSealed(userID, acct.SealedLogin, acct.SealedWith)
}

// sealCodexLogin seals a merged login blob for storage on the provider account, mirroring
// the reconciler's sealLogin: with a vault it seals under the user's DEK
// (sealed_with='dek', AAD user_id||codex_auth); without one (tests) it falls back to the
// master box (sealed_with='master').
// A locked vault surfaces as errVaultLocked (transient — the vault may unlock and a later
// pass can seal), mirroring openCodexSealed's secretopen.ErrVaultLocked → errVaultLocked
// mapping, so the advance path can classify a vault-locked seal as TRANSIENT and RETAIN the
// rotated material instead of declaring it lost (PRD #1147 M4, defect 4).
func (s *Service) sealCodexLogin(userID uuid.UUID, plaintext []byte) (sealed []byte, sealedWith string, err error) {
	if s.vlt != nil {
		sealed, err = s.vlt.Seal(userID, store.KindCodexAuth, plaintext)
		if errors.Is(err, vault.ErrLocked) {
			err = errVaultLocked
		}
		return sealed, store.SealedWithDEK, err
	}
	sealed, err = s.box.Seal(plaintext)
	return sealed, store.SealedWithMaster, err
}

// CoordinatedCodexRefresh runs the B6 refresh state machine for the subscription account
// backing the run's frozen binding (PRD #1147 M2). It authorizes ScopeStartRefresh,
// resolves the account, and then advances / replays / reconciles per the rev-3 rule:
//
//   - a prior attempt of THIS operation that already committed → REPLAY the committed
//     access token (no new exchange);
//   - an operation whose observedGeneration is behind the account's current generation →
//     RECONCILE to the current committed access token (no new exchange);
//   - otherwise (observedGeneration == account.generation, the current access token
//     expired) → ADVANCE: durable intent, lease, one provider exchange, merged reseal and
//     a CAS commit to generation+1.
//
// It NEVER returns an access token that was not durably committed, NEVER rotates two
// operations in parallel, and NEVER blindly re-spends a refresh token after a crash.
func (s *Service) CoordinatedCodexRefresh(ctx context.Context, wkr store.Worker, runID uuid.UUID, capability string, operationID uuid.UUID, observedGeneration int64) (CodexRefreshResult, error) {
	operationBudget := codexRefreshOperationBudget(ctx)
	if operationBudget <= codexRefreshCommitReserve {
		return CodexRefreshResult{}, context.DeadlineExceeded
	}
	operationDeadline := time.Now().Add(operationBudget)
	operationCtx, cancel := context.WithDeadline(ctx, operationDeadline)
	defer cancel()
	leaseDeadline := s.codexNow().Add(operationBudget)
	providerDeadline := operationDeadline.Add(-codexRefreshCommitReserve)

	// (1) Authorize ScopeStartRefresh. This is subscription-only, so a successful authCtx
	// carries the resolved account id; an api_key run is refused by the scope check.
	authCtx, err := s.AuthorizeCodexCredentialOp(operationCtx, wkr, runID, capability, ScopeStartRefresh)
	if err != nil {
		return CodexRefreshResult{}, err
	}
	res, err := s.coordinatedRefresh(operationCtx, authCtx.UserID, authCtx.AccountID, operationID, observedGeneration, leaseDeadline, providerDeadline)

	// (7) Post-exchange release recheck (audit #3b), the recheck-before-release idiom
	// ReleaseCodexCredential uses. CoordinatedCodexRefresh authorized ScopeStartRefresh
	// ONLY before the network; ownership/authority can be lost mid-IO (a requeue, a revoke,
	// an alias replace) while the exchange is in flight. The durable commit still STANDS —
	// it is good for the account, and the next authorized run reconciles to it — but a token
	// is released to THIS run only if it STILL holds authority. On a lost recheck, DISCARD
	// the token and refuse this run as contended; no token that a no-longer-owning run could
	// use ever leaves this call.
	if err == nil && res.AccessToken != "" {
		if _, rerr := s.AuthorizeCodexCredentialOp(operationCtx, wkr, runID, capability, ScopeStartRefresh); rerr != nil {
			return CodexRefreshResult{Outcome: CodexRefreshContended}, rerr
		}
	}
	return res, err
}

func codexRefreshOperationBudget(ctx context.Context) time.Duration {
	budget := codexRefreshLeaseTTL
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline) - codexRefreshResponseReserve
		if remaining < budget {
			budget = remaining
		}
	}
	return budget
}

// coordinatedRefresh is the post-authorization core, keyed on the resolved (user,
// account). Split out so its failure paths (CAS lost, persistence failure) are testable
// through a wrapped store without re-deriving the authority every time.
func (s *Service) coordinatedRefresh(ctx context.Context, userID, accountID, operationID uuid.UUID, observedGeneration int64, leaseDeadline, providerDeadline time.Time) (CodexRefreshResult, error) {
	q, ok := s.codexRefreshQueries()
	if !ok {
		return CodexRefreshResult{}, errCodexStoreUnavailable
	}

	acct, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: read account: %w", err)
	}

	// (2) Replay: a terminal intent for THIS operation is an exact idempotent answer.
	intent, ierr := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{OperationID: operationID, UserID: userID})
	switch {
	case ierr == nil:
		switch intent.State {
		case codexIntentCommitted, codexIntentReconciled:
			return s.codexReturnCommitted(userID, acct, CodexRefreshReplayed)
		case codexIntentUnrecoverable:
			return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, ErrCodexRefreshUnrecoverable
		case codexIntentRotating:
			// A prior attempt of this op started but did not finish. Do NOT re-exchange:
			// if the account advanced past the intent's from_generation a commit landed →
			// reconcile; otherwise the outcome is ambiguous → contended (a survivor
			// reconciles it, never a blind re-spend of the old refresh token).
			if acct.Generation > intent.FromGeneration {
				return s.codexReturnCommitted(userID, acct, CodexRefreshReconciled)
			}
			return CodexRefreshResult{Outcome: CodexRefreshContended}, ErrCodexRefreshContended
		default:
			return CodexRefreshResult{}, fmt.Errorf("codex refresh: unknown intent state %q", intent.State)
		}
	case errors.Is(ierr, pgx.ErrNoRows):
		// No prior intent for this op — first attempt. Fall through to the advance rule.
	default:
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: read intent: %w", ierr)
	}

	// (2, cont.) Reconcile / advance rule (rev-3 fix 3).
	switch {
	case observedGeneration > acct.Generation:
		// Impossible for an honest caller (the account is authoritative). Refuse.
		return CodexRefreshResult{}, fmt.Errorf("%w: observed=%d current=%d", errCodexObservedAhead, observedGeneration, acct.Generation)
	case observedGeneration < acct.Generation:
		// The caller is behind a completed rotation → reconcile to the current committed
		// generation with NO new exchange.
		return s.codexReturnCommitted(userID, acct, CodexRefreshReconciled)
	}

	// observedGeneration == acct.Generation → ADVANCE (steps 3-6).
	return s.advanceCodexRefresh(ctx, q, userID, accountID, operationID, acct, leaseDeadline, providerDeadline)
}

// advanceCodexRefresh performs the rotation itself (PRD #1147 M2, B6 §3-6): durable intent,
// lease, one provider exchange, merged reseal, CAS commit. The durable intent is recorded
// BEFORE the lease so a same-operationID concurrent retry is caught (23505) before any lease
// is held and can never strand it. `acct` is the account read at generation ==
// observedGeneration (the generation this rotation advances FROM).
func (s *Service) advanceCodexRefresh(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID, acct store.CodexProviderAccount, leaseDeadline, providerDeadline time.Time) (CodexRefreshResult, error) {
	// Extract the refresh token FIRST (a local read that mutates nothing). Hoisting it
	// ahead of the intent/lease means a locked vault or a login with no refresh token
	// fails cleanly — no durable intent left behind that a survivor would have to
	// (conservatively) resolve as unrecoverable, and no lease taken. The provider is still
	// called exactly once, after the durable intent, preserving the crash-window guarantee.
	prev, err := s.openCodexAccountLogin(userID, acct)
	if err != nil {
		return CodexRefreshResult{}, err
	}
	if prev.RefreshToken == "" {
		return CodexRefreshResult{}, ErrCodexRefreshNoToken
	}
	if s.codexRefresh == nil {
		return CodexRefreshResult{}, ErrCodexRefreshNoClient
	}

	// (3) Durable pre-rotation intent, recorded BEFORE the lease is acquired. Ordering the
	// intent ahead of the lease is what makes a same-operationID concurrent retry safe: a
	// duplicate op (23505) is detected on THIS insert, before any lease is held, so the
	// replay path (codexReplayAfterDuplicate) can never strand a freshly-acquired lease and
	// quarantine the account. Any non-duplicate insert failure (e.g. DB unavailable) means
	// the intent is not durable, so neither the lease nor the provider is touched — the
	// current refresh token stays valid and nothing is lost.
	if _, err := q.InsertCodexRefreshIntent(ctx, store.InsertCodexRefreshIntentParams{
		OperationID:       operationID,
		UserID:            userID,
		ProviderAccountID: accountID,
		FromGeneration:    acct.Generation,
	}); err != nil {
		if isUniqueViolation(err) {
			return s.codexReplayAfterDuplicate(ctx, q, userID, accountID, operationID)
		}
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: record intent: %w", err)
	}

	// (4) Acquire the lease, bounded to the provider callback deadline. It serializes the
	// single rotation, now AFTER the durable intent exists. The acquire is guarded on
	// generation = acct.Generation (PRD #1147 M4): a loser op that snapshotted this
	// generation but was scheduled out while a winner ran a full exchange→commit→reset cycle
	// can no longer win the freshly-idle lease, because the account's generation has moved
	// past the from_generation it presents. 0 rows means a live lease or a quarantine holds
	// the account, OR the generation already advanced — so this op must NOT exchange: if a
	// rival op has already advanced the account past our generation, reconcile to the
	// now-committed token with NO provider call; otherwise the outcome is contended and this
	// op's own 'rotating' intent (from_generation=acct.Generation) is resolved by
	// ReconcileUnresolvedCodexRefresh once the account advances past it. Either way no lease
	// is stranded and the stale refresh token is never re-spent.
	n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op:             operationID,
		Deadline:       pgconv.Time(leaseDeadline),
		ID:             accountID,
		UserID:         userID,
		FromGeneration: acct.Generation,
	})
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: acquire lease: %w", err)
	}
	if n == 0 {
		if fresh, gerr := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID}); gerr == nil && fresh.Generation > acct.Generation {
			return s.codexReturnCommitted(userID, fresh, CodexRefreshReconciled)
		}
		return CodexRefreshResult{Outcome: CodexRefreshContended}, ErrCodexRefreshContended
	}

	// (5) Exactly one provider exchange. On error the outcome is AMBIGUOUS (the provider
	// may have rotated server-side before the transport failed), so the intent is LEFT
	// 'rotating' for a survivor to reconcile — never a blind retry of the old refresh
	// token. The lease expires and routes to quarantine.
	providerCtx, cancelProvider := context.WithDeadline(ctx, providerDeadline)
	defer cancelProvider()
	result, rerr := s.codexRefresh.Refresh(providerCtx, prev.RefreshToken)
	if rerr != nil {
		return CodexRefreshResult{Outcome: CodexRefreshContended}, fmt.Errorf("codex refresh: provider exchange: %w", rerr)
	}

	// (6) Merged reseal.
	merged := mergeCodexLogin(prev, result)
	raw, err := json.Marshal(merged) //nolint:gosec // G117: the merged codex login blob is resealed under the vault DEK before it leaves this function
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: encode merged login: %w", err)
	}
	sealed, sealedWith, err := s.sealCodexLogin(userID, raw)
	if err != nil {
		// The single-use provider exchange already SPENT account A's refresh token, but the
		// merged login could not be sealed for durable storage — and unlike the absence branch
		// we cannot protect the material, because the seal that failed is exactly what a
		// recovery blob would need. The correct verdict turns on WHY the seal failed (PRD #1147
		// M4, defect 4):
		//
		//   - TRANSIENT (a vault-locked seal, errVaultLocked): protect the merged login under
		//     the server master key and persist it in the recovery slot. This leaves no raw login
		//     material beyond this operation, survives process loss, and avoids spending the
		//     single-use refresh token again. Reconciliation opens that temporary envelope and
		//     re-seals it under the user DEK before promotion once the vault unlocks. NO token is
		//     returned while the account is quarantined.
		//   - PERMANENT (any other seal error): the material genuinely cannot be sealed, so route
		//     it through the same unrecoverable resolution as an identity mismatch — mark the
		//     intent unrecoverable and quarantine, forcing a clean re-login. NO token.
		if errors.Is(err, errVaultLocked) {
			if s.box == nil {
				s.markCodexIntentUnrecoverable(ctx, q, userID, operationID)
				_, _ = q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{ID: accountID, UserID: userID, Op: operationID})
				return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: no master recovery sealer", ErrCodexRefreshUnrecoverable)
			}
			protected, perr := s.box.Seal(raw)
			if perr != nil {
				s.markCodexIntentUnrecoverable(ctx, q, userID, operationID)
				_, _ = q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{ID: accountID, UserID: userID, Op: operationID})
				return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: protect vault-locked refresh: %v", ErrCodexRefreshUnrecoverable, perr)
			}
			if perr := s.persistCodexRecoverySlot(ctx, q, userID, accountID, operationID, acct.Generation, protected, store.SealedWithMaster); perr != nil {
				s.markCodexIntentUnrecoverable(ctx, q, userID, operationID)
				return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: %v", ErrCodexRefreshUnrecoverable, perr)
			}
			return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: refreshed login retained pending vault unlock", ErrCodexRefreshQuarantined)
		}
		_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentUnrecoverable, OperationID: operationID, UserID: userID})
		_, _ = q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{ID: accountID, UserID: userID, Op: operationID})
		return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: seal of refreshed login failed: %v", ErrCodexRefreshUnrecoverable, err)
	}

	// (6b) Post-refresh identity RE-VERIFICATION (audit #2), BEFORE the commit. A refresh
	// token exchange can — through provider error, credential mix-up, or a hostile token —
	// return an access token that belongs to a DIFFERENT account than the one we are
	// rotating. Committing it would bind account A's row to account B's login and hand
	// account B's token to account A's run. So re-read identity with a NONROTATING
	// DiscoverIdentity and branch on the account's FROZEN tuple:
	//
	//   - MATCH (both fields equal) → proceed to commit exactly as before.
	//   - VERIFIED MISMATCH (discovery succeeded, tuple positively DIFFERS) → the material is
	//     NOT account A's: do NOT commit and do NOT write it to the recovery slot (it would
	//     poison a later promotion). Mark the intent unrecoverable, quarantine (owner+op
	//     guarded), return NO token.
	//   - INCOMPLETE (ErrIdentityIncomplete — an authenticated 2xx MISSING subject/workspace,
	//     i.e. "cannot tell", NOT a positively-different tuple) OR ABSENCE (DiscoverIdentity
	//     itself errored transiently — network/5xx) → we ALREADY spent account A's single-use
	//     refresh token and CANNOT yet establish a mismatch, so the NEW material must be
	//     RETAINED, not discarded: protect it in the recovery slot at from_generation and leave
	//     the intent 'rotating' so a later promotion re-verifies it. Return NO token, but do NOT
	//     claim recovery is impossible (PRD #1147 M4, defect 5).
	id, derr := s.codexRefresh.DiscoverIdentity(providerCtx, result.AccessToken)
	switch {
	case derr == nil && id.ProviderUserID == acct.ProviderUserID && id.WorkspaceAccountID == acct.WorkspaceAccountID:
		// MATCH → fall through to the commit below.
	case derr == nil:
		// VERIFIED MISMATCH: discovery succeeded and the tuple positively differs.
		return s.codexQuarantineIdentityMismatch(ctx, q, userID, accountID, operationID)
	default:
		// ErrIdentityIncomplete ("cannot tell") OR a transient absence — both RETAIN.
		return s.codexRetainUnverifiedMaterial(ctx, q, userID, accountID, operationID, acct.Generation, sealed, sealedWith, derr)
	}

	// (6c) Durable commit.
	row, cerr := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed:         sealed,
		SealedWith:     sealedWith,
		Op:             operationID,
		ID:             accountID,
		UserID:         userID,
		FromGeneration: acct.Generation,
	})
	if cerr != nil {
		return s.handleCodexCommitFailure(ctx, q, userID, accountID, operationID, acct.Generation, sealed, sealedWith, cerr)
	}

	// Commit landed. Record the intent committed, return the account to idle so the next
	// cycle can acquire, and release the freshly-committed token.
	_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentCommitted, OperationID: operationID, UserID: userID})
	_, _ = q.ResetCodexCoordIdle(ctx, store.ResetCodexCoordIdleParams{ID: accountID, UserID: userID})
	return CodexRefreshResult{
		AccessToken:      result.AccessToken,
		Generation:       row.Generation,
		ChatGPTAccountID: acct.WorkspaceAccountID,
		Outcome:          CodexRefreshAdvanced,
	}, nil
}

// handleCodexCommitFailure resolves a failed CommitCodexRefresh AFTER a successful
// provider exchange (PRD #1147 M2, B6 §7). It distinguishes the two failure shapes:
//
//   - CAS lost with the generation MOVED (another operation committed): reconcile to the
//     now-current committed generation — do NOT re-exchange, do NOT retry the old token.
//   - CAS lost with the generation UNCHANGED (our lease was quarantined/stolen), OR any
//     non-ErrNoRows persistence error: the new material exists but is not durable, so
//     protect it in the recovery slot and quarantine, returning a paused outcome and NO
//     token.
func (s *Service) handleCodexCommitFailure(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID, fromGeneration int64, sealedMerged []byte, sealedWith string, commitErr error) (CodexRefreshResult, error) {
	if errors.Is(commitErr, pgx.ErrNoRows) {
		fresh, gerr := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
		if gerr == nil && fresh.Generation > fromGeneration {
			// Another operation advanced the account: our exchange is redundant. Mark this
			// op reconciled and hand back the now-current committed token.
			_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentReconciled, OperationID: operationID, UserID: userID})
			return s.codexReturnCommitted(userID, fresh, CodexRefreshReconciled)
		}
		// Generation unchanged → the CAS failed on the lease guard (a quarantine or a
		// stolen lease), not on the generation. The provider rotated but we cannot commit,
		// so protect the new material and pause.
	}
	// Protect the freshly-rotated material into the recovery slot and quarantine. The
	// intent stays 'rotating' — a recovery scan will find the recovery copy at this
	// from_generation and resolve it (reconciled), distinguishing it from total loss. The
	// write is RESILIENT (detached ctx + bounded retry + background handoff, PRD #1147 M4
	// defect 4): a cancelled request ctx or a single transient DB blip no longer drops the
	// single-use material. Only an ESTABLISHED loss (no background seam to hand off to)
	// marks the intent unrecoverable.
	if perr := s.persistCodexRecoverySlot(ctx, q, userID, accountID, operationID, fromGeneration, sealedMerged, sealedWith); perr != nil {
		s.markCodexIntentUnrecoverable(ctx, q, userID, operationID)
		return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: %v", ErrCodexRefreshUnrecoverable, perr)
	}
	return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: commit failed: %v", ErrCodexRefreshQuarantined, commitErr)
}

// codexQuarantineIdentityMismatch resolves a post-refresh re-verification that returned a
// VERIFIED answer the material FAILS (audit #2): DiscoverIdentity either resolved the
// freshly-exchanged access token to a DIFFERENT account tuple, or reported an incomplete
// identity. The material is not account A's, so it is NEITHER committed NOR written to the
// recovery slot (a poisoned recovery blob would later mis-promote). The intent is marked
// unrecoverable (re-login required) and the account is quarantined (guarded on
// in_progress + this op, so only the lease holder may park it). NO token is returned.
func (s *Service) codexQuarantineIdentityMismatch(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID) (CodexRefreshResult, error) {
	_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentUnrecoverable, OperationID: operationID, UserID: userID})
	_, _ = q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{ID: accountID, UserID: userID, Op: operationID})
	return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, ErrCodexRefreshUnrecoverable
}

// codexRetainUnverifiedMaterial resolves a post-refresh re-verification whose OUTCOME is
// unknown (audit #2, absence): DiscoverIdentity itself errored transiently, so we cannot
// yet tell whether the exchanged material is account A's. Because the single-use refresh
// token was ALREADY spent, discarding the new material would brick the account; instead it
// is RETAINED in the recovery slot at from_generation (which also quarantines) and the
// intent is LEFT 'rotating' so a later promotion re-verifies and installs it. Returns a
// quarantined outcome with NO token — but recovery is possible, not lost. Should even the
// recovery write fail, the outcome is truly unknown with no protected copy, so the intent
// is marked unrecoverable.
func (s *Service) codexRetainUnverifiedMaterial(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID, fromGeneration int64, sealedMerged []byte, sealedWith string, discoverErr error) (CodexRefreshResult, error) {
	// The recovery write is RESILIENT (detached ctx + bounded retry + background handoff, PRD
	// #1147 M4 defect 4): a cancelled request ctx or a single transient DB blip must not drop
	// the single-use material. Only an ESTABLISHED loss marks the intent unrecoverable.
	if perr := s.persistCodexRecoverySlot(ctx, q, userID, accountID, operationID, fromGeneration, sealedMerged, sealedWith); perr != nil {
		s.markCodexIntentUnrecoverable(ctx, q, userID, operationID)
		return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: %v", ErrCodexRefreshUnrecoverable, perr)
	}
	return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: identity re-verification unavailable: %v", ErrCodexRefreshQuarantined, discoverErr)
}

// persistCodexRecoverySlot writes freshly-rotated (single-use) codex material into the
// recovery slot RESILIENTLY (PRD #1147 M4, defect 4). It runs on a DETACHED context
// (context.WithoutCancel of the request ctx, so a cancelled request no longer aborts the
// write, plus a bounded WithTimeout so a wedged write cannot hang), and retries a bounded
// number of times synchronously. If the synchronous retries are exhausted on a transient
// error it hands continued retry off to s.background — holding the sealed material in the
// closure — and returns nil (a RETAINED-pending outcome, NOT an established loss). It
// returns errCodexRecoveryLost ONLY when loss is genuinely established synchronously: no
// background seam is wired to hand off to. It preserves the store's coord_operation_id +
// generation fences and recovery_sealed_with metadata (the SQL is unchanged).
func (s *Service) persistCodexRecoverySlot(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID, fromGeneration int64, sealedMerged []byte, sealedWith string) error {
	params := store.SetCodexRecoverySlotParams{
		Sealed:             sealedMerged,
		Gen:                fromGeneration,
		RecoverySealedWith: pgconv.Text(sealedWith),
		ID:                 accountID,
		UserID:             userID,
		Op:                 operationID,
	}

	// Bounded synchronous retry on a detached ctx.
	for attempt := 0; attempt < codexRecoverySlotSyncAttempts; attempt++ {
		if werr := s.writeCodexRecoverySlotOnce(ctx, q, params); werr == nil {
			return nil
		} else if errors.Is(werr, errCodexRecoveryFenceLost) {
			return werr
		}
		if attempt < codexRecoverySlotSyncAttempts-1 {
			time.Sleep(codexRecoverySlotRetryBackoff)
		}
	}

	// Synchronous retries exhausted. Hand continued retry off to the background so the
	// request no longer blocks on it and a cancelled request ctx cannot abort it. Without a
	// background seam there is nowhere to hand off to, so loss is established here.
	if s.background == nil {
		return errCodexRecoveryLost
	}
	s.background(func() {
		for attempt := 0; attempt < codexRecoverySlotBgAttempts; attempt++ {
			werr := s.writeCodexRecoverySlotOnce(ctx, q, params)
			if werr == nil {
				return
			}
			if errors.Is(werr, errCodexRecoveryFenceLost) {
				break
			}
			time.Sleep(codexRecoverySlotRetryBackoff)
		}
		// The background retrier gave up: loss is now established, so route the account to
		// re-login by marking THIS op's intent unrecoverable, on its own fresh detached ctx.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexRecoverySlotWriteTimeout)
		defer cancel()
		s.markCodexIntentUnrecoverable(uctx, q, userID, operationID)
		slog.Warn("codex refresh: recovery slot persistence gave up", "account", accountID, "operation", operationID)
	})
	return nil
}

// writeCodexRecoverySlotOnce performs ONE SetCodexRecoverySlot write on a detached,
// bounded-timeout context derived from ctx, so a cancelled request ctx does not abort it.
// Store errors are retryable; exactly one affected row is success. Zero rows is a permanent
// operation/generation fence miss and must never masquerade as persisted material.
func (s *Service) writeCodexRecoverySlotOnce(ctx context.Context, q codexRefreshStore, params store.SetCodexRecoverySlotParams) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexRecoverySlotWriteTimeout)
	defer cancel()
	n, err := q.SetCodexRecoverySlot(wctx, params)
	if err != nil {
		return err
	}
	if n != 1 {
		return errCodexRecoveryFenceLost
	}
	return nil
}

// markCodexIntentUnrecoverable is the shared best-effort transition of an operation's intent
// to 'unrecoverable' (the re-login-required signal). Best-effort by design: it is only ever
// called on a path that already returns a quarantined/paused outcome with NO token.
func (s *Service) markCodexIntentUnrecoverable(ctx context.Context, q codexRefreshStore, userID, operationID uuid.UUID) {
	_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentUnrecoverable, OperationID: operationID, UserID: userID})
}

// codexReplayAfterDuplicate resolves a 23505 on the pre-rotation intent insert: a prior
// attempt of THIS operation already recorded an intent. Because the intent insert now
// precedes the lease acquire (advanceCodexRefresh step 3), this path holds NO lease — so a
// same-operationID concurrent retry can never strand a lease here. It re-reads the account
// + intent and applies the same replay/reconcile decision rather than exchanging a second
// time, handling BOTH terminal states (committed/reconciled → replay/reconciled token) and
// a still-'rotating' prior attempt that holds the lease (→ contended, no token, no second
// exchange).
func (s *Service) codexReplayAfterDuplicate(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID) (CodexRefreshResult, error) {
	acct, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: re-read account after duplicate: %w", err)
	}
	intent, err := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{OperationID: operationID, UserID: userID})
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: re-read intent after duplicate: %w", err)
	}
	switch intent.State {
	case codexIntentCommitted, codexIntentReconciled:
		return s.codexReturnCommitted(userID, acct, CodexRefreshReplayed)
	case codexIntentUnrecoverable:
		return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, ErrCodexRefreshUnrecoverable
	default:
		// Still 'rotating': the prior attempt is either mid-flight or stranded. If the
		// account advanced, reconcile; otherwise contended (a survivor resolves it).
		if acct.Generation > intent.FromGeneration {
			return s.codexReturnCommitted(userID, acct, CodexRefreshReconciled)
		}
		return CodexRefreshResult{Outcome: CodexRefreshContended}, ErrCodexRefreshContended
	}
}

// codexReturnCommitted opens the account's CURRENT committed login and returns its access
// token with the account's current generation — the shared tail of every replay/reconcile
// path (no provider exchange).
//
// It REFUSES a quarantined account (B6 §9): when a rotation's commit failed into the
// recovery slot the account is quarantined and its sealed_login is the pre-rotation blob
// (the very token whose expiry triggered the refresh), so handing it back would return a
// stale/expired token as a nil-error success. A quarantined account must be reconciled or
// re-logged-in first, so we surface CodexRefreshQuarantined instead of a token — this
// closes both the winner's own post-failure retry and any bystander op (one that inserted
// a rotating intent then lost the lease race) whose intent was reconciled off the winner's
// recovery material.
func (s *Service) codexReturnCommitted(userID uuid.UUID, acct store.CodexProviderAccount, outcome CodexRefreshOutcome) (CodexRefreshResult, error) {
	if acct.CoordState == codexCoordQuarantined {
		return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, ErrCodexRefreshQuarantined
	}
	blob, err := s.openCodexAccountLogin(userID, acct)
	if err != nil {
		return CodexRefreshResult{}, err
	}
	return CodexRefreshResult{
		AccessToken:      blob.AccessToken,
		Generation:       acct.Generation,
		ChatGPTAccountID: acct.WorkspaceAccountID,
		Outcome:          outcome,
	}, nil
}

// ReleaseCodexCredential authorizes ScopeReleaseAccessToken and returns the run's
// currently usable access token plus only the server-owned metadata required by its auth
// mode (PRD #1171 M1). Subscription includes the committed generation and verified
// ChatGPT account id; API-key includes neither. It never returns the refresh/login blob.
// Authority is re-verified immediately before any result is returned.
func (s *Service) ReleaseCodexCredential(ctx context.Context, wkr store.Worker, runID uuid.UUID, capability string) (CodexReleaseResult, error) {
	authCtx, err := s.AuthorizeCodexCredentialOp(ctx, wkr, runID, capability, ScopeReleaseAccessToken)
	if err != nil {
		return CodexReleaseResult{}, err
	}

	result := CodexReleaseResult{AuthMode: authCtx.AuthMode}
	switch authCtx.AuthMode {
	case codexAuthModeSubscription:
		q, ok := s.codexRefreshQueries()
		if !ok {
			return CodexReleaseResult{}, errCodexStoreUnavailable
		}
		acct, aerr := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: authCtx.UserID, ID: authCtx.AccountID})
		if aerr != nil {
			return CodexReleaseResult{}, fmt.Errorf("codex release: read account: %w", aerr)
		}
		blob, berr := s.openCodexAccountLogin(authCtx.UserID, acct)
		if berr != nil {
			return CodexReleaseResult{}, berr
		}
		generation := acct.Generation
		result.AccessToken = blob.AccessToken
		result.Generation = &generation
		result.ChatGPTAccountID = acct.WorkspaceAccountID
	case codexAuthModeAPIKey:
		// Kind-guarded open (audit #6): the release predicate already rejects a kind↔mode
		// mismatch, but the api_key release path opens by id defensively through
		// OpenByIDOfKind so a mis-bound codex_auth alias can never disclose its login blob
		// (refresh_token and all) here — a non-openai_api_key row is the not-found sentinel.
		tok, oerr := secretopen.OpenByIDOfKind(ctx, s.q, s.vlt, s.box, authCtx.UserID, authCtx.SecretID, store.KindOpenAIAPIKey)
		switch {
		case oerr == nil:
			result.AccessToken = string(tok)
		case errors.Is(oerr, secretopen.ErrVaultLocked):
			return CodexReleaseResult{}, errVaultLocked
		case errors.Is(oerr, secretopen.ErrNoSecret), errors.Is(oerr, secretopen.ErrUndecryptable):
			return CodexReleaseResult{}, fmt.Errorf("%w: codex api key could not be opened", errCredentialUnavailable)
		default:
			return CodexReleaseResult{}, oerr
		}
	default:
		return CodexReleaseResult{}, ErrCodexRunNotBound
	}

	// Re-verify authority immediately before returning the token — no provider round-trip
	// separates the read above from this recheck, so a revoke/re-mint that landed in the
	// interval refuses the release rather than leaking a token the run no longer owns.
	if _, err := s.AuthorizeCodexCredentialOp(ctx, wkr, runID, capability, ScopeReleaseAccessToken); err != nil {
		return CodexReleaseResult{}, err
	}
	return result, nil
}

// ReconcileUnresolvedCodexRefresh is the crash-safe recovery pass for one account (PRD
// #1147 M2, B6 §7-8). It reaps an expired lease to quarantine FIRST (an expired lease
// must never route to a fresh rotation), then resolves every still-'rotating' intent:
//
//   - the account advanced past the intent's from_generation (a commit landed) → mark it
//     'reconciled';
//   - a recovery copy survives for this from_generation (the provider rotated but the
//     commit was lost; the material is protected) → mark it 'reconciled' (recoverable —
//     an operator/next cycle can re-apply it), distinguishing uncertain-commit from loss;
//   - otherwise (generation unchanged, no recovery copy, outcome unknown) → mark it
//     'unrecoverable' (the re-login-required signal).
//
// It NEVER re-spends the old refresh token and NEVER rotates. Returns how many intents it
// resolved.
func (s *Service) ReconcileUnresolvedCodexRefresh(ctx context.Context, userID, accountID uuid.UUID) (int, error) {
	q, ok := s.codexRefreshQueries()
	if !ok {
		return 0, errCodexStoreUnavailable
	}

	// Reap an expired lease → quarantine (never a fresh rotation). Best-effort: a 0-row
	// result just means the lease is not expired (or the account is not in_progress).
	if _, err := q.QuarantineExpiredCodexLease(ctx, store.QuarantineExpiredCodexLeaseParams{
		ID:     accountID,
		UserID: userID,
		Now:    pgconv.Time(s.codexNow()),
	}); err != nil {
		return 0, fmt.Errorf("codex reconcile: reap lease: %w", err)
	}

	acct, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
	if err != nil {
		return 0, fmt.Errorf("codex reconcile: read account: %w", err)
	}
	intents, err := q.ListUnresolvedCodexRefreshIntents(ctx, store.ListUnresolvedCodexRefreshIntentsParams{
		UserID:            userID,
		ProviderAccountID: accountID,
	})
	if err != nil {
		return 0, fmt.Errorf("codex reconcile: list intents: %w", err)
	}

	resolved := 0
	now := s.codexNow()
	for _, it := range intents {
		state := codexRefreshResolution(now, acct, it)
		if state == codexIntentPending {
			// A LIVE, validly-leased, in-flight rotation under THIS op (PRD #1147 M4, defect
			// 6): reconciliation must NOT declare it. Leave the intent 'rotating' — no state
			// write, not counted resolved — so the owning op can still commit, and it is not
			// force-mutated in the intents slice passed to promoteCodexRecovery below.
			continue
		}
		if _, serr := q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{
			State:       state,
			OperationID: it.OperationID,
			UserID:      userID,
		}); serr != nil {
			return resolved, fmt.Errorf("codex reconcile: set intent %s state: %w", it.OperationID, serr)
		}
		resolved++
	}

	// Recovery/re-login promotion (audit #5). A quarantined account whose recovery slot
	// holds protected material AT THE CURRENT generation is a candidate for roll-forward: a
	// commit-failure or an absence-branch retention left a known-good login there. Attempt
	// to make it live again — but ONLY after re-verifying its identity against the account's
	// frozen tuple, so a poisoned/mismatched recovery blob can never be promoted. The intents
	// resolved above are passed so a verified mismatch can correct their 'reconciled'
	// (recoverable) verdict to 'unrecoverable' (the recovery copy turned out to be bad).
	if acct.CoordState == codexCoordQuarantined && len(acct.RecoverySealed) > 0 &&
		acct.RecoveryGeneration.Valid && acct.RecoveryGeneration.Int64 == acct.Generation {
		if perr := s.promoteCodexRecovery(ctx, q, userID, acct, intents); perr != nil {
			return resolved, perr
		}
	}
	return resolved, nil
}

// promoteCodexRecovery attempts to make a quarantined account whose recovery slot holds
// protected material live again (audit #5). It opens the recovery blob, re-verifies its
// identity against the account's FROZEN tuple with a nonrotating DiscoverIdentity, and only
// on a MATCH promotes it (PromoteCodexRecovery, CAS on from_generation) — installing the
// recovery material as the live login, advancing the generation, clearing the quarantine.
// A VERIFIED MISMATCH (discovery succeeded, tuple positively differs) means the recovery
// copy is not this account's material after all, so every still-recoverable intent is
// corrected to 'unrecoverable' and the account is left quarantined. An INCOMPLETE identity
// (ErrIdentityIncomplete — "cannot tell", not a positively-different tuple) or a TRANSIENT
// DiscoverIdentity error (or a locked vault) leaves the slot untouched for the next
// reconcile pass — the material is retained, not lost (PRD #1147 M4, defect 5). A nil
// identity seam (a Service wired without one) likewise defers.
func (s *Service) promoteCodexRecovery(ctx context.Context, q codexRefreshStore, userID uuid.UUID, acct store.CodexProviderAccount, intents []store.CodexRefreshIntent) error {
	if s.codexRefresh == nil {
		return nil // no identity seam wired: cannot re-verify, leave for a later pass
	}
	// Open the recovery blob with the key that sealed IT (recovery_sealed_with), NOT the
	// live sealed_login's key (acct.SealedWith): a master→dek migration can advance
	// sealed_login's discriminator while the protected recovery blob still carries the older
	// one (or vice versa), so opening under sealed_with would try the wrong key and fail to
	// decrypt. The store CHECK guarantees recovery_sealed_with is populated whenever
	// recovery_sealed is, and this path is entered only with a non-empty recovery slot.
	blob, err := s.openCodexSealed(userID, acct.RecoverySealed, acct.RecoverySealedWith.String)
	if err != nil {
		if errors.Is(err, errVaultLocked) {
			return nil // transient: the recovery material stays protected for the next pass
		}
		return fmt.Errorf("codex reconcile: open recovery: %w", err)
	}

	id, derr := s.codexRefresh.DiscoverIdentity(ctx, blob.AccessToken)
	switch {
	case derr == nil && id.ProviderUserID == acct.ProviderUserID && id.WorkspaceAccountID == acct.WorkspaceAccountID:
		// MATCH → re-seal under the currently required key before installing. A recovery
		// envelope may be master-sealed because the provider rotated while the user vault was
		// locked; it must never become the live credential in that form. If the vault remains
		// locked, leave the protected slot untouched for a later reconcile pass.
		raw, merr := json.Marshal(blob) //nolint:gosec // G117: the merged login is immediately sealed before it leaves this branch
		if merr != nil {
			return fmt.Errorf("codex reconcile: encode recovery: %w", merr)
		}
		sealed, sealedWith, serr := s.sealCodexLogin(userID, raw)
		if errors.Is(serr, errVaultLocked) {
			return nil
		}
		if serr != nil {
			return fmt.Errorf("codex reconcile: re-seal recovery: %w", serr)
		}
		// ErrNoRows means the slot moved under us (already promoted, or the
		// from_generation no longer matches) — an idempotent no-op.
		if _, perr := q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{
			ID:             acct.ID,
			UserID:         userID,
			FromGeneration: acct.Generation,
			Sealed:         sealed,
			SealedWith:     sealedWith,
		}); perr != nil && !errors.Is(perr, pgx.ErrNoRows) {
			return fmt.Errorf("codex reconcile: promote recovery: %w", perr)
		}
		return nil
	case derr == nil:
		// VERIFIED MISMATCH (tuple positively differs): the recovery copy is not this
		// account's. Correct ONLY the intents whose 'reconciled' verdict DEPENDED on this
		// (now-untrusted) recovery copy — i.e. those reconciled via codexRefreshResolution's
		// recovery-dependent branch, where it.FromGeneration == acct.RecoveryGeneration. An
		// intent reconciled
		// because the account's generation independently ADVANCED past its from_generation
		// (acct.Generation > it.FromGeneration, the generation-superseded branch) reflects a
		// committed rotation that owes nothing to this recovery blob, so it stays 'reconciled'
		// — a bad recovery copy does not retroactively invalidate an independently-committed
		// rotation. The filter mirrors that branch exactly: since this promotion path runs only
		// when acct.RecoveryGeneration == acct.Generation, a generation-superseded intent has
		// it.FromGeneration < acct.RecoveryGeneration and is excluded automatically.
		for _, it := range intents {
			recoveryDependent := acct.RecoveryGeneration.Valid && it.FromGeneration == acct.RecoveryGeneration.Int64
			if !recoveryDependent {
				continue
			}
			if _, serr := q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{
				State:       codexIntentUnrecoverable,
				OperationID: it.OperationID,
				UserID:      userID,
			}); serr != nil {
				return fmt.Errorf("codex reconcile: mark recovery-mismatch intent %s: %w", it.OperationID, serr)
			}
		}
		return nil
	default:
		// ErrIdentityIncomplete ("cannot tell") OR a transient discovery error: leave the
		// slot untouched for the next reconcile pass, do NOT mark unrecoverable (PRD #1147 M4,
		// defect 5).
		return nil
	}
}

// codexRefreshResolution decides the state for a still-'rotating' intent given the account's
// current facts and the reconcile clock. Pure, so the reconcile decision is unit-testable.
//
// It returns codexIntentPending (an IN-MEMORY sentinel, never a DB value) for a LIVE
// in-flight rotation the reconcile pass must NOT declare (PRD #1147 M4, defect 6): the reap
// (QuarantineExpiredCodexLease) already quarantined an EXPIRED lease before this runs, so an
// account still 'in_progress' under this op with a valid, non-expired lease it owns and an
// unchanged generation is a genuinely live op — declaring it unrecoverable would brick a
// refresh that is about to commit. Every other stranded shape still resolves terminally.
func codexRefreshResolution(now time.Time, acct store.CodexProviderAccount, it store.CodexRefreshIntent) string {
	switch {
	case acct.Generation > it.FromGeneration:
		// A commit landed past this intent's starting generation → the rotation completed.
		return codexIntentReconciled
	case len(acct.RecoverySealed) > 0 && acct.RecoveryGeneration.Valid && acct.RecoveryGeneration.Int64 == it.FromGeneration:
		// A recovery copy survives for this generation — recoverable, not total loss.
		return codexIntentReconciled
	case acct.CoordState == codexCoordInProgress &&
		acct.CoordOperationID.Valid && uuid.UUID(acct.CoordOperationID.Bytes) == it.OperationID &&
		acct.LeaseDeadline.Valid && acct.LeaseDeadline.Time.After(now) &&
		acct.Generation == it.FromGeneration:
		// A LIVE, validly-leased, in-flight rotation under THIS op → leave it 'rotating'.
		return codexIntentPending
	default:
		// Generation unchanged, no recovery copy, no live lease of this op: the outcome is
		// unknown and nothing survives to re-apply → re-login required.
		return codexIntentUnrecoverable
	}
}
