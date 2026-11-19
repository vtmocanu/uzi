package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRepoNeverEnabled is the typed refusal for an operation that would sync a
// repo's issues, start a run, or store an issue number on a repo that has never
// been enabled (repos.awaiting_first_enable, PRD 4173 D13). Callers check the
// repo row's AwaitingFirstEnable with RefuseNeverEnabled BEFORE any forge or
// issue tracker call, and map a UZ413 from the database through
// MapNeverEnabled; handlers answer both with 409.
var ErrRepoNeverEnabled = errors.New("the repo has never been enabled; enable it first")

// neverEnabledSQLState is the SQLSTATE the issue-number guard trigger
// (issue_number_awaiting_first_enable_guard) raises. Class UZ is not a
// PostgreSQL class.
const neverEnabledSQLState = "UZ413"

// ErrRepoSourceLocked is the typed refusal for an operation that would read, import or write
// a repo's issues, or start a run, on a re-listed project whose issue source was an issue
// tracker before the repo was removed (PRD 4173 D13: the issue source never changes). The row
// is disabled and unlinked, and the removal marker has cleared awaiting_first_enable, so the
// never-enabled marker alone does not stop it. It matches ErrRepoNeverEnabled under errors.Is,
// so every existing mapping of the never-enabled refusal answers it with the same 409; the
// message names the real reason.
var ErrRepoSourceLocked error = sourceLockedError{}

type sourceLockedError struct{}

func (sourceLockedError) Error() string {
	return "this project was linked to an issue tracker before it was removed from uzi; its issue source cannot change, so it cannot be used again"
}

func (sourceLockedError) Is(target error) bool { return target == ErrRepoNeverEnabled }

// RefuseNeverEnabled returns the typed refusal for a repo row that may not touch issues or
// start runs: ErrRepoNeverEnabled when its awaiting_first_enable marker is set,
// ErrRepoSourceLocked when it is a re-listed project that was linked to an issue tracker (the
// issue_source_locked column of GetRepoForUser and GetRepoByID), else nil. Callers pass both
// columns of the same row, so one seam covers both refusals.
func RefuseNeverEnabled(awaitingFirstEnable, issueSourceLocked bool) error {
	if awaitingFirstEnable {
		return ErrRepoNeverEnabled
	}
	if issueSourceLocked {
		return ErrRepoSourceLocked
	}
	return nil
}

// MapNeverEnabled returns ErrRepoNeverEnabled (wrapping err) when err carries the
// guard trigger's SQLSTATE, and err unchanged otherwise (nil stays nil).
func MapNeverEnabled(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == neverEnabledSQLState {
		return errors.Join(ErrRepoNeverEnabled, err)
	}
	return err
}
