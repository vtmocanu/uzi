package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseSize is the admin-health db.size probe result: the current database's total
// size and its largest user relations.
type DatabaseSize struct {
	// SizeBytes is pg_database_size(current_database()).
	SizeBytes int64
	// Largest holds at most three tables or materialized views, biggest first, sized with
	// pg_total_relation_size (heap, indexes and TOAST).
	Largest []RelationSize
}

// RelationSize is one relation's name (schema-qualified when outside search_path) and total size.
type RelationSize struct {
	Name      string
	SizeBytes int64
}

// DatabaseSizeStatus reads the database size and its three largest relations through the
// caller's pool, like SchemaVersionStatus. A relation dropped between the catalog scan and
// the size call yields a NULL size and is skipped.
func DatabaseSizeStatus(ctx context.Context, pool *pgxpool.Pool) (DatabaseSize, error) {
	var out DatabaseSize
	if err := pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&out.SizeBytes); err != nil {
		return DatabaseSize{}, fmt.Errorf("read database size: %w", err)
	}
	rows, err := pool.Query(ctx, `SELECT c.oid::regclass::text, pg_total_relation_size(c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','m')
  AND n.nspname NOT IN ('pg_catalog','information_schema')
  AND n.nspname NOT LIKE 'pg_toast%'
  AND n.nspname NOT LIKE 'pg_temp%'
ORDER BY 2 DESC NULLS LAST
LIMIT 3`)
	if err != nil {
		return DatabaseSize{}, fmt.Errorf("read largest relations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var size sql.NullInt64
		if err := rows.Scan(&name, &size); err != nil {
			return DatabaseSize{}, fmt.Errorf("scan largest relation: %w", err)
		}
		if !size.Valid {
			continue
		}
		out.Largest = append(out.Largest, RelationSize{Name: name, SizeBytes: size.Int64})
	}
	if err := rows.Err(); err != nil {
		return DatabaseSize{}, fmt.Errorf("read largest relations: %w", err)
	}
	return out, nil
}
