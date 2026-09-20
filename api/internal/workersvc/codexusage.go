package workersvc

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Codex per-ACCOUNT usage collection (PRD #1209 M2), the highest-risk half of the meter
// feature: it opens a subscription account's COMMITTED access token, reads its rate-limit
// buckets from the provider, and hands the caller (the usage poller) ONLY a sanitized,
// token-free reading. The raw access token NEVER leaves this package — CollectCodexAccountUsage
// returns a CodexUsageReading (buckets + observed fences) or a typed CodexUsageFailure, and
// nothing in either carries a token, a refresh token, or a raw provider/account principal id.
//
// It reuses the run lane's own primitives: openCodexAccountLogin (the non-rotating committed
// read) and the UNEXPORTED coordinatedRefresh core (not the worker wrapper), reached only via
// this owner-scoped principal path. It NEVER touches AuthorizeCodexCredentialOp /
// CoordinatedCodexRefresh / ReleaseCodexCredential — worker authority is untouched.

// Codex coordination states not already named elsewhere. codexCoordInProgress and
// codexCoordQuarantined live in codexrefresh.go / codexauthz.go; these two complete the set
// the poll path branches on, so it never spells a bare 'idle'/'committed' literal.
const (
	codexCoordIdle      = "idle"
	codexCoordCommitted = "committed"
)

// CodexUsageFailureKind is the CLOSED set of reasons CollectCodexAccountUsage refused to
// return a reading. Each maps to a short, sanitized status token the poller persists as the
// account's attempt_status — never a token, body or raw provider id.
type CodexUsageFailureKind int

const (
	// CodexUsageFailureUnknown is the zero value, never returned deliberately.
	CodexUsageFailureUnknown CodexUsageFailureKind = iota
	// CodexUsageFailReauthRequired: the access token is proven expired with no renewal
	// material (coordinatedRefresh returned ErrCodexRefreshNoToken). The account has been
	// flagged reauth_required and the user must re-login.
	CodexUsageFailReauthRequired
	// CodexUsageFailVaultLocked: the owner's vault is locked, so the committed login could
	// not be opened. The prior reading is retained; NO reauth is set.
	CodexUsageFailVaultLocked
	// CodexUsageFailRateLimited: the provider returned 429. RetryAfter carries its hint.
	CodexUsageFailRateLimited
	// CodexUsageFailProviderMismatch: the response identity does not match the account's
	// frozen tuple. The reading is discarded (never upserted); NO reauth is set.
	CodexUsageFailProviderMismatch
	// CodexUsageFailTransient: a retryable fault — a 403, a 5xx, a transport error, an
	// invalid/duplicate-bucket body, a refresh that could not proceed, or an authority that
	// moved mid-call. The prior reading is retained; retry next tick.
	CodexUsageFailTransient
	// CodexUsageFailDisabled: the account is not in a pollable state (quarantined, already
	// reauth-flagged, or carrying recovery material) — not an error, just not a poll target.
	CodexUsageFailDisabled
	// CodexUsageFailNotLinked: the account does not resolve to an owned account, or it has no
	// linked codex_auth alias (there is no subscription to meter).
	CodexUsageFailNotLinked
)

// String is the sanitized status token the poller persists. It is a fixed enum label, never
// derived from provider data.
func (k CodexUsageFailureKind) String() string {
	switch k {
	case CodexUsageFailReauthRequired:
		return "reauth_required"
	case CodexUsageFailVaultLocked:
		return "vault_locked"
	case CodexUsageFailRateLimited:
		return "rate_limited"
	case CodexUsageFailProviderMismatch:
		return "provider_mismatch"
	case CodexUsageFailTransient:
		return "transient"
	case CodexUsageFailDisabled:
		return "disabled"
	case CodexUsageFailNotLinked:
		return "not_linked"
	default:
		return "unknown"
	}
}

// CodexUsageFailure is the typed, token-free failure CollectCodexAccountUsage returns. It
// carries the CLOSED failure kind and (for a rate-limit) the provider's Retry-After so the
// poller can back off exactly. It never carries a token, a body, or a raw provider id.
type CodexUsageFailure struct {
	Kind       CodexUsageFailureKind
	RetryAfter time.Duration
}

func (f *CodexUsageFailure) Error() string { return "codex usage: " + f.Kind.String() }

func codexUsageFail(kind CodexUsageFailureKind) *CodexUsageFailure {
	return &CodexUsageFailure{Kind: kind}
}

// CodexUsageReading is the token-free result of a successful poll: the normalized bucket set
// (serialized by the poller as the frozen []apitypes.CodexRateLimitBucketDTO JSONB M3 reads
// back) plus the (generation, credential_revision) the reading was captured under — the fence
// the poller's revision-fenced upsert requires.
type CodexUsageReading struct {
	Buckets                    []apitypes.CodexRateLimitBucketDTO
	ObservedGeneration         int64
	ObservedCredentialRevision int64
}

// codexPollPrincipal is the account snapshot the poll path captures at its first read. It has
// NO exported fields and is built only by newCodexPollPrincipal, so a caller can never mint a
// principal for an account it did not resolve owner-scoped.
type codexPollPrincipal struct {
	userID             uuid.UUID
	accountID          uuid.UUID
	generation         int64
	credentialRevision int64
	workspaceAccountID string
	providerUserID     string
}

// newCodexPollPrincipal captures the fences a poll needs from a freshly-read, owner-scoped
// account row.
func (s *Service) newCodexPollPrincipal(acct store.CodexProviderAccount) codexPollPrincipal {
	return codexPollPrincipal{
		userID:             acct.UserID,
		accountID:          acct.ID,
		generation:         acct.Generation,
		credentialRevision: acct.CredentialRevision,
		workspaceAccountID: acct.WorkspaceAccountID,
		providerUserID:     acct.ProviderUserID,
	}
}

// CodexUsageReader is the narrow usage-read seam CollectCodexAccountUsage depends on.
// *codexauth.Client satisfies it, so the SAME injected client that backs the coordinated
// refresher (s.codexRefresh) also reads usage — no second network client with different
// bounds. Resolved via a safe-degrade assertion (codexUsageReader), mirroring codexStore /
// codexRefreshQueries.
type CodexUsageReader interface {
	ReadUsage(ctx context.Context, accessToken, workspaceAccountID string) (codexauth.UsageReading, error)
}

// codexUsageReader resolves the usage-read seam from the injected refresh client. It succeeds
// with the production *codexauth.Client and fails (false) with a client that lacks ReadUsage
// (a test fake wired only for refresh), so the caller degrades to a typed failure rather than
// panicking.
func (s *Service) codexUsageReader() (CodexUsageReader, bool) {
	r, ok := s.codexRefresh.(CodexUsageReader)
	return r, ok
}

// codexUsageStore is the narrow query surface the poll path reads/writes, kept off the broad
// Store interface for the same reason codexAuthzStore is. *store.Queries satisfies it.
type codexUsageStore interface {
	GetCodexProviderAccountByID(ctx context.Context, arg store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error)
	CountLinkedAliasesForCodexAccount(ctx context.Context, arg store.CountLinkedAliasesForCodexAccountParams) (int64, error)
	MarkCodexReauthRequired(ctx context.Context, arg store.MarkCodexReauthRequiredParams) (int64, error)
}

// codexUsageQueries adapts the Service's Store to the narrow usage surface, the same
// safe-degrade codexStore uses.
func (s *Service) codexUsageQueries() (codexUsageStore, bool) {
	q, ok := s.q.(codexUsageStore)
	return q, ok
}

// CollectCodexAccountUsage reads one owned, linked subscription account's rate-limit buckets
// and returns a token-free CodexUsageReading, or a typed CodexUsageFailure (PRD #1209 M2).
// The raw access token never leaves this package: it is opened here, handed to
// codexauth.ReadUsage, and discarded — nothing in the return type carries it.
//
// The steps mirror the release lane's authority discipline, owner-scoped rather than
// worker-scoped:
//
//  1. re-read the account (user, id) — refuse if it does not resolve;
//  2. prove a linked alias exists for it;
//  3. refuse a non-pollable coord state (in_progress/quarantined) and build the principal;
//  4. open the committed access token (a locked vault → vault_locked, prior reading kept);
//  5. ReadUsage with the persisted workspace id as ChatGPT-Account-Id;
//  6. identity-mismatch check BEFORE returning any reading;
//  7. rejected-access loop: a 401 does ONE bounded retry on a newer committed generation,
//     else one coordinated rotation + retry; a proven no-renewal expiry flags reauth; a bare
//     403 is transient (never a refresh, never reauth); a 429 backs off;
//  8. finalize: re-read and discard a reading whose authority moved.
func (s *Service) CollectCodexAccountUsage(ctx context.Context, userID, accountID uuid.UUID) (CodexUsageReading, error) {
	q, ok := s.codexUsageQueries()
	if !ok {
		return CodexUsageReading{}, codexUsageFail(CodexUsageFailTransient)
	}
	reader, ok := s.codexUsageReader()
	if !ok {
		return CodexUsageReading{}, codexUsageFail(CodexUsageFailTransient)
	}

	// (1) Re-read the account owner-scoped.
	acct, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: userID, ID: accountID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CodexUsageReading{}, codexUsageFail(CodexUsageFailNotLinked)
		}
		return CodexUsageReading{}, codexUsageFail(CodexUsageFailTransient)
	}

	// (2) Prove a linked alias for THIS account.
	if f := s.codexRequireLinkedAlias(ctx, q, userID, accountID); f != nil {
		return CodexUsageReading{}, f
	}

	// (3) Refuse a non-pollable state, then capture the principal.
	if f := codexInitialPollableState(acct); f != nil {
		return CodexUsageReading{}, f
	}
	principal := s.newCodexPollPrincipal(acct)

	// (4)-(7) Read the usage, with at most one bounded refresh/retry.
	reading, captured, f := s.readCodexUsage(ctx, q, reader, principal, acct)
	if f != nil {
		return CodexUsageReading{}, f
	}

	// (8) Post-call finalization: discard a reading whose authority moved under the call.
	if f := s.finalizeCodexUsage(ctx, q, captured); f != nil {
		return CodexUsageReading{}, f
	}

	return CodexUsageReading{
		Buckets:                    codexBucketsToDTO(reading.Buckets),
		ObservedGeneration:         captured.Generation,
		ObservedCredentialRevision: captured.CredentialRevision,
	}, nil
}

