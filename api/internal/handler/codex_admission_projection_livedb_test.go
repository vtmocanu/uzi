package handler

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCodexAdmissionOwnerActionsLiveDB(t *testing.T) {
	db := ephemeralPriorityDatabase(t)
	fx := priorityFixture(t, db)
	alias := fx.seedEphemeralAdmissionAlias()
	run := fx.queuedRun([]string{})
	fx.bindEphemeralAdmission(run, alias)
	fx.holdEphemeralAdmission(alias)
	worker := fx.onlineWorker("codex", false)
	caps := []string{"codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, caps); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET status='paused',started_at=now()-interval '1 minute' WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	svc.SetTxBeginner(fx.pool)
	h := &Handler{q: fx.q, wsvc: svc}
	rec := httptest.NewRecorder()
	h.ResumeRunNow(rec, userReq(http.MethodPost, "/api/runs/"+run.String()+"/resume-now", "", fx.userID, map[string]string{"id": run.String()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner Resume=%d %s", rec.Code, rec.Body.String())
	}
	r, err := fx.q.GetRunByID(fx.ctx, run)
	if err != nil || r.Status != "queued" || r.ClaimGeneration != 0 {
		t.Fatalf("resume did not remain queued: %+v %v", r, err)
	}
	now := time.Now()
	params := store.ClaimRunParams{UserID: fx.userID, WorkerID: pgconv.UUID(worker),
		WorkerProtocolCaps: caps, WorkerCaps: []string{}, CodexCuratedModels: workersvc.CodexCuratedModels(),
		HeartbeatCutoff: pgconv.Time(now.Add(-time.Minute)), AffinityCutoff: pgconv.Time(now.Add(-25 * time.Minute)),
		SpreadCutoff: pgconv.Time(now.Add(-9 * time.Second)), BackgroundGraceCutoff: pgconv.Time(now.Add(-15 * time.Minute))}
	if _, err := fx.q.ClaimRun(fx.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Resume bypassed cooldown: %v", err)
	}
	var before, after []byte
	if err := fx.pool.QueryRow(fx.ctx, "SELECT to_jsonb(a) FROM codex_account_admission_throttle a WHERE user_id=$1", fx.userID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.CreateRunInput(rec, userReq(http.MethodPost, "/api/runs/"+run.String()+"/inputs", `{"kind":"cancel"}`, fx.userID, map[string]string{"id": run.String()}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("cooled cancel=%d %s", rec.Code, rec.Body.String())
	}
	r, err = fx.q.GetRunByID(fx.ctx, run)
	if err != nil || r.Status != "cancelled" {
		t.Fatalf("cancel did not settle queued run: %s %v", r.Status, err)
	}
	if err := fx.pool.QueryRow(fx.ctx, "SELECT to_jsonb(a) FROM codex_account_admission_throttle a WHERE user_id=$1", fx.userID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("owner action changed account cooldown: %v", err)
	}
	// A replacement root after cancellation still resolves to the same account.
	replacement := fx.queuedRun([]string{})
	fx.bindEphemeralAdmission(replacement, alias)
	if _, err := fx.q.ClaimRun(fx.ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replacement bypassed cooldown: %v", err)
	}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE codex_account_admission_throttle SET effective_expiry=now()-interval '2 minutes' WHERE user_id=$1", fx.userID); err != nil {
		t.Fatal(err)
	}
	claimed, err := fx.q.ClaimRun(fx.ctx, params)
	if err != nil || claimed.ID != replacement || claimed.ClaimGeneration != 1 {
		t.Fatalf("expired owner replacement control: %s %v", claimed.ID, err)
	}
}

// Exercise every generated projection against the same persisted policy row.
// Viewer authorization belongs to the read, not to the presence of an account ID.
func TestCodexAdmissionReadProjectionsLiveDB(t *testing.T) {
	db := ephemeralPriorityDatabase(t)
	fx := priorityFixture(t, db)
	alias := fx.seedEphemeralAdmissionAlias()
	run := fx.queuedRun([]string{})
	fx.bindEphemeralAdmission(run, alias)
	fx.holdEphemeralAdmission(alias)
	fx.liftPauseOnCleanup()
	h := &Handler{q: fx.q}
	ownerCtx := mw.ContextWithUser(fx.ctx, store.User{ID: fx.userID})
	foreignCtx := mw.ContextWithUser(fx.ctx, store.User{ID: uuid.New(), IsAdmin: true})
	var now time.Time
	if err := fx.pool.QueryRow(fx.ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, class               string
		rate, overload, effective time.Duration
	}{
		{"rate", "rate_limit", time.Hour, 30 * time.Minute, time.Hour},
		{"overload", "overload", 30 * time.Minute, time.Hour, time.Hour},
		{"tie", "rate_limit", time.Hour, time.Hour, time.Hour},
		{"recovery floor", "rate_limit", time.Hour, 30 * time.Minute, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fx.pool.Exec(fx.ctx, `UPDATE codex_account_admission_throttle SET rate_expiry=$2,overload_expiry=$3,effective_expiry=$4,recovery_floor=$4 WHERE user_id=$1`,
				fx.userID, now.Add(tc.rate), now.Add(tc.overload), now.Add(tc.effective))
			if err != nil {
				t.Fatal(err)
			}
			var expected time.Time
			if err := fx.pool.QueryRow(fx.ctx, "SELECT codex_admission_run_eligible_at($1)", run).Scan(&expected); err != nil {
				t.Fatal(err)
			}
			r, err := fx.q.GetRunByID(fx.ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			dto := runToDTO(r, "", 0, 0, 0, now, 0)
			h.overlayQueueReasons(ownerCtx, &dto)
			if dto.QueueReason == nil || *dto.QueueReason != "codex_"+tc.class || dto.CodexAdmissionClass == nil || *dto.CodexAdmissionClass != tc.class || dto.CodexAdmissionEligibleAt == nil || !dto.CodexAdmissionEligibleAt.Equal(expected) {
				t.Fatalf("run projection disagrees: %+v, expected %s at %v", dto.CodexAdmissionClass, tc.class, expected)
			}
			if dto.HealthReason == nil || *dto.HealthReason != apitypes.CodexAdmissionReason(tc.class, expected) {
				t.Fatal("incorrect derived wait")
			}
			foreign := runToDTO(r, "", 0, 0, 0, now, 0)
			h.overlayQueueReasons(foreignCtx, &foreign)
			if foreign.QueueReason != nil || foreign.CodexAdmissionClass != nil || foreign.CodexAdmissionEligibleAt != nil || foreign.HealthReason != nil {
				t.Fatal("non-owner read leaked account admission")
			}
			legacy, err := fx.q.ListRunQueueReasons(fx.ctx, []uuid.UUID{run})
			if err != nil || len(legacy) != 0 {
				t.Fatalf("jobs' legacy maintenance-only projection changed: %+v %v", legacy, err)
			}
			latest, err := fx.q.ListLatestRunsForRepo(fx.ctx, fx.repoID)
			if err != nil || len(latest) != 1 || latest[0].CodexAdmissionClass.String != tc.class || !latest[0].CodexAdmissionEligibleAt.Time.Equal(expected) {
				t.Fatalf("board SQL projection disagrees: %v %v", latest, err)
			}
			single, err := fx.q.GetLatestRunForIssue(fx.ctx, store.GetLatestRunForIssueParams{RepoID: fx.repoID, IssueIid: r.IssueIid})
			if err != nil || single.CodexAdmissionClass.String != tc.class || !single.CodexAdmissionEligibleAt.Time.Equal(expected) {
				t.Fatalf("card SQL projection disagrees: %v", err)
			}
			health, err := fx.q.ListActiveRunsForHealth(fx.ctx, workersvc.CodexCuratedModels())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range health {
				if row.ID == run {
					found = true
					if row.CodexAccountGated || row.CodexAdmissionClass.String != tc.class || !row.CodexAdmissionEligibleAt.Time.Equal(expected) {
						t.Fatalf("health SQL projection disagrees: %+v", row)
					}
				}
			}
			if !found {
				t.Fatal("health omitted queued run")
			}
		})
	}
	fx.setIntakePause(true)
	r, err := fx.q.GetRunByID(fx.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	dto := runToDTO(r, "", 0, 0, 0, now, 0)
	h.overlayQueueReasons(ownerCtx, &dto)
	if dto.QueueReason == nil || *dto.QueueReason != "maintenance" || dto.CodexAdmissionClass == nil {
		t.Fatal("maintenance lost precedence or independent admission details")
	}
	other := runToDTO(r, "", 0, 0, 0, now, 0)
	h.overlayQueueReasons(foreignCtx, &other)
	if other.QueueReason == nil || *other.QueueReason != "maintenance" || other.CodexAdmissionClass != nil || other.CodexAdmissionEligibleAt != nil {
		t.Fatal("public maintenance leaked private admission")
	}
	fx.setIntakePause(false)
	// PRD #3522: a weekly reserve hold outranks the still-open cooldown in every SQL projection,
	// while the owner keeps the independent admission details.
	reserve := "weekly usage reserve: 72% >= 70%"
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET reserve_hold_reason=$2,health='waiting_worker',health_reason=$2 WHERE id=$1", run, reserve); err != nil {
		t.Fatal(err)
	}
	r, err = fx.q.GetRunByID(fx.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	dto = runToDTO(r, "", 0, 0, 0, now, 0)
	h.overlayQueueReasons(ownerCtx, &dto)
	if dto.QueueReason == nil || *dto.QueueReason != "weekly_reserve" || dto.CodexAdmissionClass == nil || dto.HealthReason == nil || *dto.HealthReason != reserve {
		t.Fatalf("weekly reserve lost precedence over the cooldown: queue=%v class=%v health=%v", dto.QueueReason, dto.CodexAdmissionClass, dto.HealthReason)
	}
	if latest, err := fx.q.ListLatestRunsForRepo(fx.ctx, fx.repoID); err != nil || len(latest) != 1 || latest[0].QueueReason != "weekly_reserve" {
		t.Fatalf("board SQL precedence disagrees: %v %v", latest, err)
	}
	if single, err := fx.q.GetLatestRunForIssue(fx.ctx, store.GetLatestRunForIssueParams{RepoID: fx.repoID, IssueIid: r.IssueIid}); err != nil || single.QueueReason != "weekly_reserve" {
		t.Fatalf("card SQL precedence disagrees: %v", err)
	}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET reserve_hold_reason=NULL,health='ok',health_reason=NULL WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}
	stale := apitypes.CodexAdmissionReason("rate_limit", now.Add(-time.Minute))
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET health='waiting_worker',health_reason=$2 WHERE id=$1", run, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(fx.ctx, "UPDATE codex_account_admission_throttle SET effective_expiry=now()-interval '2 minutes' WHERE user_id=$1", fx.userID); err != nil {
		t.Fatal(err)
	}
	r, err = fx.q.GetRunByID(fx.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	dto = runToDTO(r, "", 0, 0, 0, now, 0)
	h.overlayQueueReasons(ownerCtx, &dto)
	if dto.QueueReason != nil || dto.CodexAdmissionClass != nil || dto.CodexAdmissionEligibleAt != nil || dto.HealthReason != nil {
		t.Fatal("expired read retained provider wait")
	}
	newlyQueued := fx.queuedRun([]string{})
	fx.bindEphemeralAdmission(newlyQueued, alias)
	var count int
	if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM codex_queued_run_admission WHERE user_id=$1", fx.userID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("finite spread did not release old/new rows: %d %v", count, err)
	}
}
