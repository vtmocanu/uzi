package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled.go is the server side of the credential_disabled park (PRD #1732 D14):
// the claim-time classification sentinel, and the automatic promoter that returns a parked run
// to queued once its exact credential requirement is enabled again.
//
// Lock order, shared by every writer that can change a parked run's requirement:
//
//  1. the per-user secret mutation advisory lock (store.LockSecretMutation), always first;
//  2. the run row FOR UPDATE;
//  3. the requirement source row FOR SHARE: the recorded worker (worker binding) or the users
//     row (Judge / self-improve binding);
//  4. the credential row FOR SHARE.
//
// Enable/disable, default changes, pool opt-in, worker and Judge rebinding (SetWorkerAnthropicToken,
// SetUserJudgeBinding) and the per-run reassignment of a HELD run (SetRunCredential's
// credential_disabled branch) take (1) first, so each promotion decision is serialized against
// them. Other requirement writers do not take (1): the override of a run in any other state, and
// the held-state credential switch (StampHeldCredentialSwitch / ReleaseCredentialSwitch), touch
// runs that are not parked here and never race this hold. The promotion UPDATE is additionally
// guarded on the requirement columns it evaluated, so a writer outside (1) that does reach a held
// run matches 0 rows instead of promoting on a stale requirement. The claim finisher never takes
// (1): it holds only its run row and uses NOWAIT for every further lock.

// errCredentialDisabled is the claim-assembly outcome for a run whose resolved credential is
// locally disabled (PRD #1732 D2/D14). It is neither transient (a requeue would spin) nor
// terminal: finishRunClaimTx parks the exact undelivered claim on paused/credential_disabled
// and the promoter resumes it.
var errCredentialDisabled = errors.New("credential disabled")

const (
	// credentialPromotePageSize bounds one worklist page (runs of one owner, or owners).
	credentialPromotePageSize = 100
	// credentialPromoteConcurrency bounds how many owners' requested passes run at once.
	credentialPromoteConcurrency = 4
	// credentialPromotePassTimeout bounds one requested pass for one owner.
	credentialPromotePassTimeout = 30 * time.Second
)

// credentialPromoteTestHooks are the promoter's LiveDB race seams (nil in production).
type credentialPromoteTestHooks struct {
	// beforePromote runs inside a candidate's transaction, after every lock is held and the
	// requirement evaluated as met, before the guarded promote UPDATE. A non-nil error aborts
	// that candidate as a promotion error (rolled back).
	beforePromote func(ctx context.Context, runID uuid.UUID) error
}

// credentialPromoteQueue coalesces requested promoter passes per owner. Its zero value is ready
// to use. A request for an owner whose pass is already running marks it dirty, so the running
// pass loops once more instead of starting a second goroutine; distinct owners run in their own
// goroutines, at most credentialPromoteConcurrency at a time.
type credentialPromoteQueue struct {
	mu      sync.Mutex
	running map[uuid.UUID]bool
	dirty   map[uuid.UUID]bool
	sem     chan struct{}
}

// RequestCredentialDisabledPromotion asks for an immediate credential_disabled promoter pass
// over one owner's held runs (PRD #1732 D14). It never blocks: the pass runs through the
// Service's background dispatcher and is coalesced per owner. Callers invoke it only AFTER the
// transaction that changed a requirement committed, never while holding a lock. The Sweep pass
// is the restart-safe fallback when a request is lost with the process.
func (s *Service) RequestCredentialDisabledPromotion(userID uuid.UUID) {
	if s == nil || userID == uuid.Nil {
		return
	}
	p := &s.credPromote
	p.mu.Lock()
	if p.running == nil {
		p.running, p.dirty = map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
		p.sem = make(chan struct{}, credentialPromoteConcurrency)
	}
	if p.running[userID] {
		p.dirty[userID] = true
		p.mu.Unlock()
		return
	}
	p.running[userID] = true
	sem := p.sem
	p.mu.Unlock()
	s.background(func() {
		sem <- struct{}{}
		defer func() { <-sem }()
		// released is set when the loop hands the owner back under p.mu, in the SAME critical
		// section as its last dirty check, so a request that lands after that check starts a
		// fresh goroutine instead of being dropped. The deferred cleanup covers only a
		// panicking pass: it releases the owner so later requests are not swallowed into a
		// dirty flag no goroutine drains, and the panic is recovered and logged (a best-effort
		// background pass must not take the process down; Sweep retries the owner).
		released := false
		defer func() {
			if r := recover(); r != nil {
				slog.Error("credential promoter: requested pass panicked", "user", userID, "panic", r)
			}
			if !released {
				p.mu.Lock()
				delete(p.running, userID)
				delete(p.dirty, userID)
				p.mu.Unlock()
			}
		}()
		for {
			s.runCredentialPromotePass(userID)
			p.mu.Lock()
			if p.dirty[userID] {
				delete(p.dirty, userID)
				p.mu.Unlock()
				continue
			}
			delete(p.running, userID)
			released = true
			p.mu.Unlock()
			return
		}
	})
}

// runCredentialPromotePass is one bounded requested pass for one owner.
func (s *Service) runCredentialPromotePass(userID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), credentialPromotePassTimeout)
	defer cancel()
	if _, err := s.promoteCredentialDisabledRuns(ctx, userID); err != nil {
		slog.Error("credential promoter: requested pass", "user", userID, "error", err)
	}
}

