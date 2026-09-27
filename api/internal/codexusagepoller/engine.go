// Package codexusagepoller is the per-ACCOUNT Codex rate-limit poller (PRD #1209 M2), the
// Codex sibling of usagepoller (the Claude per-token engine). It is a Boot-pass-plus-ticker
// background engine wired in main.go under the background WaitGroup; 0 disables it entirely.
//
// Each pass does TWO phases, in order:
//
// Background work follows PRD #1732 D6: disable is per alias, liveness is per account. A
// staged alias is reconciled only while THAT alias is enabled, and an account is polled (and
// so background-refreshed, since the poll's 401 path is the only background refresh) only
// while at least one ENABLED linked alias resolves to it. Both rules live in the listing
// queries, so a disabled credential is not listed. The listing is check-then-act: a disable
// that commits between the listing and the reconcile (or recovery promotion) can still let
// one nonrotating identity call through, which spends no token (ADR 1732 D6).
//
//  1. RECONCILE staged aliases FIRST. A freshly imported codex_auth login sits 'staging'
//     until its identity is established; this engine is its first production caller of
//     ReconcileCodexAuthIdentity (a NONROTATING DiscoverIdentity — it never spends a
//     refresh). A PROVEN unusable credential — a 401/403 auth rejection, or a 2xx whose
//     identity was incomplete — marks the alias 'failed' (terminal, no longer enumerated
//     as staging, so not re-probed every tick); a transient failure (transport timeout,
//     429, 5xx, undecodable body) stays 'staging' and is retried under an in-memory
//     backoff, exactly like the Claude engine.
//  2. POLL linked accounts. For each canonical linked account it calls the workersvc
//     collector, whose return type carries NO token — only a normalized bucket set. A
//     reading is written with the revision-fenced UpsertCodexAccountRateLimits; a typed
//     failure with the revision-fenced RecordCodexAccountPollFailure (which preserves the
//     last-good reading). Both queries return 0 rows when authority moved — a discard, not an
//     error. Both are also fenced on the account's enablement list captured by the listing
//     (PRD #1732 D13), so a poll that started before its last enabled alias was disabled
//     writes nothing, even if the alias was re-enabled before the write.
//
// A POKE (credential save, login, vault unlock, re-enable) additionally runs the existing
// crash-safe recovery pass on the user's live accounts that are not pollable because of an
// unfinished refresh or a protected recovery slot, BEFORE polling them (PRD #1732 D7): the
// poll that follows performs the normal coordinated refresh on a 401, and only a proven
// no-renewal expiry or a provider rejection flags reauth_required (a new login paste).
//
// The engine makes NO model calls and has NO run/worker dependency: the Codex usage GET is
// free (it reads the account's own meter, spending no token). Bounded concurrency, a per-tick
// deadline of one interval, and Retry-After/backoff for 429/transient keep provider egress to
// the fixed usage host bounded.
package codexusagepoller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// defaultMaxConcurrency bounds how many accounts are polled in parallel per pass (mirrors the
// Claude engine's fan-out bound).
const defaultMaxConcurrency = 4

// defaultBackoff is the in-memory backoff after a transient / provider-mismatch failure, or a
// rate-limit with no Retry-After hint. It comfortably exceeds the default 5m interval so a
// persistently-failing account is not retried every tick.
const defaultBackoff = 15 * time.Minute

// pokeBuffer bounds the poke channel; a full buffer drops the signal (the next tick covers the
// user anyway), so a caller never blocks on the poller.
const pokeBuffer = 64

// attemptStatusOK is the raw poll attempt status persisted on a successful reading (M3 derives
// the user-facing status separately).
const attemptStatusOK = "ok"

// Store is the query surface the engine needs; *store.Queries satisfies it. The two staged
// listings drive phase 1, the two linked listings drive phase 2, and the two revision-fenced
// writes persist the outcome.
type Store interface {
	ListStagedCodexAliases(ctx context.Context) ([]store.ListStagedCodexAliasesRow, error)
	ListStagedCodexAliasesForUser(ctx context.Context, userID uuid.UUID) ([]store.ListStagedCodexAliasesForUserRow, error)
	ListLinkedCodexAccountsToPoll(ctx context.Context) ([]store.ListLinkedCodexAccountsToPollRow, error)
	ListLinkedCodexAccountsForUser(ctx context.Context, userID uuid.UUID) ([]store.ListLinkedCodexAccountsForUserRow, error)
	UpsertCodexAccountRateLimits(ctx context.Context, arg store.UpsertCodexAccountRateLimitsParams) (int64, error)
	RecordCodexAccountPollFailure(ctx context.Context, arg store.RecordCodexAccountPollFailureParams) (int64, error)
}

