package store

// Hand-written statements for admin add/delete of hosted worker sizes (PRD #4213 M3). They are
// plain SQL rather than sqlc queries because the correctness of each one is its exact
// statement shape (a lock taken by its own statement, a count read by its own statement):
// folding them into a single guarded statement would reintroduce a read-then-write race that
// the lock order below exists to close. Every function takes the caller's DBTX, which for all
// of the locking ones MUST be a pgx.Tx the caller commits or rolls back.
//
// Lock order (no cycle):
//
//	size delete:           WorkerSizesLockClass advisory -> worker_sizes row FOR UPDATE
//	settings write:        SettingsMutationLockKey advisory -> app_settings rows FOR UPDATE
//	                       -> worker_sizes rows FOR SHARE
//	persistent / ephemeral provision: per-user hosted-provision advisory -> worker_sizes row FOR SHARE
//
// A delete never takes the settings advisory lock and never locks app_settings rows (its
// settings read is a plain read), so it cannot wait on a settings writer that waits on it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Worker-size step stages reported to the test hook (SetWorkerSizeStepHookForTest).
const (
	// WorkerSizeStepShared fires right after a FOR SHARE statement on worker_sizes succeeded.
	WorkerSizeStepShared = "shared"
	// WorkerSizeStepDeleteLocked fires right after the delete's FOR UPDATE row lock succeeded.
	WorkerSizeStepDeleteLocked = "delete_locked"
)

type workerSizeStepFunc func(stage string, names []string, db DBTX)

// workerSizeStepHook is nil in production; only SetWorkerSizeStepHookForTest (a
// non-_test file compiled into the package so other packages' tests can use it) sets it.
var workerSizeStepHook atomic.Pointer[workerSizeStepFunc]

func workerSizeStep(stage string, db DBTX, names ...string) {
	if f := workerSizeStepHook.Load(); f != nil {
		(*f)(stage, names, db)
	}
}

// LockWorkerSizeShared takes FOR SHARE on the named size row inside the caller's
// transaction, so a concurrent delete cannot commit until this transaction ends. It reports
// false when no such row exists (never created, or already deleted).
func LockWorkerSizeShared(ctx context.Context, db DBTX, name string) (bool, error) {
	var one int32
	err := db.QueryRow(ctx, `SELECT 1 FROM worker_sizes WHERE name = $1 FOR SHARE`, name).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock worker size %q: %w", name, err)
	}
	workerSizeStep(WorkerSizeStepShared, db, name)
	return true, nil
}

