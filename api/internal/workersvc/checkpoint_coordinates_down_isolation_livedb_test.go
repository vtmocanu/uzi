package workersvc

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCheckpointCoordinatesDownIsolationLiveDB(t *testing.T) {
	env := isolatedCoordinateDatabase(t)
	migration, err := os.ReadFile("../store/migrations/00352_task_checkpoint_coordinates.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(migration), "-- +goose Down\n")
	if !ok {
		t.Fatal("missing production Down script")
	}
	for _, isolation := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		t.Run(string(isolation), func(t *testing.T) {
			tx, err := env.pool.BeginTx(env.ctx, pgx.TxOptions{IsoLevel: isolation})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			// Establish the old snapshot before the script acquires its table lock.
			if _, err := tx.Exec(env.ctx, "SELECT count(*) FROM runs"); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(env.ctx, down)
			if err == nil || !strings.Contains(err.Error(), "checkpoint schema downgrade requires READ COMMITTED") {
				t.Fatalf("strict-snapshot Down did not refuse: %v", err)
			}
			if err := tx.Rollback(env.ctx); err != nil {
				t.Fatal(err)
			}
			var retained bool
			if err := env.pool.QueryRow(env.ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute
WHERE attrelid='runs'::regclass AND attname='checkpoint_coordinates' AND NOT attisdropped)
AND to_regprocedure('fn_checkpoint_coordinates(runs)') IS NOT NULL`).Scan(&retained); err != nil || !retained {
				t.Fatalf("refused Down changed protected schema: retained=%v err=%v", retained, err)
			}
		})
	}
}