// readCodexUsage performs step 4-7: the first attempt with the committed token, and — on a
// 401 only — one bounded retry, either against a newer committed generation or after a single
// coordinated rotation. It returns the reading and the account row it was CAPTURED UNDER (so
// step 8 fences on the right generation), or a typed failure.
func (s *Service) readCodexUsage(ctx context.Context, q codexUsageStore, reader CodexUsageReader, principal codexPollPrincipal, acct store.CodexProviderAccount) (codexauth.UsageReading, store.CodexProviderAccount, *CodexUsageFailure) {
	reading, f, unauthorized := s.readCodexUsageAttempt(ctx, reader, acct)
	if !unauthorized {
		return reading, acct, f
	}
	return s.handleCodexUsageUnauthorized(ctx, q, reader, principal)
}

// readCodexUsageAttempt opens the account's committed access token and performs ONE ReadUsage.
// It returns (reading, failure, unauthorized): unauthorized is true ONLY for a 401, which the
// caller handles specially (the refresh loop); every other outcome is a terminal reading or a
// typed failure. The token is opened and discarded here — it never leaves this function.
func (s *Service) readCodexUsageAttempt(ctx context.Context, reader CodexUsageReader, acct store.CodexProviderAccount) (codexauth.UsageReading, *CodexUsageFailure, bool) {
	blob, err := s.openCodexAccountLogin(acct.UserID, acct)
	if err != nil {
		if errors.Is(err, errVaultLocked) {
			return codexauth.UsageReading{}, codexUsageFail(CodexUsageFailVaultLocked), false
		}
		// Undecryptable/malformed committed login: the login exists but cannot be read. Retry
		// next tick; do NOT set reauth (that is reserved for a proven-expired token).
		return codexauth.UsageReading{}, codexUsageFail(CodexUsageFailTransient), false
	}

	reading, rerr := reader.ReadUsage(ctx, blob.AccessToken, acct.WorkspaceAccountID)
	if rerr != nil {
		if codexUsageUnauthorized(rerr) {
			return codexauth.UsageReading{}, nil, true
		}
		return codexauth.UsageReading{}, classifyCodexUsageError(rerr), false
	}

	// (6) Identity-mismatch check BEFORE returning any reading. Never persist/log/return the
	// raw provider ids — only the boolean verdict leaves here.
	if !codexUsageIdentityMatches(acct, reading) {
		return codexauth.UsageReading{}, codexUsageFail(CodexUsageFailProviderMismatch), false
	}
	return reading, nil, false
}

