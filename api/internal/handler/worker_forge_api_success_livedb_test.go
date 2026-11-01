package handler

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Issue #1324 D9: forge-clone-success carries a surface. The api surface needs the
// forge_api_success_v1 capability and clears only the API flag of the exact reservation; the git
// surface (explicit or absent) clears only the Git flag. On a same-host origin both live on one row.
func TestForgeAPISuccessRouteLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 1324, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker, incarnation := uuid.New(), uuid.New()
	token := "api-success-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'api-success',$3,'online',$4)",
		worker, owner, sum[:], []string{capability.ForgeCloneSuccessV1})
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
	cliMustExec(t, pool, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,
 incarnation,reservation_run_id,reservation_generation,reservation_incarnation,reservation_version,reservation_observation_version,
 git_unverified,api_unverified)
 VALUES($1,$2,'https://forge.e2e',now(),'transient_publication_exhausted',now(),$3,$4,1,$3,7,1,true,true)`, owner, worker, incarnation, run)
	cliMustExec(t, pool, `UPDATE run_forge_origins SET reservation_incarnation=$2,reservation_version=7,
 api_origin=origin,api_reservation_incarnation=$2,api_reservation_version=7 WHERE run_id=$1`, run, incarnation)
	body := func(surface string) string {
		extra := ""
		if surface != "" {
			extra = fmt.Sprintf(`,"surface":%q`, surface)
		}
		return fmt.Sprintf(`{"claim_generation":1,"incarnation":%q,"version":7,"observation_version":1%s}`, incarnation.String(), extra)
	}
	call := func(payload string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/forge-clone-success", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	expect := func(payload string, status int, applied string) {
		t.Helper()
		rec := call(payload)
		if rec.Code != status || (applied != "" && strings.TrimSpace(rec.Body.String()) != applied) {
			t.Fatalf("route status=%d body=%s want=%d %s for %s", rec.Code, rec.Body.String(), status, applied, payload)
		}
	}
	flags := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(t.Context(), `SELECT COALESCE((SELECT 'git='||git_unverified||' api='||api_unverified
			FROM forge_origin_cooldowns WHERE worker_id=$1),'deleted')`, worker).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A worker that never negotiated the api surface is refused, and nothing changes.
	expect(body("api"), http.StatusBadRequest, "")
	if got := flags(); got != "git=true api=true" {
		t.Fatalf("refused api report changed the row: %s", got)
	}
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker,
		[]string{capability.ForgeCloneSuccessV1, capability.ForgeAPISuccessV1})
	expect(body("bogus"), http.StatusBadRequest, "")
	expect(strings.Replace(body("api"), `"version":7`, `"version":8`, 1), http.StatusOK, `{"applied":false}`)
	expect(strings.Replace(body("api"), `"observation_version":1`, `"observation_version":2`, 1), http.StatusOK, `{"applied":false}`)
	if got := flags(); got != "git=true api=true" {
		t.Fatalf("stale api reports changed the row: %s", got)
	}
	// The exact api report clears the API flag only; repeating it applies nothing.
	expect(body("api"), http.StatusOK, `{"applied":true}`)
	if got := flags(); got != "git=true api=false" {
		t.Fatalf("api success flags: %s", got)
	}
	expect(body("api"), http.StatusOK, `{"applied":false}`)
	// The Git surface (an old worker sends no surface) then clears the rest and the row goes.
	expect(body(""), http.StatusOK, `{"applied":true}`)
	if got := flags(); got != "deleted" {
		t.Fatalf("git success after api success: %s", got)
	}
}
