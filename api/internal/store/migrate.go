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

// SecretMutationLockClass is the class half of the advisory lock that serializes
// one user's anthropic-token mutations — create, set-default, delete (PRD #104 M2,
// D12). Taken as pg_advisory_xact_lock(SecretMutationLockClass, objid(user_id)) as
// the first statement of every mutating transaction.
//
// It exists because the "exactly one default while any token exists" invariant is
// not schema-enforceable and three races break it under READ COMMITTED (D12): two
// concurrent first-token creates both claiming default; a delete-default that
// passes its "no other tokens" check while a second token is being inserted
// (leaving tokens with no default); and set-default, a two-statement clear-then-set
// swap. The PRD suggests `SELECT ... FOR UPDATE`, but that is INSUFFICIENT for the
// create-vs-delete race: FOR UPDATE locks existing rows, so it cannot block a
// concurrent INSERT of a NEW row, and it locks nothing at all when the set is empty
// (the first-token case). An advisory lock is a mutex over the whole (user, kind)
// key, so it serializes create against delete against set-default regardless of
// which rows exist — the same reasoning HostedProvisionLockClass records for the
// quota count.
//
// Disjoint from the other two lock spaces: the two-int space differs from
// RegistrationLockKey's one-bigint space, and this class differs from
// HostedProvisionLockClass, so none can collide. XACT-scoped: released on commit or
// rollback, no unlock to forget.
const SecretMutationLockClass int32 = 0x757A736B // "uzsk"

// SettingsMutationLockKey is the fixed key passed to pg_advisory_xact_lock as the
// first statement of every app_settings write transaction (issue #831). It
// serializes all settings PUTs globally so the cross-key label invariant
// (uzi_label != autopilot_label != finding_label, ValidateMerged) can never be
// broken by two concurrent PUTs.
//
// It exists because the transaction's own guard — ListAppSettingsForUpdate, a
// `SELECT ... FOR UPDATE` — is INSUFFICIENT here for the SAME reason spelled out on
// SecretMutationLockClass above: FOR UPDATE locks only rows that already exist, so
// when a label key has no stored row yet (its value is the compiled-in default),
// the loser of a race cannot see the winner's newly-INSERTED row under READ
// COMMITTED and both PUTs pass the check. That was the live bug: a fresh DB has no
// uzi_label row (00036 seeded the now-retired prd_label; PRD #764 renamed the key
// without reseeding), so two concurrent PUTs could commit uzi_label == SHARED and
// autopilot_label == SHARED. An advisory lock is a mutex regardless of which rows
// exist, so the second transaction runs its check only after the first commits.
//
// Single-bigint space, like RegistrationLockKey (settings are global, not
// per-user, so no objid). Its value differs from RegistrationLockKey's, and the
// one-bigint space is disjoint from the two-int space the *LockClass constants use,
// so none can collide. XACT-scoped: released on commit or rollback, no unlock to
// forget.
//
// Depends on the pool's default READ COMMITTED isolation (OpenPool sets none): the
// loser acquires the lock only after the winner commits, and its NEXT statement then
// takes a fresh snapshot that sees the winner's committed row. At REPEATABLE READ or
// SERIALIZABLE the transaction snapshot would instead be fixed at this lock statement
// — taken while the loser is still blocked, before the winner commits — so the loser
// would miss the new row and the race would reopen. The sibling locks above make the
// same assumption; do not add an isolation level to the DSN without revisiting this.
const SettingsMutationLockKey int64 = 0x757A7365 // "uzse"

// Migrate runs all pending goose migrations against the database at dsn. It
// retries the initial connection so the API can start slightly ahead of
// Postgres becoming ready.
func Migrate(ctx context.Context, dsn string) error {
	db, err := openForMigrate(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// MigrateTo runs the embedded goose migrations up to and including version,
// where version is the numeric migration filename prefix with leading zeros
// stripped (e.g. 00093 -> 93). Like Migrate, it retries the initial connection.
//
// It exists so a test can stand a database at a chosen version and observe what
// a data migration's one-shot backfill actually did — a blind spot every data
// migration shares (issue #187).
func MigrateTo(ctx context.Context, dsn string, version int64) error {
	db, err := openForMigrate(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := goose.UpToContext(ctx, db, "migrations", version); err != nil {
		return fmt.Errorf("run migrations to %d: %w", version, err)
	}
	return nil
}

// openForMigrate opens the sql.DB at dsn, waits for it to become reachable, and
// prepares goose (base FS + dialect). The caller owns the returned db and must
// Close it.
func openForMigrate(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sql db: %w", err)
	}

	if err := waitForDB(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("postgres"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set goose dialect: %w", err)
	}
	return db, nil
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
