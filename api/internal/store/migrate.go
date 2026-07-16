package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// RegistrationLockKey is the fixed key passed to pg_advisory_xact_lock during
// registration. It serializes concurrent registrations so the
// first-user-becomes-admin check-and-insert is race-free on Postgres (whose
// READ COMMITTED isolation would otherwise let two concurrent first
// registrations both see zero users and both become admin).
const RegistrationLockKey int64 = 0x757A69 // "uzi"

// HostedProvisionLockClass is the class half of the two-int advisory lock that
// serializes one user's hosted-worker provisions (PRD #58 M2). Taken as
// pg_advisory_xact_lock(HostedProvisionLockClass, objid), where objid is derived
// from the user's uuid — so provisions serialize PER USER rather than globally,
// which is the only difference from RegistrationLockKey above.
//
// It exists for the same reason, stated there: under READ COMMITTED two concurrent
// transactions each count against their own snapshot, neither sees the other's
// uncommitted row, and both pass a quota check that was true when each looked. The
// lock is a mutex rather than a snapshot rule, so the second transaction's count
// runs only after the first commits and therefore sees its row.
//
// Three properties worth knowing before touching this:
//   - The TWO-int lock space is disjoint from the ONE-bigint space
//     RegistrationLockKey uses, so the two can never collide despite both being
//     "uz"-ish constants.
//   - A uuid's first four bytes are random, so two users can collide on objid. That
//     serializes two unrelated provisions for a moment: a contention non-event,
//     never a correctness one.
//   - It is an XACT lock: it releases on commit or rollback. There is no unlock
//     path to forget and no way to leak it by returning early.
const HostedProvisionLockClass int32 = 0x757A6877 // "uzhw"

// Migrate runs all pending goose migrations against the database at dsn. It
// retries the initial connection so the API can start slightly ahead of
// Postgres becoming ready.
func Migrate(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open sql db: %w", err)
	}
	defer db.Close()

	if err := waitForDB(ctx, db); err != nil {
		return err
	}

	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func waitForDB(ctx context.Context, db *sql.DB) error {
	const maxAttempts = 30
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		lastErr = db.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("database not reachable after %d attempts: %w", maxAttempts, lastErr)
}