// LockWorkerSizesShared takes FOR SHARE on every named row that exists, in name order (a
// single statement, so the lock order is deterministic), and returns the names that exist.
func LockWorkerSizesShared(ctx context.Context, db DBTX, names []string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT name FROM worker_sizes WHERE name = ANY($1) ORDER BY name FOR SHARE`, names)
	if err != nil {
		return nil, fmt.Errorf("lock worker sizes: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("lock worker sizes: %w", err)
	}
	workerSizeStep(WorkerSizeStepShared, db, names...)
	return found, nil
}

// CountWorkerSizes is the table's row count, its own statement: the caller holds the
// WorkerSizesLockClass advisory lock, which is what makes the count safe to act on.
func CountWorkerSizes(ctx context.Context, db DBTX) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM worker_sizes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count worker sizes: %w", err)
	}
	return n, nil
}

// InsertAdminWorkerSize inserts a non-builtin, non-customized size after every existing one
// (display_order = max + 1). The caller holds the advisory lock and has checked the cap.
func InsertAdminWorkerSize(ctx context.Context, db DBTX, name string, cpuReq, cpuLim, memReq, memLim, data, dind int64, updatedBy pgtype.UUID) (WorkerSize, error) {
	var s WorkerSize
	err := db.QueryRow(ctx, `INSERT INTO worker_sizes (name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib,
		memory_limit_mib, data_gib, dind_data_gib, display_order, builtin, customized, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, (SELECT COALESCE(MAX(display_order), 0) + 1 FROM worker_sizes), false, false, $8)
		RETURNING name, cpu_request_millicores, cpu_limit_millicores, memory_request_mib, memory_limit_mib,
		data_gib, dind_data_gib, display_order, builtin, customized, updated_at, updated_by`,
		name, cpuReq, cpuLim, memReq, memLim, data, dind, updatedBy).Scan(
		&s.Name, &s.CpuRequestMillicores, &s.CpuLimitMillicores, &s.MemoryRequestMib, &s.MemoryLimitMib,
		&s.DataGib, &s.DindDataGib, &s.DisplayOrder, &s.Builtin, &s.Customized, &s.UpdatedAt, &s.UpdatedBy)
	if err != nil {
		return WorkerSize{}, fmt.Errorf("insert worker size %q: %w", name, err)
	}
	return s, nil
}

// Delete refusal reasons.
const (
	WorkerSizeRefusedBuiltin = "builtin"
	WorkerSizeRefusedInUse   = "in_use"
)

// ErrWorkerSizeNotFound is returned by DeleteWorkerSizeGuarded for an absent size.
var ErrWorkerSizeNotFound = errors.New("worker size not found")

// WorkerSizeRefusal is a refused delete: a builtin size, or a size something still uses.
type WorkerSizeRefusal struct {
	Reason   string
	Workers  int64    // hosted workers whose hosted_size is the name
	Settings []string // stored settings keys whose (trimmed) value is the name
}

func (e *WorkerSizeRefusal) Error() string {
	if e.Reason == WorkerSizeRefusedBuiltin {
		return "builtin sizes cannot be removed; reset it instead"
	}
	var parts []string
	if e.Workers > 0 {
		parts = append(parts, fmt.Sprintf("%d worker(s) still use it", e.Workers))
	}
	if len(e.Settings) > 0 {
		parts = append(parts, "it is the stored value of "+strings.Join(e.Settings, ", "))
	}
	return "size is in use: " + strings.Join(parts, "; ")
}

// sizeSettingKeys are the app_settings keys whose value names a worker size. Spelled here (not
// imported from settings) because settings imports store.
var sizeSettingKeys = []string{"ephemeral_default_size", "hosted_default_size"}

// DeleteWorkerSizeGuarded deletes a non-builtin, unreferenced size inside the caller's
// transaction, which must already hold the WorkerSizesLockClass advisory lock. The guard is
// four SEPARATE statements in this exact order, and must not be folded into one:
//
//  1. SELECT builtin ... FOR UPDATE: takes the row lock that every referencing writer's
//     FOR SHARE conflicts with, so from here no writer can commit a new reference; a writer
//     that already holds FOR SHARE makes this statement wait until it commits.
//  2. SELECT EXISTS (workers): a NEW statement, so under READ COMMITTED it sees any worker a
//     writer committed while (1) waited.
//  3. a PLAIN read of the stored size settings (never FOR UPDATE: a settings writer holds
//     app_settings row locks and then waits for our row lock, so locking them here would
//     deadlock). It likewise sees what a settings writer committed while (1) waited.
//  4. DELETE.
//
// The env overlay of ephemeral_default_size can only name a builtin, and a builtin is refused
// at (1), so the stored rows are the complete reference set.
func DeleteWorkerSizeGuarded(ctx context.Context, db DBTX, name string) error {
	var isBuiltin bool
	err := db.QueryRow(ctx, `SELECT builtin FROM worker_sizes WHERE name = $1 FOR UPDATE`, name).Scan(&isBuiltin)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkerSizeNotFound
	}
	if err != nil {
		return fmt.Errorf("lock worker size %q: %w", name, err)
	}
	workerSizeStep(WorkerSizeStepDeleteLocked, db, name)
	if isBuiltin {
		return &WorkerSizeRefusal{Reason: WorkerSizeRefusedBuiltin}
	}

	var used bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workers WHERE hosted_size = $1)`, name).Scan(&used); err != nil {
		return fmt.Errorf("check workers using size %q: %w", name, err)
	}
	refusal := &WorkerSizeRefusal{Reason: WorkerSizeRefusedInUse}
	if used {
		// Informational count for the message, a separate statement; the EXISTS above is the guard.
		if err := db.QueryRow(ctx, `SELECT count(*) FROM workers WHERE hosted_size = $1`, name).Scan(&refusal.Workers); err != nil {
			return fmt.Errorf("count workers using size %q: %w", name, err)
		}
		if refusal.Workers == 0 {
			refusal.Workers = 1 // a worker vanished between the two reads; it still held the size at the guard
		}
	}

	rows, err := db.Query(ctx, `SELECT key, value FROM app_settings WHERE key = ANY($1) ORDER BY key`, sizeSettingKeys)
	if err != nil {
		return fmt.Errorf("read size settings: %w", err)
	}
	type kv struct{ Key, Value string }
	stored, err := pgx.CollectRows(rows, pgx.RowToStructByPos[kv])
	if err != nil {
		return fmt.Errorf("read size settings: %w", err)
	}
	for _, s := range stored {
		if strings.TrimSpace(s.Value) == name {
			refusal.Settings = append(refusal.Settings, s.Key)
		}
	}
	if refusal.Workers > 0 || len(refusal.Settings) > 0 {
		return refusal
	}

	if _, err := db.Exec(ctx, `DELETE FROM worker_sizes WHERE name = $1`, name); err != nil {
		return fmt.Errorf("delete worker size %q: %w", name, err)
	}
	return nil
}