// handleCodexUsageUnauthorized resolves a 401 (step 7). It re-reads the canonical generation:
// a newer committed generation gets ONE retry with the fresh committed token; otherwise, if
// the account is rotatable, it drives ONE coordinated rotation and retries once. A proven
// no-renewal expiry (ErrCodexRefreshNoToken) flags reauth_required. It never re-spends a
// refresh more than once and never sets reauth on anything but the proven-expired case.
func (s *Service) handleCodexUsageUnauthorized(ctx context.Context, q codexUsageStore, reader CodexUsageReader, principal codexPollPrincipal) (codexauth.UsageReading, store.CodexProviderAccount, *CodexUsageFailure) {
	fresh, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: principal.userID, ID: principal.accountID})
	if err != nil {
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailTransient)
	}

	// A newer generation was committed since step 1 (a concurrent refresh landed): retry ONCE
	// with the fresh committed token, no rotation of our own.
	if fresh.Generation > principal.generation {
		reading, f, unauthorized := s.readCodexUsageAttempt(ctx, reader, fresh)
		if unauthorized {
			// Still rejected even on the freshly committed token — not a proven no-renewal
			// expiry, so retry next tick rather than refresh again or flag reauth.
			return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailTransient)
		}
		return reading, fresh, f
	}

	// No newer generation. Rotate only if the account state permits it.
	if f := codexRotatableState(fresh); f != nil {
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, f
	}

	operationBudget := codexRefreshOperationBudget(ctx)
	if operationBudget <= codexRefreshCommitReserve {
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailTransient)
	}
	operationDeadline := time.Now().Add(operationBudget)
	operationCtx, cancel := context.WithDeadline(ctx, operationDeadline)
	defer cancel()
	leaseDeadline := s.codexNow().Add(operationBudget)
	providerDeadline := operationDeadline.Add(-codexRefreshCommitReserve)

	// The UNEXPORTED core, not the worker wrapper: authority here is the owner-scoped principal
	// path, not a worker capability. A fresh operation id + the observed generation drive one
	// bounded rotation.
	_, rerr := s.coordinatedRefresh(operationCtx, principal.userID, principal.accountID, uuid.New(), principal.generation, leaseDeadline, providerDeadline)
	if errors.Is(rerr, ErrCodexRefreshNoToken) {
		// Proven expired with no renewal material → flag reauth (fenced on the observed
		// counters; a 0-row result means the account moved and the flag is discarded).
		_, _ = q.MarkCodexReauthRequired(ctx, store.MarkCodexReauthRequiredParams{
			ObservedGeneration:         principal.generation,
			ObservedCredentialRevision: principal.credentialRevision,
			ID:                         principal.accountID,
			UserID:                     principal.userID,
		})
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailReauthRequired)
	}
	if rerr != nil {
		// Contended / quarantined / no client / transport: retry next tick, never reauth.
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailTransient)
	}

	// Rotation advanced (or reconciled to a committed generation). Re-read the account and
	// retry ReadUsage once against the now-committed token.
	advanced, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: principal.userID, ID: principal.accountID})
	if err != nil {
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailTransient)
	}
	reading, f, unauthorized := s.readCodexUsageAttempt(ctx, reader, advanced)
	if unauthorized {
		// A freshly refreshed token that is STILL rejected is not a no-renewal expiry — retry
		// next tick rather than loop the refresh or flag reauth.
		return codexauth.UsageReading{}, store.CodexProviderAccount{}, codexUsageFail(CodexUsageFailTransient)
	}
	return reading, advanced, f
}