// UsageCollector reads one account's usage and returns a token-free reading or a typed
// failure. *workersvc.Service satisfies it. The engine depends on the interface so its tests
// can inject a fake collector returning each outcome without a live store or provider.
type UsageCollector interface {
	CollectCodexAccountUsage(ctx context.Context, userID, accountID uuid.UUID) (workersvc.CodexUsageReading, error)
}

// Reconciler establishes a staged alias's identity and links it (NONROTATING).
// *workersvc.CodexReconciler satisfies it.
type Reconciler interface {
	ReconcileCodexAuthIdentity(ctx context.Context, userID, userSecretID uuid.UUID) error
}

// Recoverer runs the crash-safe recovery pass for one account: reap an expired refresh lease,
// resolve orphaned refresh intents, and promote identity-verified recovery material. It never
// re-spends a refresh token. *workersvc.Service satisfies it; the engine discovers it on the
// collector (the production collector IS the Service), so a test collector that does not
// implement it simply skips poke-time recovery.
type Recoverer interface {
	ReconcileUnresolvedCodexRefresh(ctx context.Context, userID, accountID uuid.UUID) (int, error)
}

// Engine is the Codex rate-limit poller.
type Engine struct {
	store      Store
	collector  UsageCollector
	reconciler Reconciler
	interval   time.Duration
	maxConc    int
	now        func() time.Time
	logger     *slog.Logger

	// mu guards the two backoff maps (written by concurrent per-account goroutines).
	mu sync.Mutex
	// pollBackoff is keyed by ACCOUNT id: a rate-limited/transient account backs off only
	// itself, never its siblings.
	pollBackoff map[uuid.UUID]time.Time
	// reconcileBackoff is keyed by ALIAS id (user_secret_id): a transient reconcile failure
	// backs off just that staged alias so a stuck import is not re-probed every tick.
	reconcileBackoff map[uuid.UUID]time.Time

	// poke carries poke-on-credential-save signals: a user whose codex credential was just
	// saved is reconciled + polled out-of-band so their meters appear in seconds.
	poke chan uuid.UUID
}

// New builds an Engine. interval is the poll cadence; the caller starts the engine only when
// interval > 0 (0 disables it). logger may be nil.
func New(st Store, collector UsageCollector, reconciler Reconciler, interval time.Duration, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		store:            st,
		collector:        collector,
		reconciler:       reconciler,
		interval:         interval,
		maxConc:          defaultMaxConcurrency,
		now:              time.Now,
		logger:           logger,
		pollBackoff:      make(map[uuid.UUID]time.Time),
		reconcileBackoff: make(map[uuid.UUID]time.Time),
		poke:             make(chan uuid.UUID, pokeBuffer),
	}
}

// Poke requests an out-of-band reconcile+poll for one user. Non-blocking: it drops the signal
// when the buffer is full (the next tick covers the user regardless), satisfying the
// handler.CodexUsagePoker interface structurally.
func (e *Engine) Poke(userID uuid.UUID) {
	select {
	case e.poke <- userID:
	default:
	}
}

// Boot runs one immediate pass at start so a credential saved while the API was down gets
// reconciled + metered promptly, not one interval later. Non-fatal on failure.
func (e *Engine) Boot(ctx context.Context) { e.tickAll(ctx) }

// Run blocks until ctx is cancelled, polling every interval and servicing pokes.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	e.logger.Info("codex usage poller started", "interval", e.interval.String())
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("codex usage poller stopped")
			return
		case userID := <-e.poke:
			e.pokeUser(ctx, userID)
		case <-ticker.C:
			e.tickAll(ctx)
		}
	}
}

// tickAll runs one factory-wide pass: reconcile every staged alias, then poll every canonical
// linked account, with a per-tick deadline of one interval so a pile-up cannot run past the
// next tick.
func (e *Engine) tickAll(ctx context.Context) {
	tickCtx, cancel := context.WithTimeout(ctx, e.interval)
	defer cancel()

	// Phase 1: reconcile staged aliases FIRST (factory-wide).
	staged, err := e.store.ListStagedCodexAliases(tickCtx)
	if err != nil {
		e.logger.Error("codex usage poller: list staged aliases", "error", err)
	} else {
		for _, s := range staged {
			e.reconcileAlias(tickCtx, s.UserID, s.UserSecretID, false)
		}
	}

	// Phase 2: poll linked accounts (factory-wide), bounded concurrency.
	rows, err := e.store.ListLinkedCodexAccountsToPoll(tickCtx)
	if err != nil {
		e.logger.Error("codex usage poller: list linked accounts", "error", err)
		return
	}
	targets := make([]pollTarget, 0, len(rows))
	for _, r := range rows {
		targets = append(targets, pollTargetFromToPollRow(r))
	}
	e.pollAccounts(tickCtx, targets, false)
}