// promoteAllCredentialDisabledRuns is the Sweep fallback: it pages every owner with a held run
// and runs that owner's pass. A worklist read error fails the pass; a per-owner error is logged
// and the sweep continues with the next owner. Without a transaction beginner there is no
// fenced promoter, so it promotes nothing.
func (s *Service) promoteAllCredentialDisabledRuns(ctx context.Context) (int64, error) {
	if s.txBeginner == nil {
		return 0, nil
	}
	var total int64
	var after pgtype.UUID
	for {
		users, err := s.q.ListCredentialDisabledUsers(ctx, store.ListCredentialDisabledUsersParams{
			AfterUserID: after, PageSize: credentialPromotePageSize,
		})
		if err != nil {
			return total, fmt.Errorf("list credential-disabled owners: %w", err)
		}
		for _, userID := range users {
			n, err := s.promoteCredentialDisabledRuns(ctx, userID)
			total += n
			if err != nil {
				slog.Error("credential promoter: sweep pass", "user", userID, "error", err)
			}
		}
		if len(users) < credentialPromotePageSize {
			return total, nil
		}
		after = pgtype.UUID{Bytes: users[len(users)-1], Valid: true}
	}
}

// promoteCredentialDisabledRuns pages one owner's held runs oldest first and tries each in its
// own transaction. A run that stays held (requirement still disabled, owner pause, spent
// budget) is skipped by the keyset cursor, so one blocked run never starves the rest. A
// candidate whose transaction errors is logged and skipped the same way; the pass carries on
// with the owner's other runs and returns the joined candidate errors with the promoted count.
// Only a worklist read error stops the pass.
func (s *Service) promoteCredentialDisabledRuns(ctx context.Context, userID uuid.UUID) (int64, error) {
	if s.txBeginner == nil {
		return 0, nil
	}
	var promoted int64
	var errs []error
	params := store.ListCredentialDisabledRunsParams{UserID: userID, PageSize: credentialPromotePageSize}
	for {
		page, err := s.q.ListCredentialDisabledRuns(ctx, params)
		if err != nil {
			errs = append(errs, fmt.Errorf("list credential-disabled runs: %w", err))
			return promoted, errors.Join(errs...)
		}
		for _, r := range page {
			status, err := s.promoteCredentialDisabledRun(ctx, userID, r.ID)
			if err != nil {
				slog.Error("credential promoter: candidate", "user", userID, "run", r.ID, "error", err)
				errs = append(errs, fmt.Errorf("promote credential-disabled run %s: %w", r.ID, err))
				continue
			}
			switch status {
			case "queued":
				promoted++
				s.publishSwept(r.ID, "queued")
			case "paused":
				// Settled into the pending pause (owner pause or wall hold): still paused, but
				// its hold changed, so clients re-render it.
				s.publishSwept(r.ID, "paused")
			}
		}
		if len(page) < credentialPromotePageSize {
			return promoted, errors.Join(errs...)
		}
		last := page[len(page)-1]
		params.AfterStatusSince, params.AfterID = last.StatusSince, pgtype.UUID{Bytes: last.ID, Valid: true}
	}
}