// finalizeCodexUsage re-reads the account after the provider call and refuses a stale reading
// (step 8): a moved generation/credential_revision, a vanished last linked alias, or a locked
// vault all discard the reading so the poller keeps the prior good one.
func (s *Service) finalizeCodexUsage(ctx context.Context, q codexUsageStore, captured store.CodexProviderAccount) *CodexUsageFailure {
	fresh, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: captured.UserID, ID: captured.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return codexUsageFail(CodexUsageFailNotLinked)
		}
		return codexUsageFail(CodexUsageFailTransient)
	}
	if fresh.Generation != captured.Generation || fresh.CredentialRevision != captured.CredentialRevision {
		return codexUsageFail(CodexUsageFailTransient)
	}
	if f := s.codexRequireLinkedAlias(ctx, q, captured.UserID, captured.ID); f != nil {
		return f
	}
	if !s.codexVaultUnlocked(captured.UserID) {
		return codexUsageFail(CodexUsageFailVaultLocked)
	}
	return nil
}

// codexRequireLinkedAlias returns not_linked unless the account has at least one linked
// codex_auth alias owned by the user.
func (s *Service) codexRequireLinkedAlias(ctx context.Context, q codexUsageStore, userID, accountID uuid.UUID) *CodexUsageFailure {
	n, err := q.CountLinkedAliasesForCodexAccount(ctx, store.CountLinkedAliasesForCodexAccountParams{
		UserID:            userID,
		ProviderAccountID: pgconv.UUID(accountID),
	})
	if err != nil {
		return codexUsageFail(CodexUsageFailTransient)
	}
	if n <= 0 {
		return codexUsageFail(CodexUsageFailNotLinked)
	}
	return nil
}

