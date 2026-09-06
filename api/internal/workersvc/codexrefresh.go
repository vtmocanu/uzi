package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
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

// codexRefreshLeaseTTL bounds the refresh lease deadline. It MUST stay at or under the
// provider's 10-second callback deadline (PRD #1147 M2, B6): a lease that outlived the
// provider window would let a presumed-dead refresher keep the account blocked past the
// point the provider itself considers the exchange abandoned. 8s leaves a small margin
// under 10s for the commit round-trip while still expiring promptly for a survivor to
// reconcile.
const codexRefreshLeaseTTL = 8 * time.Second

// Codex refresh-intent states (the codex_refresh_intent.state CHECK values, migration
// 00200). Named here so the state machine never spells a bare literal that could drift
// from the schema.
const (
	codexIntentRotating      = "rotating"
	codexIntentCommitted     = "committed"
	codexIntentUnrecoverable = "unrecoverable"
	codexIntentReconciled    = "reconciled"
)

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
// It is DELIBERATELY narrow (only Refresh), mirroring codexcred.go's CodexIdentityClient:
// the coordinated refresher's ONE provider interaction is a token exchange, so the seam
// exposes exactly that and nothing else. The interface lives here in workersvc rather
// than reaching into codexauth so the codexauth package needs no edit.
type CodexRefreshClient interface {
	Refresh(ctx context.Context, refreshToken string) (codexauth.RefreshResult, error)
}

// CodexRefreshResult is the outcome of a CoordinatedCodexRefresh / reconcile / replay.
// AccessToken is non-empty ONLY when Outcome is ADVANCED/REPLAYED/RECONCILED and the
// returned error is nil; it is never set for a contended or quarantined outcome.
type CodexRefreshResult struct {
	AccessToken string
	Generation  int64
	Outcome     CodexRefreshOutcome
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

// openCodexAccountLogin opens and decodes the account's sealed_login on the shared vault
// path (AAD user_id||codex_auth). A locked vault surfaces as errVaultLocked (transient,
// retry); undecryptable/malformed material surfaces as errCredentialUnavailable.
func (s *Service) openCodexAccountLogin(userID uuid.UUID, acct store.CodexProviderAccount) (codexLoginBlob, error) {
	plain, err := secretopen.OpenSealed(s.vlt, s.box, userID, store.KindCodexAuth, acct.SealedWith, acct.SealedLogin)
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

// sealCodexLogin seals a merged login blob for storage on the provider account, mirroring
// the reconciler's sealLogin: with a vault it seals under the user's DEK
// (sealed_with='dek', AAD user_id||codex_auth); without one (tests) it falls back to the
// master box (sealed_with='master').
func (s *Service) sealCodexLogin(userID uuid.UUID, plaintext []byte) (sealed []byte, sealedWith string, err error) {
	if s.vlt != nil {
		sealed, err = s.vlt.Seal(userID, store.KindCodexAuth, plaintext)
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
	// (1) Authorize ScopeStartRefresh. This is subscription-only, so a successful authCtx
	// carries the resolved account id; an api_key run is refused by the scope check.
	authCtx, err := s.AuthorizeCodexCredentialOp(ctx, wkr, runID, capability, ScopeStartRefresh)
	if err != nil {
		return CodexRefreshResult{}, err
	}
	return s.coordinatedRefresh(ctx, authCtx.UserID, authCtx.AccountID, operationID, observedGeneration)
}

// coordinatedRefresh is the post-authorization core, keyed on the resolved (user,
// account). Split out so its failure paths (CAS lost, persistence failure) are testable
// through a wrapped store without re-deriving the authority every time.
func (s *Service) coordinatedRefresh(ctx context.Context, userID, accountID, operationID uuid.UUID, observedGeneration int64) (CodexRefreshResult, error) {
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
	return s.advanceCodexRefresh(ctx, q, userID, accountID, operationID, acct)
}

// advanceCodexRefresh performs the rotation itself (PRD #1147 M2, B6 §3-6): durable intent,
// lease, one provider exchange, merged reseal, CAS commit. The durable intent is recorded
// BEFORE the lease so a same-operationID concurrent retry is caught (23505) before any lease
// is held and can never strand it. `acct` is the account read at generation ==
// observedGeneration (the generation this rotation advances FROM).
func (s *Service) advanceCodexRefresh(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID, acct store.CodexProviderAccount) (CodexRefreshResult, error) {
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
	deadline := s.codexNow().Add(codexRefreshLeaseTTL)
	n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op:             operationID,
		Deadline:       pgconv.Time(deadline),
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
	result, rerr := s.codexRefresh.Refresh(ctx, prev.RefreshToken)
	if rerr != nil {
		return CodexRefreshResult{Outcome: CodexRefreshContended}, fmt.Errorf("codex refresh: provider exchange: %w", rerr)
	}

	// (6) Merged reseal + durable commit.
	merged := mergeCodexLogin(prev, result)
	raw, err := json.Marshal(merged) //nolint:gosec // G117: the merged codex login blob is resealed under the vault DEK before it leaves this function
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: encode merged login: %w", err)
	}
	sealed, sealedWith, err := s.sealCodexLogin(userID, raw)
	if err != nil {
		return CodexRefreshResult{}, fmt.Errorf("codex refresh: seal merged login: %w", err)
	}

	row, cerr := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed:         sealed,
		SealedWith:     sealedWith,
		Op:             operationID,
		ID:             accountID,
		UserID:         userID,
		FromGeneration: acct.Generation,
	})
	if cerr != nil {
		return s.handleCodexCommitFailure(ctx, q, userID, accountID, operationID, acct.Generation, sealed, cerr)
	}

	// Commit landed. Record the intent committed, return the account to idle so the next
	// cycle can acquire, and release the freshly-committed token.
	_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentCommitted, OperationID: operationID, UserID: userID})
	_, _ = q.ResetCodexCoordIdle(ctx, store.ResetCodexCoordIdleParams{ID: accountID, UserID: userID})
	return CodexRefreshResult{AccessToken: result.AccessToken, Generation: row.Generation, Outcome: CodexRefreshAdvanced}, nil
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
func (s *Service) handleCodexCommitFailure(ctx context.Context, q codexRefreshStore, userID, accountID, operationID uuid.UUID, fromGeneration int64, sealedMerged []byte, commitErr error) (CodexRefreshResult, error) {
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
	// from_generation and resolve it (reconciled), distinguishing it from total loss.
	if _, rerr := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: sealedMerged,
		Gen:    fromGeneration,
		ID:     accountID,
		UserID: userID,
	}); rerr != nil {
		// Even the recovery write failed: the outcome is now truly unknown with no
		// protected copy. Mark unrecoverable so the account routes to re-login.
		_, _ = q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{State: codexIntentUnrecoverable, OperationID: operationID, UserID: userID})
		return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: %v", ErrCodexRefreshQuarantined, rerr)
	}
	return CodexRefreshResult{Outcome: CodexRefreshQuarantined}, fmt.Errorf("%w: commit failed: %v", ErrCodexRefreshQuarantined, commitErr)
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
	return CodexRefreshResult{AccessToken: blob.AccessToken, Generation: acct.Generation, Outcome: outcome}, nil
}

