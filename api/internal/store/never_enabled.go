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

// RefuseNeverEnabled returns ErrRepoNeverEnabled when the repo row's
// awaiting_first_enable marker is set, else nil.
func RefuseNeverEnabled(awaitingFirstEnable bool) error {
	if awaitingFirstEnable {
		return ErrRepoNeverEnabled
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
