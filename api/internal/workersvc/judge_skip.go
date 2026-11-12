package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Visible judge skip (issue #3257). When the automatic judge is not created because the
// judge's harness has no usable credential, the refusal is recorded in judge_skips so the
// run page and the CLI can say why and offer a retry. Creating a judge for the target (the
// automatic path or a manual rerun) clears the record in the same transaction.

// The reason vocabulary of judge_skips.reason. JudgeSkipReasons is the ONE list; the table's
// CHECK constraint repeats it and TestJudgeSkipReasonsMatchCheckLiveDB pins the two together.
const (
	JudgeSkipNoUsableCredential   = "no_usable_credential" //nolint:gosec // G101: fixed skip reason code, not a credential
	JudgeSkipCredentialDisabled   = "credential_disabled"  //nolint:gosec // G101: fixed skip reason code, not a credential
	JudgeSkipCodexPoolUnavailable = "codex_pool_unavailable"
)

// JudgeSkipReasons lists every reason the judge_skips CHECK accepts.
var JudgeSkipReasons = []string{
	JudgeSkipNoUsableCredential,
	JudgeSkipCredentialDisabled,
	JudgeSkipCodexPoolUnavailable,
}

// JudgeSkip is the recorded reason the automatic judge of a run was skipped.
type JudgeSkip struct {
	Harness string
	Reason  string
	At      time.Time
}

// errJudgeCreateIsolation fails a judge creation closed when its transaction is not READ
// COMMITTED: the skip record's lock protocol (see lockJudgeTargetForCreate) is only sound
// there. The transaction rolls back, so no judge is created and nothing is recorded.
var errJudgeCreateIsolation = errors.New("judge creation requires a READ COMMITTED transaction")

// errJudgeCreateRequiresTx fails a judge creation closed when the Service runs on a real
// *store.Queries with no transaction beginner. createRunAtomic then takes its no-transaction
// branch, so lockJudgeTargetForCreate would autocommit: the target lock would fence nothing
// and the skip delete would not roll back with the judge insert. Nothing is created, deleted
// or recorded. Production always wires SetTxBeginner, so this is a misconfiguration guard.
var errJudgeCreateRequiresTx = errors.New("judge creation requires a transaction beginner")

// judgeCreateLacksTx reports the hazardous configuration: a live store without a beginner.
// The in-memory unit fakeStore is not a *store.Queries and runs its insert directly (see
// createRunResolved), where no real lock or skip row exists, so it is exempt.
func (s *Service) judgeCreateLacksTx() bool {
	_, live := s.q.(*store.Queries)
	return live && s.txBeginner == nil
}

// judgeSkipReasonFor maps a credential refusal from createRunResolved to its skip reason.
// The precedence matters because the typed refusals wrap one another: a disabled credential
// and an unavailable pool both wrap ErrNoCredentialForHarness. Any other error (including a
// 23505 duplicate judge) maps to no reason, so nothing is recorded for it.
func judgeSkipReasonFor(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrHarnessCredentialDisabled):
		return JudgeSkipCredentialDisabled, true
	case errors.Is(err, ErrCodexPoolUnavailable):
		return JudgeSkipCodexPoolUnavailable, true
	case errors.Is(err, ErrNoCredentialForHarness):
		return JudgeSkipNoUsableCredential, true
	default:
		return "", false
	}
}

// lockJudgeTargetForCreate is the first part of every judge insert closure, run on the
// creating transaction q before CreateJudgeRun: assert READ COMMITTED, take the target's
// advisory lock, and delete the target's skip record.
//
// The lock is a transaction-scoped advisory lock keyed on the target (store.JudgeTargetLockClass),
// not a row lock, so it never conflicts with an FK cascade or any other writer of the run row.
// The lock and the record/delete are separate statements on purpose. The skip writer
// (recordJudgeSkip) takes the same lock before its NOT EXISTS check, so the two serialize on
// the target key; under READ COMMITTED each later statement takes a fresh snapshot, so the
// writer's check sees a judge committed before the lock was granted, and this closure's
// delete sees a skip committed before the lock was granted. The existence read inside the lock
// statement uses its pre-wait snapshot, which is fine: the reads that must be fresh are those
// LATER statements, and they must never be folded into the lock statement. No fencing argument
// relies on lock-before-read order inside the lock statement.
func (s *Service) lockJudgeTargetForCreate(ctx context.Context, q Store, targetRunID uuid.UUID) error {
	level, err := q.CurrentTransactionIsolation(ctx)
	if err != nil {
		return err
	}
	if level != "read committed" {
		return errJudgeCreateIsolation
	}
	if s.judgeBeforeLockHook != nil {
		s.judgeBeforeLockHook(ctx)
	}
	if _, err := q.LockRunForJudge(ctx, targetRunID); err != nil {
		return err
	}
	return q.DeleteJudgeSkipForTarget(ctx, targetRunID)
}

// txOptionsBeginner is implemented by *pgxpool.Pool. The skip writer uses it to ask for READ
// COMMITTED explicitly; a plain TxBeginner (a test double) falls back to Begin and the
// isolation assertion below.
type txOptionsBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// recordJudgeSkip writes the skip record for an automatic judge that was refused for want of
// a usable credential. BEST-EFFORT: every failure is logged and swallowed, and with no
// transaction beginner it does nothing. It runs in its own READ COMMITTED transaction:
// lock the target (no row means the run is gone), then insert unless any judge run of the
// target exists (RecordJudgeSkip's NOT EXISTS).
func (s *Service) recordJudgeSkip(ctx context.Context, targetRunID uuid.UUID, harness Harness, reason string) {
	if s.txBeginner == nil {
		return
	}
	var tx pgx.Tx
	var err error
	if b, ok := s.txBeginner.(txOptionsBeginner); ok {
		tx, err = b.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	} else {
		tx, err = s.txBeginner.Begin(ctx)
	}
	if err != nil {
		slog.Warn("judge skip: begin", "run", targetRunID, "error", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	level, err := qtx.CurrentTransactionIsolation(ctx)
	if err != nil || level != "read committed" {
		slog.Warn("judge skip: transaction is not READ COMMITTED, recording nothing", "run", targetRunID, "level", level, "error", err)
		return
	}
	if _, err := qtx.LockRunForJudge(ctx, targetRunID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("judge skip: lock target", "run", targetRunID, "error", err)
		}
		return // the target run is gone: nothing to explain
	}
	if _, err := qtx.RecordJudgeSkip(ctx, store.RecordJudgeSkipParams{
		TargetRunID: targetRunID,
		Harness:     string(harness),
		Reason:      reason,
	}); err != nil {
		slog.Warn("judge skip: record", "run", targetRunID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("judge skip: commit", "run", targetRunID, "error", err)
	}
}

// judgeSkipForTarget reads the recorded skip of a target, or (nil, nil) when there is none.
// Callers MUST have applied the GetRunForViewer visibility gate first.
func (s *Service) judgeSkipForTarget(ctx context.Context, targetRunID uuid.UUID) (*JudgeSkip, error) {
	row, err := s.q.GetJudgeSkipForTarget(ctx, targetRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &JudgeSkip{Harness: row.Harness, Reason: row.Reason, At: row.CreatedAt.Time}, nil
}
