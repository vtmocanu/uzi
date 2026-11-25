package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// freshResumeSQLState is the SQLSTATE a0_guard_fresh_resume_trg (migration 00535) raises for both
// of its rules. Class UZ is not a PostgreSQL class; UZ416 follows UZ413 to UZ415.
const freshResumeSQLState = "UZ416"

// freshResumeRequestKey is the UNIQUE (run_id, request_id) constraint of run_fresh_resumes.
const freshResumeRequestKey = "run_fresh_resumes_run_request_key"

const (
	freshResumeInProgressMessage          = "fresh resume transfer in progress"
	freshResumePredecessorExcludedMessage = "fresh resume predecessor excluded"
)

func freshResumeGuardError(err error, message string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == freshResumeSQLState && strings.Contains(pgErr.Message, message)
}

// FreshResumeOwnerLockClass is the class half of the two-int advisory lock that serializes every path
// consuming or checking one owner's fresh resume headroom (issue #4628): Start, and an owner Resume
// while the feature is on or the owner has an outstanding transfer. The objid is hashtext(owner::text),
// taken in SQL by the callers: pg_advisory_xact_lock(FreshResumeOwnerLockClass, hashtext($owner::text)).
// It is the FIRST lock of those transactions, before any worker, run, transfer or capture row lock. The
// fresh claim takes it with pg_try_advisory_xact_lock and skips the candidate when busy, because
// Service.Claim already holds the claimant row. XACT-scoped. An objid collision only serializes two
// unrelated owners for a moment. TestProductTokenMintLockClassMatchesSQL's source enumeration fails on
// any collision with another *LockClass.
const FreshResumeOwnerLockClass int32 = 0x757A6672 // "uzfr"

// freshResumeLockSQL is the owner lock statement, one text so every taker agrees on the objid.
const freshResumeLockSQL = "SELECT pg_advisory_xact_lock($1, hashtext($2::text))"

// freshResumeTryLockSQL is the fresh claim's non-waiting form of freshResumeLockSQL: the same class and
// the same hashtext(owner) objid, so a holder of either blocks the other.
const freshResumeTryLockSQL = "SELECT pg_try_advisory_xact_lock($1, hashtext($2::text))"

// TryLockFreshResumeOwner is LockFreshResumeOwner without the wait: it reports whether the lock was
// taken. The fresh claim skips a candidate whose owner lock is busy.
func TryLockFreshResumeOwner(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ownerID uuid.UUID) (bool, error) {
	var got bool
	err := db.QueryRow(ctx, freshResumeTryLockSQL, FreshResumeOwnerLockClass, ownerID.String()).Scan(&got)
	return got, err
}

// LockFreshResumeOwner takes the owner's fresh resume advisory lock for the rest of the transaction.
func LockFreshResumeOwner(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, ownerID uuid.UUID) error {
	_, err := db.Exec(ctx, freshResumeLockSQL, FreshResumeOwnerLockClass, ownerID.String())
	return err
}

// IsFreshResumeInProgress reports whether err is rule 1 of the fresh resume guard: a runs status,
// worker_id or claim_generation write refused because a pending or claimed transfer row exists.
// Handlers answer it with 409 fresh_resume_in_progress.
func IsFreshResumeInProgress(err error) bool {
	return freshResumeGuardError(err, freshResumeInProgressMessage)
}

// IsFreshResumePredecessorExcluded reports whether err is rule 2 of the guard: a worker_id write
// naming the predecessor, or a holder of an older custody hold, on a run that has been seeded.
func IsFreshResumePredecessorExcluded(err error) bool {
	return freshResumeGuardError(err, freshResumePredecessorExcludedMessage)
}

// IsFreshResumeRequestConflict reports whether err is a unique violation (23505) of the
// (run_id, request_id) constraint only, matched by constraint name. The per-run partial unique
// indexes (one pending or claimed row, one seeded row) are never a request conflict.
func IsFreshResumeRequestConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == freshResumeRequestKey
}
