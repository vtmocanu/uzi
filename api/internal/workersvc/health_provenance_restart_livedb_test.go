package workersvc

import (
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexProvenanceReleasedWorkerRestartReasonLiveDB(t *testing.T) {
	for _, young := range []bool{true, false} {
		name := "aged queued run"
		if young {
			name = "young queued run"
		}
		t.Run(name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newClaimProvFix(t, env)
			fx.firstClaimByA(t)
			env.exec(`UPDATE runs SET status='running', started_at=now()-interval '2 hours',
				status_since=now()-interval '2 hours', budget_wall_seconds=60 WHERE id=$1`, fx.runID)
			env.exec(`UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1`, fx.workerA)
			now := time.Now()
			parked, err := env.q.ParkRunsAtWall(env.ctx, store.ParkRunsAtWallParams{
				Now: pgconv.Time(now), GlobalTimeoutSeconds: 3600,
				WorkerStaleCutoff: pgconv.Time(now.Add(-45 * time.Second)), GraceSeconds: 600,
			})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range parked {
				found = found || row.ID == fx.runID
			}
			if !found {
				t.Fatal("server wall park did not release the run")
			}
			if _, err := env.q.ExtendAndResumeWallPark(env.ctx, store.ExtendAndResumeWallParkParams{
				ID: fx.runID, UserID: fx.userID, Secs: 10800, Cap: 57600, BaseWallUpperSeconds: 86400,
				FinalizeAllowanceSeconds: 600, MaxWallSeconds: 172800, GlobalTimeoutSeconds: 3600,
			}); err != nil {
				t.Fatal(err)
			}
			r := mustRun(t, env, fx.runID)
			if r.Status != "queued" || r.WorkerID.Valid || r.ReleasedWorkerID != pgconv.UUID(fx.workerA) || r.ReleasedWorkerNonce.String != "nonce-A" {
				t.Fatalf("resumed wall park lost its released-incarnation fence: %+v", r)
			}
			// A is online again, but the same nonce still prevents its claim. No
			// foreign worker can help: provenance holds the run for A alone.
			fx.assertRefused(t, fx.workerA)
			fx.assertRefused(t, fx.workerB)
			if !young {
				env.exec(`UPDATE runs SET status_since=now()-interval '3 hours', updated_at=now()-interval '3 hours' WHERE id=$1`, fx.runID)
			}
			fx.svc.detectRunHealth(env.ctx, time.Now())
			r = mustRun(t, env, fx.runID)
			want := fmt.Sprintf("held for worker %s: restart it or cancel the run; its previous process was released; only the worker that last ran this Codex run may resume it", fx.nameA)
			if r.Health != healthWaitingWorker || r.HealthReason.String != want {
				t.Fatalf("health=(%s,%q), want waiting_worker and %q", r.Health, r.HealthReason.String, want)
			}
			// Re-registration removes the nonce fence, so ordinary waiting advice
			// returns and only A can make the next fresh claim.
			env.exec(`UPDATE workers SET snapshot_register_nonce='nonce-A-restarted' WHERE id=$1`, fx.workerA)
			fx.svc.detectRunHealth(env.ctx, time.Now())
			r = mustRun(t, env, fx.runID)
			if r.HealthReason.String != fmt.Sprintf(provHeldNamedA, fx.nameA) {
				t.Fatalf("after restart reason=%q, want ordinary recorded-worker wait", r.HealthReason.String)
			}
			fx.assertRefused(t, fx.workerB)
			if payload := fx.claim(t, fx.workerA); payload == nil || payload.Secrets.Codex == nil {
				t.Fatal("restarted recorded worker A could not claim")
			}
		})
	}
}
