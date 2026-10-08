package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Direct SQL calls isolate each restriction from the service admission guard.
// Rollback preserves the parent fixture's open custody for its remaining checks.
func testPrerequisiteSQLGuards(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cap store.RecoveryCapture, hold, worker, user, run uuid.UUID) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	digest := strings.Repeat("a", 64)
	exec("UPDATE recovery_captures SET manifest_bound=true,prerequisite_shas=ARRAY['base1'] WHERE id=$1", cap.ID)
	protect := store.ProtectFinalInventoryCaptureParams{ID: cap.ID, HoldID: hold, WorkerID: worker, SourceSha: cap.SourceSha, CoverageDigest: pgtype.Text{String: digest, Valid: true}, RetentionSeconds: 3600}
	if n, err := q.ProtectFinalInventoryCapture(ctx, protect); err != nil || n != 0 {
		t.Fatalf("SQL thin protection: %d %v", n, err)
	}
	var intact bool
	if err := tx.QueryRow(ctx, "SELECT manifest_bound AND prerequisite_shas=ARRAY['base1'] AND coverage_digest=$2 AND local_replica_worker_id IS NULL AND ready_retention_seconds IS NULL FROM recovery_captures WHERE id=$1", cap.ID, digest).Scan(&intact); err != nil || !intact {
		t.Fatalf("SQL protection rejection mutated markers: %v %v", intact, err)
	}
	// Stamp protection deliberately so the release query must enforce its own guard.
	exec("UPDATE recovery_captures SET local_replica_worker_id=$2,ready_retention_seconds=3600 WHERE id=$1", cap.ID, worker)
	release := store.ReleaseFinalInventoryHoldParams{ID: hold, RunID: run, UserID: user, WorkerID: worker, Generation: 1, FinalDisposition: "archive", FinalCaptureID: pgtype.UUID{Bytes: cap.ID, Valid: true}, FinalSourceSha: pgtype.Text{String: cap.SourceSha, Valid: true}, FinalCoverageDigest: digest, ReleaseEvidence: "archive"}
	if n, err := q.ReleaseFinalInventoryHold(ctx, release); err != nil || n != 0 {
		t.Fatalf("SQL thin release: %d %v", n, err)
	}
	if err := tx.QueryRow(ctx, "SELECT state='open' AND live_worker_id=$2 AND live_run_id=$3 FROM recovery_custody_holds WHERE id=$1", hold, worker, run).Scan(&intact); err != nil || !intact {
		t.Fatalf("SQL release rejection mutated custody: %v %v", intact, err)
	}
	// Positive controls prove every other predicate is satisfied for nil and empty.
	for _, prerequisites := range [][]string{nil, {}} {
		exec("SAVEPOINT empty_prerequisites")
		exec("UPDATE recovery_captures SET prerequisite_shas=$2 WHERE id=$1", cap.ID, prerequisites)
		if n, err := q.ProtectFinalInventoryCapture(ctx, protect); err != nil || n != 1 {
			t.Fatalf("SQL empty protection: %d %v", n, err)
		}
		if n, err := q.ReleaseFinalInventoryHold(ctx, release); err != nil || n != 1 {
			t.Fatalf("SQL empty release: %d %v", n, err)
		}
		exec("ROLLBACK TO SAVEPOINT empty_prerequisites")
		exec("RELEASE SAVEPOINT empty_prerequisites")
	}
}
