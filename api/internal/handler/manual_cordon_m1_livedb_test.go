package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestManualCordonMountedPositiveMatrixLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	for _, admin := range []bool{false, true} {
		owner := cliSeedUser(t, pool, admin)
		jwt := cliMintJWT(t, pool, owner)
		scope := clitoken.ScopeUser
		if admin {
			scope = clitoken.ScopeAdminRO
		}
		token := cliMintToken(t, pool, owner, scope)
		for _, cookie := range []bool{false, true} {
			t.Run(fmt.Sprintf("admin=%v/cookie=%v", admin, cookie), func(t *testing.T) {
				id, _ := manualCordonSeedWorker(t, pool, owner, "external", false)
				path := "/api/workers/" + id.String() + "/cordon"
				for _, method := range []string{http.MethodPost, http.MethodDelete} {
					var rec *httptest.ResponseRecorder
					if cookie {
						rec = cookieReq(t, router, method, path, jwt, "")
					} else {
						rec = bearerReq(router, method, path, token)
					}
					w := manualCordonWorkerResponse(t, rec, id)
					if w.EffectiveCordoned != (method == http.MethodPost) {
						t.Fatalf("%s effective cordon=%v", method, w.EffectiveCordoned)
					}
				}
				manualCordonEventCounts(t, pool, id, 1, 1)
				for _, method := range []string{http.MethodPost, http.MethodDelete} {
					rec := bearerReq(router, method, "/api/workers/"+uuid.NewString()+"/cordon", token)
					if rec.Code != 404 {
						t.Fatalf("unknown %s=%d %s", method, rec.Code, rec.Body.String())
					}
				}
			})
		}
	}
}

func TestManualCordonMountedActualBodyBoundLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, length := range []int64{-1, 1} {
			for _, body := range []string{"null", "[]", "42", "{", "{} {}", "{}", `{"unknown":true}`, `{"reason":"` + strings.Repeat("😀", 501) + `"}`, "{}" + strings.Repeat(" ", 8190), "{}" + strings.Repeat(" ", 8191)} {
				// A complete object at the exact byte bound is the positive control.
				name := fmt.Sprintf("%s/length=%d/bytes=%d/%s", method, length, len(body), body[:min(len(body), 12)])
				t.Run(name, func(t *testing.T) {
					id, _ := manualCordonSeedWorker(t, pool, owner, "external", false)
					req := httptest.NewRequest(method, "/api/workers/"+id.String()+"/cordon", strings.NewReader(body))
					req.ContentLength = length
					req.Header.Set("Authorization", "Bearer "+token)
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					want := 400
					if body == "{}" || len(body) == 8192 {
						want = 200
					}
					if rec.Code != want {
						t.Fatalf("actual body bound/shape response=%d %s want=%d", rec.Code, rec.Body.String(), want)
					}
					if want == 400 {
						manualCordonEventCounts(t, pool, id, 0, 0)
						var cordoned bool
						if err := pool.QueryRow(context.Background(), "SELECT manual_cordoned_at IS NOT NULL FROM workers WHERE id=$1", id).Scan(&cordoned); err != nil {
							t.Fatal(err)
						}
						if cordoned {
							t.Fatal("invalid body changed worker")
						}
					}
				})
			}
		}
	}
}

func TestManualCordonMountedClaimSeamLiveDB(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		for _, state := range []string{"new", "assigned", "assigned-hard-fence", "automatic-hold"} {
			t.Run(fmt.Sprintf("snapshot=%v/%s", snapshot, state), func(t *testing.T) {
				ctx := context.Background()
				f := newClaimGuardFixture(ctx, t) // Includes the production SetTxBeginner(pool) wiring.
				h, router, _ := cliLiveDB(t)
				h.wsvc = f.svc
				h.pool = f.pool
				run := f.seedEnabledRepoAndQueuedRun(ctx, t, 9001, 1, protClean)
				key := "uzw_" + uuid.NewString()
				cliMustExec(t, f.pool, "UPDATE workers SET token_hash=$2,last_heartbeat_at=now(),snapshot_register_nonce='m1-nonce' WHERE id=$1", f.wkr.ID, jointoken.Hash(key))
				if strings.HasPrefix(state, "assigned") {
					cliMustExec(t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", run, f.wkr.ID)
				}
				if state == "assigned-hard-fence" {
					cliMustExec(t, f.pool, "UPDATE workers SET maintenance_fenced=true WHERE id=$1", f.wkr.ID)
				}
				if state == "automatic-hold" {
					cliMustExec(t, f.pool, "UPDATE workers SET draining_since=now(),draining_reason='image_roll' WHERE id=$1", f.wkr.ID)
				}
				token := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
				path := "/api/workers/" + f.wkr.ID.String() + "/cordon"
				rec := bearerReqBody(router, http.MethodPost, path, token, `{"reason":"mounted seam"}`)
				if rec.Code != 200 {
					t.Fatalf("cordon=%d %s", rec.Code, rec.Body.String())
				}
				epoch := 0
				claim := func(want int) {
					t.Helper()
					body := ""
					if snapshot {
						epoch++
						body = fmt.Sprintf(`{"active_snapshot":{"register_nonce":"m1-nonce","snapshot_epoch":%d,"active":[]}}`, epoch)
					}
					got := bearerReqBody(router, http.MethodPost, "/api/worker/runs/claim", key, body)
					if got.Code != want {
						t.Fatalf("claim status=%d %s want=%d", got.Code, got.Body.String(), want)
					}
					if want == 200 {
						var payload workersvc.ClaimPayload
						if err := json.Unmarshal(got.Body.Bytes(), &payload); err != nil {
							t.Fatal(err)
						}
						if payload.RunID != run.String() {
							t.Fatalf("claimed run=%s want=%s", payload.RunID, run)
						}
					}
				}
				if state == "assigned" {
					claim(200)
				} else {
					claim(204)
					status, _ := f.runStatusAndReason(ctx, t, run)
					if status != "queued" {
						t.Fatalf("refused claim changed status=%s", status)
					}
				}
				rec = bearerReq(router, http.MethodDelete, path, token)
				if rec.Code != 200 {
					t.Fatalf("uncordon=%d %s", rec.Code, rec.Body.String())
				}
				if state == "assigned" {
					return
				}
				if state == "assigned-hard-fence" || state == "automatic-hold" {
					claim(204)
					cliMustExec(t, f.pool, "UPDATE workers SET maintenance_fenced=false,draining_since=NULL,draining_reason=NULL WHERE id=$1", f.wkr.ID)
				}
				claim(200)
			})
		}
	}
}