// promoteCredentialDisabledRun is one candidate's transaction, in the fixed lock order above.
// It returns the status the run left the hold in: "queued" when it was promoted, "paused" when
// its requirement is met but a pending pause request settled it into that pause instead, or ""
// when it stays held (or is no longer this hold).
func (s *Service) promoteCredentialDisabledRun(ctx context.Context, userID, runID uuid.UUID) (string, error) {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// (1) The user lock, before any run lock.
	if err := store.LockSecretMutation(ctx, tx, userID); err != nil {
		return "", err
	}
	q := store.New(tx)
	// (2) The run row, re-read under its lock.
	run, err := q.LockCredentialDisabledRunForPromotion(ctx, store.LockCredentialDisabledRunForPromotionParams{ID: runID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if run.Status != "paused" || !run.HoldReason.Valid || run.HoldReason.String != "credential_disabled" {
		return "", nil // no longer this hold
	}
	// (3) + (4) The requirement source and the requirement credential.
	heartbeatCutoff := pgconv.Time(s.now().Add(-s.p.WorkerHeartbeatStale))
	met, err := credentialRequirementMet(ctx, q, run, heartbeatCutoff)
	if err != nil || !met {
		return "", err
	}
	if h := s.credPromoteHooks; h != nil && h.beforePromote != nil {
		if err := h.beforePromote(ctx, runID); err != nil {
			return "", err
		}
	}
	status := "queued"
	if run.PauseRequestedAt.Valid {
		// A pending pause request rode the requeue into this claim (a worker that died after
		// CreatePauseInput or RequestWallParks is requeued with the request intact, by design).
		// The promoter never bypasses it (D14): the request is consumed into the pause it asked
		// for, never promoted past and never left stranded on a hold nothing else releases.
		_, err = q.SettleCredentialDisabledPause(ctx, store.SettleCredentialDisabledPauseParams{
			ID: runID, UserID: userID,
			ExpectedOverrideMode:     run.CredentialOverrideMode,
			ExpectedOverrideSecretID: run.CredentialOverrideSecretID,
			ExpectedCodexSecretID:    run.CodexSecretID,
			ExpectedWorkerID:         run.WorkerID,
		})
		status = "paused"
	} else {
		_, err = q.PromoteCredentialDisabledRun(ctx, store.PromoteCredentialDisabledRunParams{
			ID: runID, UserID: userID,
			ExpectedOverrideMode:     run.CredentialOverrideMode,
			ExpectedOverrideSecretID: run.CredentialOverrideSecretID,
			ExpectedCodexSecretID:    run.CodexSecretID,
			ExpectedWorkerID:         run.WorkerID,
			GlobalTimeoutSeconds:     int32(s.p.RunTimeout.Seconds()),
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil // spent budget, or a guard saw a changed requirement
	}
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return status, nil
}

// credentialRequirementMet re-evaluates the credential the run's NEXT claim will need, from
// the locked run row, and reports whether it is available. It mirrors the claim ladder
// (claimSecretID / judgeChoice / harness-authoritative Codex) rather than the previous claim:
//
//   - a Codex run needs its frozen alias;
//   - judge and self_improve follow the owner's Judge binding (a pin needs that credential, the
//     default mode needs an enabled default, auto needs an enabled pooled token or default, the
//     #1140 fallback); chat always needs the enabled default (it is not bindable);
//   - otherwise a per-run override decides (pinned id, default, or auto), and a nulled pin or
//     no override inherits the binding of the worker the next claim will use.
//
// An ordinary auto lane (override or worker binding) is always promoted (PRD #1732 D2): it has
// no single disabled pin to wait for. ListAutoSelectCandidates never offers a disabled token,
// so the next claim either spends an enabled pooled token or, with none, holds the run in
// pool_wait (errAutoPoolEmpty). It cannot cycle park -> promote -> park.
//
// A default requirement with no default at all (the slot's last credential was disabled, D4)
// keeps a Judge/self-improve/chat run held (D2: it parks rather than fabricating a fallback)
// while the owner still holds (disabled) tokens, while an ordinary run is promoted so its next
// claim behaves as it does today for a user with no token (D15). An owner who has since deleted
// every token has nothing left to enable, so a held default-only run is promoted too and its
// next claim fails exactly as a token-less owner's always has.
//
// The next claimant follows ClaimRun's affinity: the kept worker_id pins a queued run to its
// worker while that worker is draining or heartbeat-fresh (holds_affinity, read against
// heartbeatCutoff), and falls open to any of the owner's workers otherwise. A worker that does
// not hold affinity is therefore not the requirement source, and the run is promoted so the
// actual claimant decides with its own binding (and parks again, on its own worker, if that
// binding is disabled too).
//
// A different default never satisfies an explicit pin. A requirement that can no longer be
// resolved (no recorded worker, a deleted credential) is promoted, so the next claim decides
// with its own checks instead of the run waiting forever on something that no longer exists.
func credentialRequirementMet(ctx context.Context, q *store.Queries, run store.LockCredentialDisabledRunForPromotionRow, heartbeatCutoff pgtype.Timestamptz) (bool, error) {
	if run.Harness == harnessCodex {
		if !run.CodexSecretID.Valid {
			return true, nil
		}
		return secretEnabledForPromotion(ctx, q, run.UserID, run.CodexSecretID)
	}
	if run.Kind == runkind.Chat {
		// Chat is not bindable and takes no override: it always spends the owner default.
		return defaultEnabledForPromotion(ctx, q, run.UserID, false)
	}
	if run.Kind == runkind.Judge || run.Kind == runkind.SelfImprove {
		b, err := q.LockUserJudgeBindingForShare(ctx, run.UserID)
		if err != nil {
			return false, fmt.Errorf("lock judge binding: %w", err)
		}
		switch {
		case b.JudgeAnthropicBindMode == BindModePinned && b.JudgeAnthropicSecretID.Valid:
			return secretEnabledForPromotion(ctx, q, run.UserID, b.JudgeAnthropicSecretID)
		case b.JudgeAnthropicBindMode == BindModeAuto:
			available, err := q.HasEnabledAnthropicForJudgeAuto(ctx, run.UserID)
			if err != nil || available {
				return available, err
			}
			return noAnthropicTokenAtAll(ctx, q, run.UserID)
		default:
			return defaultEnabledForPromotion(ctx, q, run.UserID, false)
		}
	}
	if run.CredentialOverrideMode.Valid {
		switch run.CredentialOverrideMode.String {
		case BindModePinned:
			if run.CredentialOverrideSecretID.Valid {
				return secretEnabledForPromotion(ctx, q, run.UserID, run.CredentialOverrideSecretID)
			}
			// A nulled pin inherits the worker binding (D1 of PRD #1247).
		case BindModeAuto:
			return true, nil // the next claim spends an enabled pooled token or holds in pool_wait
		case BindModeDefault:
			return defaultEnabledForPromotion(ctx, q, run.UserID, true)
		}
	}
	worker := run.WorkerID
	if !worker.Valid {
		return true, nil // no affinity: any of the owner's workers may claim it
	}
	b, err := q.LockWorkerBindingForShare(ctx, store.LockWorkerBindingForShareParams{
		ID: uuid.UUID(worker.Bytes), UserID: run.UserID, HeartbeatCutoff: heartbeatCutoff,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // the worker is gone: the run falls open to any worker
	}
	if err != nil {
		return false, fmt.Errorf("lock worker binding: %w", err)
	}
	if !b.HoldsAffinity {
		return true, nil // not the next claimant: the actual claimant's binding decides
	}
	switch {
	case b.AnthropicBindMode == BindModePinned && b.AnthropicSecretID.Valid:
		return secretEnabledForPromotion(ctx, q, run.UserID, b.AnthropicSecretID)
	case b.AnthropicBindMode == BindModeAuto:
		return true, nil // the next claim spends an enabled pooled token or holds in pool_wait
	default:
		return defaultEnabledForPromotion(ctx, q, run.UserID, true)
	}
}

func secretEnabledForPromotion(ctx context.Context, q *store.Queries, userID uuid.UUID, id pgtype.UUID) (bool, error) {
	enabled, err := q.LockSecretForPromotion(ctx, store.LockSecretForPromotionParams{ID: uuid.UUID(id.Bytes), UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // deleted: the next claim reports the missing credential itself
	}
	if err != nil {
		return false, fmt.Errorf("lock requirement credential: %w", err)
	}
	return enabled, nil
}

// defaultEnabledForPromotion reports whether a default requirement is met. emptySlotMet is the
// answer when the slot has no default at all: true for an ordinary run (its next claim fails
// as a token-less user's always has, D15), false for a Judge/self-improve run (it waits for a
// default, D2).
func defaultEnabledForPromotion(ctx context.Context, q *store.Queries, userID uuid.UUID, emptySlotMet bool) (bool, error) {
	row, err := q.LockDefaultAnthropicSecretForShare(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		if emptySlotMet {
			return true, nil
		}
		return noAnthropicTokenAtAll(ctx, q, userID)
	}
	if err != nil {
		return false, fmt.Errorf("lock default credential: %w", err)
	}
	return row.Enabled, nil
}

// noAnthropicTokenAtAll reports whether the owner holds no Anthropic token, disabled or not:
// a default-only lane held with nothing left to enable is released to its next claim, which
// fails as a token-less owner's run does. Read under the user's secret mutation lock, which
// token create, delete and enablement take.
func noAnthropicTokenAtAll(ctx context.Context, q *store.Queries, userID uuid.UUID) (bool, error) {
	has, err := q.UserHasAnthropicToken(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("anthropic token presence: %w", err)
	}
	return !has, nil
}

// secretMutationQueries is the statement surface of the requirement writers that share the
// promoter's lock order: the worker and Judge rebinds and the per-run reassignment of a held
// run. Both the Store and a transaction-bound *store.Queries satisfy it.
type secretMutationQueries interface {
	GetUserSecretCiphertextByID(ctx context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error)
	SetWorkerAnthropicSecret(ctx context.Context, arg store.SetWorkerAnthropicSecretParams) (store.Worker, error)
	SetUserJudgeAnthropicBinding(ctx context.Context, arg store.SetUserJudgeAnthropicBindingParams) (store.User, error)
	ReassignCredentialDisabledRun(ctx context.Context, arg store.ReassignCredentialDisabledRunParams) (store.ReassignCredentialDisabledRunRow, error)
}

// withSecretMutation runs fn in one transaction whose first statement is the user's secret
// mutation lock (lock-order step 1), so a requirement write and a promotion decision for the
// same owner serialize. With no transaction beginner (fake-store unit tests) it runs fn on the
// Store directly: there is then no promoter either, so there is nothing to serialize against.
func (s *Service) withSecretMutation(ctx context.Context, userID uuid.UUID, fn func(q secretMutationQueries) error) error {
	if s.txBeginner == nil {
		return fn(s.q)
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockSecretMutation(ctx, tx, userID); err != nil {
		return err
	}
	if err := fn(store.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