// pokeUser runs an out-of-band pass for ONE user: reconcile their enabled staged aliases,
// recover their live accounts left unpollable by an unfinished refresh or a protected
// recovery slot, then poll their live accounts, ignoring any prior backoff (a just-saved or
// just-enabled credential may work where an earlier attempt failed).
func (e *Engine) pokeUser(ctx context.Context, userID uuid.UUID) {
	pokeCtx, cancel := context.WithTimeout(ctx, e.interval)
	defer cancel()

	staged, err := e.store.ListStagedCodexAliasesForUser(pokeCtx, userID)
	if err != nil {
		e.logger.Error("codex usage poller: list staged aliases for poke", "user", userID.String(), "error", err)
	} else {
		for _, s := range staged {
			e.reconcileAlias(pokeCtx, s.UserID, s.UserSecretID, true)
		}
	}

	targets, ok := e.listUserTargets(pokeCtx, userID)
	if !ok {
		return
	}
	// Recovery first (PRD #1732 D7). The listing only returns accounts with an enabled linked
	// alias, so recovery is gated exactly like polling. A recovery attempt may change the
	// account's state (a promotion advances its generation and makes it pollable), so the
	// targets are re-read before polling rather than polled on the stale row.
	if e.recoverAccounts(pokeCtx, targets) {
		if targets, ok = e.listUserTargets(pokeCtx, userID); !ok {
			return
		}
	}
	e.pollAccounts(pokeCtx, targets, true)
}

// listUserTargets lists one user's live (enabled-linked) accounts as poll targets.
func (e *Engine) listUserTargets(ctx context.Context, userID uuid.UUID) ([]pollTarget, bool) {
	rows, err := e.store.ListLinkedCodexAccountsForUser(ctx, userID)
	if err != nil {
		e.logger.Error("codex usage poller: list linked accounts for poke", "user", userID.String(), "error", err)
		return nil, false
	}
	targets := make([]pollTarget, 0, len(rows))
	for _, r := range rows {
		targets = append(targets, pollTargetFromForUserRow(r))
	}
	return targets, true
}

// recoverAccounts runs the recovery pass on every target that needs it (an unfinished refresh
// or a protected recovery slot) and reports whether it attempted any. It is a no-op when the
// collector does not implement Recoverer. A failure is logged and never blocks the poll.
func (e *Engine) recoverAccounts(ctx context.Context, targets []pollTarget) bool {
	rec, ok := e.collector.(Recoverer)
	if !ok {
		return false
	}
	attempted := false
	for _, t := range targets {
		if !targetNeedsRecovery(t) {
			continue
		}
		attempted = true
		if _, err := rec.ReconcileUnresolvedCodexRefresh(ctx, t.userID, t.accountID); err != nil {
			e.logger.Info("codex usage poller: recover account", "account", t.accountID.String(), "error", err.Error())
		}
	}
	return attempted
}

// targetNeedsRecovery reports whether a live account is a recovery target: a refresh that did
// not settle (in progress, or quarantined) or a populated recovery slot. A reauth-flagged
// account with neither needs a new login, which recovery cannot provide.
func targetNeedsRecovery(t pollTarget) bool {
	return t.hasRecovery || t.coordState == "in_progress" || t.coordState == "quarantined"
}

// pollTarget is the common shape both linked listings project to, so pollAccounts is shared
// between the tick and poke paths.
type pollTarget struct {
	userID             uuid.UUID
	accountID          uuid.UUID
	generation         int64
	credentialRevision int64
	coordState         string
	reauthRequired     bool
	hasRecovery        bool
	// enablementSig is the account's linked-alias enablement list at listing time (PRD #1732
	// D13): both writes are fenced on it, so a poll that raced an enable/disable writes nothing.
	enablementSig string
}

func pollTargetFromToPollRow(r store.ListLinkedCodexAccountsToPollRow) pollTarget {
	return pollTarget{
		userID:             r.UserID,
		accountID:          r.ProviderAccountID,
		generation:         r.Generation,
		credentialRevision: r.CredentialRevision,
		coordState:         r.CoordState,
		reauthRequired:     r.ReauthRequired,
		hasRecovery:        r.HasRecovery,
		enablementSig:      r.EnablementSig,
	}
}

