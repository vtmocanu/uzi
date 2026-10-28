package handler

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"net/http/httptest"
	"strings"
)

func TestIdleStallOwnerResumeLiveDB(t *testing.T) {
	for _, timed := range []bool{false, true} {
		t.Run(fmt.Sprint(timed), func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			h := &Handler{q: fx.q}
			h.cfg.RunTimeout = time.Hour
			w := fx.onlineWorker("idle-resume-"+uuid.NewString(), true)
			id := fx.queuedRun([]string{})
			_, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status='recovery_wait',worker_id=$2,
    claim_generation=7,claim_released_at=now(),released_worker_id=$2,
    recovery_wait_cause='codex_idle_stall',idle_stall_park_count=4,recovery_wait_count=9,
    recovery_retry_not_before=CASE WHEN $3::boolean THEN now()+interval '1 hour' END,
    worker_recovery_episode=3,requeue_count=7,requeue_episode_baseline=5,
    provider_park_count=4,provider_park_streak_started_at=now()-interval '1 hour',
    session_id='idle-resume-session',started_at=now()-interval '10 minutes',
    budget_wall_seconds=3600,budget_paused_seconds=17,codex_cap_hash='idle-cap',
    health='stalled',health_reason='idle test',health_since=now()-interval '3 minutes',
    status_since=now()-interval '2 minutes' WHERE id=$1`, id, w, timed)
			if err != nil {
				t.Fatal(err)
			}
			before, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if rec := recoveryResumeRequest(h, id, uuid.New()); rec.Code != http.StatusNotFound {
				t.Fatalf("foreign %d", rec.Code)
			}
			if _, err = fx.q.ResumeCodexIdleStall(fx.ctx, store.ResumeCodexIdleStallParams{ID: id, UserID: uuid.New()}); err == nil {
				t.Fatal("foreign query admitted")
			}
			rec := recoveryResumeRequest(h, id, fx.userID)
			if rec.Code != http.StatusOK {
				t.Fatalf("resume %d %s", rec.Code, rec.Body.String())
			}
			after, err := fx.q.GetRunByID(fx.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "queued" || after.RecoveryRetryNotBefore.Valid || after.IdleStallParkCount != 0 ||
				after.StartedAt.Valid || after.BudgetPausedSeconds != 0 || after.CodexCapHash != nil ||
				after.CodexClaimEpoch != before.CodexClaimEpoch+1 || after.Health != "ok" || after.HealthReason.Valid || after.HealthSince.Valid ||
				!after.StatusSince.Time.After(before.StatusSince.Time) || !after.UpdatedAt.Time.After(before.UpdatedAt.Time) {
				t.Fatalf("resume shape %+v", after)
			}
			// Compare the FULL ROW, permitting exactly the specified Resume columns.
			allowed := map[string]bool{"Status": true, "RecoveryRetryNotBefore": true, "IdleStallParkCount": true,
				"StartedAt": true, "BudgetPausedSeconds": true, "CodexCapHash": true, "CodexClaimEpoch": true,
				"StatusSince": true, "Health": true, "HealthReason": true, "HealthSince": true, "UpdatedAt": true}
			a, b := reflect.ValueOf(before), reflect.ValueOf(after)
			for i := 0; i < a.NumField(); i++ {
				name := a.Type().Field(i).Name
				if !allowed[name] && !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
					t.Fatalf("Resume changed preserved column %s", name)
				}
			}
			var audits int
			if err = fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", id).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("audit %d %v", audits, err)
			}
			if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
				t.Fatalf("duplicate %d", rec.Code)
			}
			// A status or cause race must be refused by the query's final write guard.
			for _, mutation := range []string{"UPDATE runs SET status='running' WHERE id=$1", "UPDATE runs SET status='recovery_wait',recovery_wait_cause='forge_unreachable' WHERE id=$1"} {
				if _, err = fx.pool.Exec(fx.ctx, mutation, id); err != nil {
					t.Fatal(err)
				}
				if _, err = fx.q.ResumeCodexIdleStall(fx.ctx, store.ResumeCodexIdleStallParams{ID: id, UserID: fx.userID}); err == nil {
					t.Fatal("raced Resume admitted")
				}
			}
		})
	}
}

func TestIdleStallHeldCredentialError(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).writeSetRunCredentialError(rec, workersvc.ErrCodexIdleStallHeld)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "codex_idle_stall") || !strings.Contains(rec.Body.String(), "Resume") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
