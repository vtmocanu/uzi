package store

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// freshResumeSQLState is the SQLSTATE a0_guard_fresh_resume_trg (migration 00523) raises for both
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
