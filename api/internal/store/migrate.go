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
