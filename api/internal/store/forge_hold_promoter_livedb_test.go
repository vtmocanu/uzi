package store_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The old API's early promoter bypassed a NULL forge stamp. Execute that exact
// prior SQL only inside a rolled-back transaction; production keeps its fence.
func TestForgeHoldLegacyPromoterResidualLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("forge-held", 0, false)
	id := fx.pinnedRun(w, time.Hour)
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='running',forge_park_count=6,claim_generation=1 WHERE id=$1`, id)
	if _, err := fx.q.HoldRunForgeUnreachable(fx.ctx, store.HoldRunForgeUnreachableParams{ID: id, WorkerID: pgtype.UUID{Bytes: w, Valid: true}, ClaimGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	if n, err := fx.q.PromoteRecoveryWaitRunNow(fx.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: fx.userID}); err != nil || n != 0 {
		t.Fatalf("current promoter=%d/%v", n, err)
	}
	if n, err := fx.q.PromoteCodexAccountWaitRun(fx.ctx, id); err != nil || n != 0 {
		t.Fatalf("account promoter=%d/%v", n, err)
	}
	raw, err := os.ReadFile("testdata/m3_legacy_promote_recovery_wait.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fx.ctx) }()
	tag, err := tx.Exec(fx.ctx, string(raw), id, fx.userID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("legacy residual rows=%d err=%v", tag.RowsAffected(), err)
	}
	var status string
	if err := tx.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id=$1`, id).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("legacy status=%s/%v", status, err)
	}
	if err := tx.Rollback(fx.ctx); err != nil {
		t.Fatal(err)
	}
	// A changed or cancelled hold cannot be released by the dedicated writer.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET stop_kind='cancelled' WHERE id=$1`, id)
	if _, err := fx.q.ResumeForgeUnreachableHold(fx.ctx, store.ResumeForgeUnreachableHoldParams{ID: id, UserID: fx.userID, GlobalTimeoutSeconds: 86400}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stamped cancel resume=%v", err)
	}
	run, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil || run.Status != "recovery_wait" || run.ForgeParkCount != 6 || run.RecoveryRetryNotBefore.Valid {
		t.Fatalf("isolated residual escaped: %s/%v", run.Status, err)
	}
}
