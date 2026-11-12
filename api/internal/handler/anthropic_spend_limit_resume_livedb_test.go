package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #3767: owner Resume of an anthropic_spend_limit hold through the real handler, against a
// REAL Postgres (skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh).
func TestAnthropicSpendLimitOwnerResumeLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	t.Cleanup(func() {
		// Other LiveDB rollback tests share this database and narrow the cause CHECK.
		_, _ = fx.pool.Exec(fx.ctx, `UPDATE runs SET recovery_wait_cause=NULL WHERE user_id=$1 AND recovery_wait_cause='anthropic_spend_limit'`, fx.userID)
	})
	h := &Handler{q: fx.q}
	h.cfg.RunTimeout = time.Hour
	w := fx.onlineWorker("spend-resume-"+uuid.NewString(), true)
	id := fx.queuedRun([]string{})
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',worker_id=$2,claim_generation=3,
    claim_released_at=NULL,recovery_wait_cause='anthropic_spend_limit',recovery_retry_not_before=NULL,
    started_at=now()-interval '10 minutes',budget_wall_seconds=3600,status_since=now()-interval '2 minutes' WHERE id=$1`, id, w); err != nil {
		t.Fatal(err)
	}
	before, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec := recoveryResumeRequest(h, id, uuid.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign resume %d", rec.Code)
	}
	if got, _ := fx.q.GetRunByID(fx.ctx, id); got.Status != "recovery_wait" || got.RecoveryWaitCause != before.RecoveryWaitCause {
		t.Fatalf("foreign resume changed the row: %+v", got)
	}
	rec := recoveryResumeRequest(h, id, fx.userID)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner resume %d %s", rec.Code, rec.Body.String())
	}
	after, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "queued" || after.RecoveryWaitCause.Valid || after.RecoveryRetryNotBefore.Valid || !after.ClaimReleasedAt.Valid {
		t.Fatalf("resumed row %+v", after)
	}
	if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate resume %d", rec.Code)
	}
}
