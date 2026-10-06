package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestTerminalRejectionRegisterHeartbeatLiveDB(t *testing.T) {
	for _, path := range []string{"register", "attested", "heartbeat"} {
		for _, scope := range []string{"exact", "foreign", "history", "unannotated"} {
			t.Run(path+"/"+scope, func(t *testing.T) {
				e := newSettleEnv(t)
				q, box := store.New(e.pool), newHandlerTestBox(t)
				svc := workersvc.New(q, box, workersvc.Params{ActiveSnapshotMaxEntries: 256})
				svc.SetTxBeginner(e.pool)
				h := &Handler{pool: e.pool, q: q, box: box, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}, wsvc: svc}
				lim := mw.NewLimiter(100000, time.Minute, nil)
				e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
				e.exec("UPDATE runs SET status='running', claim_generation=1, requeue_count=2, claim_released_at=NULL, status_since=now()-interval '1 hour', started_at=now(), interactive=true WHERE id=$1", e.run)
				e.exec("UPDATE workers SET snapshot_register_nonce='u1-nonce', snapshot_epoch=0 WHERE id=$1", e.workerA)
				if scope != "unannotated" {
					token, generation := e.tokenA, int64(1)
					if scope == "foreign" {
						token = e.tokenB
					}
					if scope == "history" {
						generation = 2
					}
					rec := rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", token,
						fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":%d,"reason":"mac_failure"}]}`, e.run, generation))
					assertRejectionDisposition(t, rec, e.run, generation, "recorded")
				}
				endpoint, body := "/api/worker/register", "{}"
				switch path {
				case "attested":
					// Max requeues is zero, so the valid attestation cannot use an allowance.
					body = fmt.Sprintf(`{"active_snapshot":{"snapshot_epoch":1,"active":[],"finalize_resume":[{"run_id":"%s","claim_generation":1}]}}`, e.run)
				case "heartbeat":
					endpoint = "/api/worker/heartbeat"
					body = `{"active_snapshot":{"snapshot_epoch":1,"register_nonce":"u1-nonce","active":[]}}`
				}
				rec := rejectionHTTP(e, http.MethodPost, endpoint, e.tokenA, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", endpoint, rec.Code, rec.Body.String())
				}
				var status, origin, reason string
				var generation int64
				if err := e.pool.QueryRow(e.ctx, "SELECT status,fail_origin,failure_reason,claim_generation FROM runs WHERE id=$1", e.run).Scan(&status, &origin, &reason, &generation); err != nil {
					t.Fatal(err)
				}
				want := "worker restarted; run orphaned and out of re-queue budget"
				if path == "heartbeat" {
					want = "worker lost the execution; exceeded re-queue budget"
				}
				if scope == "exact" {
					want = rejectionExplanation
				}
				if status != "failed" || origin != "worker_lost" || reason != want || generation != 1 {
					t.Fatalf("run status=%s origin=%s reason=%q generation=%d; want failed/worker_lost/%q/1", status, origin, reason, generation, want)
				}
				var state string
				if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", e.pred).Scan(&state); err != nil {
					t.Fatal(err)
				}
				if state != "open" {
					t.Fatalf("custody state=%s; want open", state)
				}
			})
		}
	}
}
