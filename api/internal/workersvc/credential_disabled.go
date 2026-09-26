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
// Enable/disable, default changes, worker and Judge rebinding and per-run reassignment all take
// (1) first, so each promotion decision is serialized against them; the promotion UPDATE is
// additionally guarded on the requirement columns it evaluated. The claim finisher never takes
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
	// requirement evaluated as met, before the guarded promote UPDATE.
	beforePromote func(ctx context.Context, runID uuid.UUID)
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
		for {
			ctx, cancel := context.WithTimeout(context.Background(), credentialPromotePassTimeout)
			if _, err := s.promoteCredentialDisabledRuns(ctx, userID); err != nil {
				slog.Error("credential promoter: requested pass", "user", userID, "error", err)
			}
			cancel()
			p.mu.Lock()
			if p.dirty[userID] {
				delete(p.dirty, userID)
				p.mu.Unlock()
				continue
			}
			delete(p.running, userID)
			p.mu.Unlock()
			return
		}
	})
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
// budget) is skipped by the keyset cursor, so one blocked run never starves the rest.
func (s *Service) promoteCredentialDisabledRuns(ctx context.Context, userID uuid.UUID) (int64, error) {
	if s.txBeginner == nil {
		return 0, nil
	}
	var promoted int64
	params := store.ListCredentialDisabledRunsParams{UserID: userID, PageSize: credentialPromotePageSize}
	for {
		page, err := s.q.ListCredentialDisabledRuns(ctx, params)
		if err != nil {
			return promoted, fmt.Errorf("list credential-disabled runs: %w", err)
		}
		for _, r := range page {
			ok, err := s.promoteCredentialDisabledRun(ctx, userID, r.ID)
			if err != nil {
				return promoted, fmt.Errorf("promote credential-disabled run %s: %w", r.ID, err)
			}
			if ok {
				promoted++
				s.publishSwept(r.ID, "queued")
			}
		}
		if len(page) < credentialPromotePageSize {
			return promoted, nil
		}
		last := page[len(page)-1]
		params.AfterStatusSince, params.AfterID = last.StatusSince, pgtype.UUID{Bytes: last.ID, Valid: true}
	}
}

// promoteCredentialDisabledRun is one candidate's transaction, in the fixed lock order above.
// It reports whether the run was returned to queued.
func (s *Service) promoteCredentialDisabledRun(ctx context.Context, userID, runID uuid.UUID) (bool, error) {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// (1) The user lock, before any run lock.
	if err := store.LockSecretMutation(ctx, tx, userID); err != nil {
		return false, err
	}
	q := store.New(tx)
	// (2) The run row, re-read under its lock.
	run, err := q.LockCredentialDisabledRunForPromotion(ctx, store.LockCredentialDisabledRunForPromotionParams{ID: runID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if run.Status != "paused" || !run.HoldReason.Valid || run.HoldReason.String != "credential_disabled" ||
		run.PauseRequestedAt.Valid {
		return false, nil // no longer this hold, or an owner pause keeps it paused
	}
	// (3) + (4) The requirement source and the requirement credential.
	met, err := credentialRequirementMet(ctx, q, run)
	if err != nil || !met {
		return false, err
	}
	if h := s.credPromoteHooks; h != nil && h.beforePromote != nil {
		h.beforePromote(ctx, runID)
	}
	_, err = q.PromoteCredentialDisabledRun(ctx, store.PromoteCredentialDisabledRunParams{
		ID: runID, UserID: userID,
		ExpectedOverrideMode:     run.CredentialOverrideMode,
		ExpectedOverrideSecretID: run.CredentialOverrideSecretID,
		ExpectedCodexSecretID:    run.CodexSecretID,
		ExpectedWorkerID:         run.WorkerID,
		ExpectedReleasedWorkerID: run.CredentialDisableReleasedWorkerID,
		GlobalTimeoutSeconds:     int32(s.p.RunTimeout.Seconds()),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // spent budget, or a guard saw a changed requirement
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// credentialRequirementMet re-evaluates the credential the run's NEXT claim will need, from
// the locked run row, and reports whether it is available. It mirrors the claim ladder
// (claimSecretID / judgeChoice / harness-authoritative Codex) rather than the previous claim:
//
//   - a Codex run needs its frozen alias;
//   - judge and self_improve follow the owner's Judge binding (a pin needs that credential, the
//     default mode needs an enabled default, auto needs an enabled pooled token or default);
//   - otherwise a per-run override decides (pinned id, default, or auto), and a nulled pin or
//     no override inherits the recorded worker's binding.
//
// A different default never satisfies an explicit pin. Lanes with no single credential (an
// ordinary auto lane) and a requirement that can no longer be resolved (no recorded worker, a
// deleted credential) are promoted, so the next claim decides with its own checks instead of
// the run waiting forever on something that no longer exists.
func credentialRequirementMet(ctx context.Context, q *store.Queries, run store.LockCredentialDisabledRunForPromotionRow) (bool, error) {
	if run.Harness == harnessCodex {
		if !run.CodexSecretID.Valid {
			return true, nil
		}
		return secretEnabledForPromotion(ctx, q, run.UserID, run.CodexSecretID)
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
			return q.HasEnabledAnthropicForJudgeAuto(ctx, run.UserID)
		default:
			return defaultEnabledForPromotion(ctx, q, run.UserID)
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
			return true, nil
		case BindModeDefault:
			return defaultEnabledForPromotion(ctx, q, run.UserID)
		}
	}
	worker := run.WorkerID
	if !worker.Valid {
		worker = run.CredentialDisableReleasedWorkerID
	}
	if !worker.Valid {
		return true, nil
	}
	b, err := q.LockWorkerBindingForShare(ctx, store.LockWorkerBindingForShareParams{ID: uuid.UUID(worker.Bytes), UserID: run.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock worker binding: %w", err)
	}
	switch {
	case b.AnthropicBindMode == BindModePinned && b.AnthropicSecretID.Valid:
		return secretEnabledForPromotion(ctx, q, run.UserID, b.AnthropicSecretID)
	case b.AnthropicBindMode == BindModeAuto:
		return true, nil
	default:
		return defaultEnabledForPromotion(ctx, q, run.UserID)
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

func defaultEnabledForPromotion(ctx context.Context, q *store.Queries, userID uuid.UUID) (bool, error) {
	row, err := q.LockDefaultAnthropicSecretForShare(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // the slot is empty (D4): wait for a default
	}
	if err != nil {
		return false, fmt.Errorf("lock default credential: %w", err)
	}
	return row.Enabled, nil
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