// ReleaseCodexAccessToken authorizes ScopeReleaseAccessToken and returns ONLY the run's
// currently-usable access token (PRD #1147 M2, B6 Deliverable 3) — never the refresh /
// login blob. Authority is RE-VERIFIED immediately before the token is returned: the read
// and the authority check are not separated by any provider round-trip, and a second
// authorize right before return closes the window in which authority could have gone
// stale between the first check and the release.
func (s *Service) ReleaseCodexAccessToken(ctx context.Context, wkr store.Worker, runID uuid.UUID, capability string) (string, error) {
	authCtx, err := s.AuthorizeCodexCredentialOp(ctx, wkr, runID, capability, ScopeReleaseAccessToken)
	if err != nil {
		return "", err
	}

	var token string
	switch authCtx.AuthMode {
	case codexAuthModeSubscription:
		q, ok := s.codexRefreshQueries()
		if !ok {
			return "", errCodexStoreUnavailable
		}
		acct, aerr := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: authCtx.UserID, ID: authCtx.AccountID})
		if aerr != nil {
			return "", fmt.Errorf("codex release: read account: %w", aerr)
		}
		blob, berr := s.openCodexAccountLogin(authCtx.UserID, acct)
		if berr != nil {
			return "", berr
		}
		token = blob.AccessToken
	case codexAuthModeAPIKey:
		// Kind-guarded open (audit #6): the release predicate already rejects a kind↔mode
		// mismatch, but the api_key release path opens by id defensively through
		// OpenByIDOfKind so a mis-bound codex_auth alias can never disclose its login blob
		// (refresh_token and all) here — a non-openai_api_key row is the not-found sentinel.
		tok, oerr := secretopen.OpenByIDOfKind(ctx, s.q, s.vlt, s.box, authCtx.UserID, authCtx.SecretID, store.KindOpenAIAPIKey)
		switch {
		case oerr == nil:
			token = string(tok)
		case errors.Is(oerr, secretopen.ErrVaultLocked):
			return "", errVaultLocked
		case errors.Is(oerr, secretopen.ErrNoSecret), errors.Is(oerr, secretopen.ErrUndecryptable):
			return "", fmt.Errorf("%w: codex api key could not be opened", errCredentialUnavailable)
		default:
			return "", oerr
		}
	default:
		return "", ErrCodexRunNotBound
	}

	// Re-verify authority immediately before returning the token — no provider round-trip
	// separates the read above from this recheck, so a revoke/re-mint that landed in the
	// interval refuses the release rather than leaking a token the run no longer owns.
	if _, err := s.AuthorizeCodexCredentialOp(ctx, wkr, runID, capability, ScopeReleaseAccessToken); err != nil {
		return "", err
	}
	return token, nil
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
	for _, it := range intents {
		state := codexRefreshResolution(acct, it)
		if _, serr := q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{
			State:       state,
			OperationID: it.OperationID,
			UserID:      userID,
		}); serr != nil {
			return resolved, fmt.Errorf("codex reconcile: set intent %s state: %w", it.OperationID, serr)
		}
		resolved++
	}
	return resolved, nil
}

// codexRefreshResolution decides the terminal state for a stranded 'rotating' intent given
// the account's current facts. Pure, so the reconcile decision is unit-testable.
func codexRefreshResolution(acct store.CodexProviderAccount, it store.CodexRefreshIntent) string {
	switch {
	case acct.Generation > it.FromGeneration:
		// A commit landed past this intent's starting generation → the rotation completed.
		return codexIntentReconciled
	case len(acct.RecoverySealed) > 0 && acct.RecoveryGeneration.Valid && acct.RecoveryGeneration.Int64 == it.FromGeneration:
		// A recovery copy survives for this generation — recoverable, not total loss.
		return codexIntentReconciled
	default:
		// Generation unchanged, no recovery copy: the outcome is unknown and nothing
		// survives to re-apply → re-login required.
		return codexIntentUnrecoverable
	}
}