// codexVaultUnlocked reports whether the owner's vault is open. A nil vault (the master-box
// test path / a deployment without the vault) is treated as unlocked, matching the run lane.
func (s *Service) codexVaultUnlocked(userID uuid.UUID) bool {
	return s.vlt == nil || s.vlt.Unlocked(userID)
}

// codexUsageIdentityMatches enforces the response-vs-account identity check. The response
// user_id MUST equal the account's provider_user_id; a PRESENT response account_id MUST equal
// the workspace id, while an ABSENT one is fine (a personal seat omits it). Only the boolean
// leaves the caller — the raw ids are never persisted, logged or returned.
func codexUsageIdentityMatches(acct store.CodexProviderAccount, reading codexauth.UsageReading) bool {
	if reading.UserID == "" || reading.UserID != acct.ProviderUserID {
		return false
	}
	if reading.AccountID != "" && reading.AccountID != acct.WorkspaceAccountID {
		return false
	}
	return true
}

// codexUsageUnauthorized reports whether err is a provider 401 — the ONLY status that drives
// the refresh loop. A 403 is deliberately NOT unauthorized here (it is a bare, actionable
// transient that must never trigger a refresh or reauth).
func codexUsageUnauthorized(err error) bool {
	var aerr *codexauth.AuthError
	return errors.As(err, &aerr) && aerr.StatusCode == http.StatusUnauthorized
}

