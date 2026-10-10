package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseSize is the admin-health db.size probe result: the current database's total
// size and, when it could be read, its largest user relations.
type DatabaseSize struct {
	// SizeBytes is pg_database_size(current_database()).
	SizeBytes int64
	// Largest holds at most three tables or materialized views, biggest first, sized with
	// pg_total_relation_size (heap, indexes and TOAST). Empty when RelationsUnavailable.
	Largest []RelationSize
	// RelationsUnavailable is true when the largest-relations query was attempted but gave
	// up (a table lock held it past relationsLockTimeout, or it exceeded relationsTimeout).
	// SizeBytes is still valid.
	RelationsUnavailable bool
}

// RelationSize is one relation's name (schema-qualified when outside search_path) and total size.
type RelationSize struct {
	Name      string
	SizeBytes int64
}

const (
	// relationsLockTimeout bounds how long the evidence-only relations query waits for a
	// relation lock; pg_total_relation_size queues behind any ACCESS EXCLUSIVE holder
	// (VACUUM FULL, TRUNCATE, a migration ALTER).
	relationsLockTimeout = "500ms"
	// relationsTimeout bounds the whole relations query, independent of the caller's deadline.
	relationsTimeout = 2 * time.Second
	// lockNotAvailable is SQLSTATE 55P03, raised when lock_timeout expires.
	lockNotAvailable = "55P03"
)

// DatabaseSizeOnly reads only pg_database_size, which takes no relation locks. Use it
// when the largest relations are not needed.
func DatabaseSizeOnly(ctx context.Context, pool *pgxpool.Pool) (DatabaseSize, error) {
	var out DatabaseSize
	if err := pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&out.SizeBytes); err != nil {
		return DatabaseSize{}, fmt.Errorf("read database size: %w", err)
	}
	return out, nil
}

// DatabaseSizeStatus reads the database size and its three largest relations through the
// caller's pool, like SchemaVersionStatus. The size read failing is an error. The
// relations are evidence only: when their query cannot get its locks within
// relationsLockTimeout, or runs past relationsTimeout, the size is returned with
// Largest empty and RelationsUnavailable set, and no error. A relation dropped between
// the catalog scan and the size call yields a NULL size and is skipped.
func DatabaseSizeStatus(ctx context.Context, pool *pgxpool.Pool) (DatabaseSize, error) {
	out, err := DatabaseSizeOnly(ctx, pool)
	if err != nil {
		return DatabaseSize{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, relationsTimeout)
	defer cancel()
	largest, err := largestRelations(rctx, pool)
	switch {
	case err == nil:
		out.Largest = largest
	case isRelationsGiveUp(err) || (rctx.Err() != nil && ctx.Err() == nil):
		out.RelationsUnavailable = true
	default:
		return DatabaseSize{}, err
	}
	return out, nil
}

func isRelationsGiveUp(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

func largestRelations(ctx context.Context, pool *pgxpool.Pool) ([]RelationSize, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin largest relations: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+relationsLockTimeout+`'`); err != nil {
		return nil, fmt.Errorf("set lock_timeout: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT c.oid::regclass::text, pg_total_relation_size(c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m')
  AND n.nspname NOT IN ('pg_catalog','information_schema')
  AND n.nspname NOT LIKE 'pg_toast%'
  AND n.nspname NOT LIKE 'pg_temp%'
ORDER BY 2 DESC NULLS LAST
LIMIT 3`)
	if err != nil {
		return nil, fmt.Errorf("read largest relations: %w", err)
	}
	defer rows.Close()
	var out []RelationSize
	for rows.Next() {
		var name string
		var size sql.NullInt64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, fmt.Errorf("scan largest relation: %w", err)
		}
		if !size.Valid {
			continue
		}
		out = append(out, RelationSize{Name: name, SizeBytes: size.Int64})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read largest relations: %w", err)
	}
	return out, nil
}