func pollTargetFromForUserRow(r store.ListLinkedCodexAccountsForUserRow) pollTarget {
	return pollTarget{
		userID:             r.UserID,
		accountID:          r.ProviderAccountID,
		generation:         r.Generation,
		credentialRevision: r.CredentialRevision,
		coordState:         r.CoordState,
		reauthRequired:     r.ReauthRequired,
		hasRecovery:        r.HasRecovery,
		enablementSig:      r.EnablementSig,
	}
}

// pollAccounts fans out one goroutine per account, bounded by maxConc.
func (e *Engine) pollAccounts(ctx context.Context, targets []pollTarget, ignoreBackoff bool) {
	sem := make(chan struct{}, e.maxConc)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(t pollTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			e.pollAccount(ctx, t, ignoreBackoff)
		}(t)
	}
	wg.Wait()
}

// pollAccount polls ONE account: it skips a non-pollable account (an in-flight refresh, a
// quarantine, an already-raised reauth flag, or a populated recovery slot — all reconcile
// targets, not poll targets, identified from the listing row so no work is spent), honours the
// in-memory backoff, then collects and persists the outcome.
func (e *Engine) pollAccount(ctx context.Context, t pollTarget, ignoreBackoff bool) {
	if !targetPollable(t) {
		return
	}
	if ignoreBackoff {
		e.clearPollBackoff(t.accountID)
	} else if e.inPollBackoff(t.accountID) {
		return
	}

	reading, err := e.collector.CollectCodexAccountUsage(ctx, t.userID, t.accountID)
	if err != nil {
		e.recordFailure(ctx, t, err)
		return
	}
	e.writeReading(ctx, t, reading)
	e.clearPollBackoff(t.accountID)
}

// targetPollable reports whether the listing row names a poll target: a linked account that is
// idle/committed, not reauth-flagged, and carrying no recovery material. Everything else is a
// reconcile / re-login target the poll path must not touch (deliverable C: skip an in-flight
// refresh, an already-flagged account, and a populated recovery slot).
func targetPollable(t pollTarget) bool {
	if t.reauthRequired || t.hasRecovery {
		return false
	}
	switch t.coordState {
	case "idle", "committed":
		return true
	default:
		return false
	}
}

// writeReading marshals the reading's buckets to JSONB and installs it under the observed
// (generation, credential_revision) fence. 0 rows means authority moved between the poll and
// the write — a discard, not an error.
func (e *Engine) writeReading(ctx context.Context, t pollTarget, reading workersvc.CodexUsageReading) {
	raw, err := json.Marshal(reading.Buckets)
	if err != nil {
		// Buckets are our own sanitized DTOs, so this is effectively unreachable; log and skip
		// rather than write a broken snapshot.
		e.logger.Error("codex usage poller: marshal buckets", "account", t.accountID.String(), "error", err)
		return
	}
	n, err := e.store.UpsertCodexAccountRateLimits(ctx, store.UpsertCodexAccountRateLimitsParams{
		UserID:                     t.userID,
		ProviderAccountID:          t.accountID,
		Buckets:                    raw,
		ObservedGeneration:         reading.ObservedGeneration,
		ObservedCredentialRevision: reading.ObservedCredentialRevision,
		AttemptStatus:              attemptStatusOK,
		EnablementSig:              t.enablementSig,
	})
	if err != nil {
		e.logger.Error("codex usage poller: upsert reading", "account", t.accountID.String(), "error", err)
		return
	}
	if n == 0 {
		// Fence rejected the write (generation/credential_revision moved, the last linked
		// alias vanished, no linked alias is enabled any more, or an alias's enablement moved
		// since the listing) — the reading is discarded, the prior good row untouched.
		e.logger.Info("codex usage poller: reading discarded (authority moved)", "account", t.accountID.String())
	}
}

// recordFailure classifies a typed failure, records the health-only failure (fenced on the
// listing's observed counters, so it never overwrites a good reading and is discarded when
// authority moved), and arms the backoff the kind warrants.
func (e *Engine) recordFailure(ctx context.Context, t pollTarget, err error) {
	kind, retryAfter := classifyFailure(err)
	if _, werr := e.store.RecordCodexAccountPollFailure(ctx, store.RecordCodexAccountPollFailureParams{
		UserID:                     t.userID,
		ProviderAccountID:          t.accountID,
		AttemptStatus:              kind.String(),
		AttemptError:               failureMessage(kind),
		ObservedGeneration:         t.generation,
		ObservedCredentialRevision: t.credentialRevision,
		EnablementSig:              t.enablementSig,
	}); werr != nil {
		e.logger.Error("codex usage poller: record failure", "account", t.accountID.String(), "error", werr)
	}
	if backoff := backoffFor(kind, retryAfter); backoff > 0 {
		e.setPollBackoff(t.accountID, backoff)
	}
}