// classifyCodexUsageError maps a NON-401 ReadUsage error to a typed failure: 429 →
// rate_limited (with the provider Retry-After), 403 / 5xx / other statuses → transient, and a
// transport or decode/duplicate-bucket error → transient (the last-good reading is preserved).
func classifyCodexUsageError(err error) *CodexUsageFailure {
	var aerr *codexauth.AuthError
	if errors.As(err, &aerr) {
		switch aerr.StatusCode {
		case http.StatusTooManyRequests:
			return &CodexUsageFailure{Kind: CodexUsageFailRateLimited, RetryAfter: aerr.RetryAfter}
		default:
			// 403 (bare, actionable), 5xx, or any other non-2xx — never a refresh, never reauth.
			return codexUsageFail(CodexUsageFailTransient)
		}
	}
	// Transport error, or an invalid/duplicate-bucket body — keep last-good, retry next tick.
	return codexUsageFail(CodexUsageFailTransient)
}

// codexInitialPollableState refuses an account that is not a poll target at step 3: an
// in-flight refresh (transient — retry next tick), a quarantine, an already-raised reauth
// flag, or a populated recovery slot (all disabled — a reconcile, not a poll, target).
func codexInitialPollableState(acct store.CodexProviderAccount) *CodexUsageFailure {
	switch acct.CoordState {
	case codexCoordInProgress:
		return codexUsageFail(CodexUsageFailTransient)
	case codexCoordQuarantined:
		return codexUsageFail(CodexUsageFailDisabled)
	}
	if acct.ReauthRequired || len(acct.RecoverySealed) > 0 {
		return codexUsageFail(CodexUsageFailDisabled)
	}
	return nil
}

// codexRotatableState reports whether a post-401 account may be rotated. Only an
// idle/committed account with no reauth flag and no recovery material is rotatable; anything
// else returns the matching failure (an in-flight refresh is transient; a quarantine / reauth
// flag / recovery slot is disabled).
func codexRotatableState(acct store.CodexProviderAccount) *CodexUsageFailure {
	switch acct.CoordState {
	case codexCoordIdle, codexCoordCommitted:
		if acct.ReauthRequired || len(acct.RecoverySealed) > 0 {
			return codexUsageFail(CodexUsageFailDisabled)
		}
		return nil
	case codexCoordQuarantined:
		return codexUsageFail(CodexUsageFailDisabled)
	default:
		// in_progress (or any unexpected state): a refresh is already under way, retry later.
		return codexUsageFail(CodexUsageFailTransient)
	}
}

// codexBucketsToDTO maps codexauth's normalized buckets onto the FROZEN apitypes DTO shape —
// the JSONB the poller persists and M3 reads back. Pointers carry through verbatim so an
// absent sub-field stays null rather than a misleading zero.
func codexBucketsToDTO(buckets []codexauth.UsageBucket) []apitypes.CodexRateLimitBucketDTO {
	out := make([]apitypes.CodexRateLimitBucketDTO, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, apitypes.CodexRateLimitBucketDTO{
			ID:           b.ID,
			DisplayName:  b.DisplayName,
			Allowed:      b.Allowed,
			LimitReached: b.LimitReached,
			Primary:      codexWindowToDTO(b.Primary),
			Secondary:    codexWindowToDTO(b.Secondary),
		})
	}
	return out
}

func codexWindowToDTO(w *codexauth.UsageWindow) *apitypes.CodexRateLimitWindowDTO {
	if w == nil {
		return nil
	}
	return &apitypes.CodexRateLimitWindowDTO{
		UsedPercent:        w.UsedPercent,
		LimitWindowSeconds: w.LimitWindowSeconds,
		ResetAfterSeconds:  w.ResetAfterSeconds,
		ResetAt:            w.ResetAt,
	}
}