// reconcileAlias reconciles ONE staged alias under its own in-memory backoff. The reconciler
// marks a proven unusable credential terminal ('failed') — a 401/403 auth rejection or an
// incomplete identity — so it drops out of the staging list and is not re-probed; a transient
// failure stays 'staging', and the backoff keeps it from being re-probed every tick.
func (e *Engine) reconcileAlias(ctx context.Context, userID, aliasID uuid.UUID, ignoreBackoff bool) {
	if ignoreBackoff {
		e.clearReconcileBackoff(aliasID)
	} else if e.inReconcileBackoff(aliasID) {
		return
	}
	if err := e.reconciler.ReconcileCodexAuthIdentity(ctx, userID, aliasID); err != nil {
		// Best-effort: the reconciler already recorded a terminal 'failed' status with a reason
		// where the credential proved unusable (a 401/403, or an incomplete identity); a transient
		// failure it left 'staging'. Either way, back off so a still-staging alias is not re-probed
		// every tick.
		e.logger.Info("codex usage poller: reconcile staged alias", "user", userID.String(), "alias", aliasID.String(), "error", err.Error())
		e.setReconcileBackoff(aliasID, defaultBackoff)
		return
	}
	e.clearReconcileBackoff(aliasID)
}

// classifyFailure extracts the typed failure kind and any Retry-After. A non-typed error
// (never expected from the collector, which returns only *CodexUsageFailure) is treated as
// transient so the health write and backoff still happen.
func classifyFailure(err error) (workersvc.CodexUsageFailureKind, time.Duration) {
	var f *workersvc.CodexUsageFailure
	if errors.As(err, &f) {
		return f.Kind, f.RetryAfter
	}
	return workersvc.CodexUsageFailTransient, 0
}

// backoffFor returns the backoff a failure kind warrants: a rate-limit uses its Retry-After
// (or the default when absent); a transient / provider-mismatch uses the default; every other
// kind (reauth/disabled/not_linked) arms none — those accounts drop out of the pollable set
// or are fine to re-record.
func backoffFor(kind workersvc.CodexUsageFailureKind, retryAfter time.Duration) time.Duration {
	switch kind {
	case workersvc.CodexUsageFailRateLimited:
		if retryAfter > 0 {
			return retryAfter
		}
		return defaultBackoff
	case workersvc.CodexUsageFailTransient, workersvc.CodexUsageFailProviderMismatch:
		return defaultBackoff
	default:
		return 0
	}
}

// failureMessage is the bounded, sanitized-by-construction attempt_error text for a failure
// kind. It is a FIXED string per kind — never provider data — so no token, body or raw id can
// ever reach the stored health row.
func failureMessage(kind workersvc.CodexUsageFailureKind) string {
	switch kind {
	case workersvc.CodexUsageFailReauthRequired:
		return "subscription login expired; re-login required"
	case workersvc.CodexUsageFailVaultLocked:
		return "vault locked; last reading retained"
	case workersvc.CodexUsageFailRateLimited:
		return "provider rate-limited the usage read"
	case workersvc.CodexUsageFailProviderMismatch:
		return "usage response identity did not match the account"
	case workersvc.CodexUsageFailTransient:
		return "transient usage read failure"
	case workersvc.CodexUsageFailDisabled:
		return "account not in a pollable state"
	case workersvc.CodexUsageFailNotLinked:
		return "account has no linked subscription alias"
	default:
		return "usage read failed"
	}
}

func (e *Engine) inPollBackoff(accountID uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.pollBackoff[accountID]
	return ok && e.now().Before(until)
}

func (e *Engine) setPollBackoff(accountID uuid.UUID, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pollBackoff[accountID] = e.now().Add(d)
}

func (e *Engine) clearPollBackoff(accountID uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.pollBackoff, accountID)
}

func (e *Engine) inReconcileBackoff(aliasID uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.reconcileBackoff[aliasID]
	return ok && e.now().Before(until)
}

func (e *Engine) setReconcileBackoff(aliasID uuid.UUID, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reconcileBackoff[aliasID] = e.now().Add(d)
}

func (e *Engine) clearReconcileBackoff(aliasID uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.reconcileBackoff, aliasID)
}
