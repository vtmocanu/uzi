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

func TestForgeCloneSuccessRouteLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 3585, true)
	run := rmSeedRun(t, pool, owner, repo, "running")
	worker, incarnation := uuid.New(), uuid.New()
	token := "clone-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'clone',$3,'online',$4)",
		worker, owner, sum[:], []string{capability.ForgeCloneSuccessV1})
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", run, worker)
	cliMustExec(t, pool, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,
 incarnation,reservation_run_id,reservation_generation,reservation_incarnation,reservation_version,reservation_observation_version)
 VALUES($1,$2,'https://forge.e2e',now(),'transient_clone_fetch_exhausted',now(),$3,$4,1,$3,7,1)`, owner, worker, incarnation, run)
	cliMustExec(t, pool, "UPDATE run_forge_origins SET reservation_incarnation=$2,reservation_version=7 WHERE run_id=$1", run, incarnation)
	body := fmt.Sprintf(`{"claim_generation":1,"incarnation":%q,"version":7,"observation_version":1}`, incarnation.String())
	call := func(auth, payload string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+run.String()+"/forge-clone-success", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+auth)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	assert := func(auth, payload string, status int, applied string) {
		t.Helper()
		rec := call(auth, payload)
		if rec.Code != status || (applied != "" && strings.TrimSpace(rec.Body.String()) != applied) {
			t.Fatalf("route status=%d body=%s want=%d %s", rec.Code, rec.Body.String(), status, applied)
		}
	}
	assert("invalid", body, http.StatusUnauthorized, "")
	assert(token, strings.TrimSuffix(body, "}")+`,"url":"https://arbitrary.example"}`, http.StatusBadRequest, "")
	assert(token, body+" {}", http.StatusBadRequest, "")
	assert(token, strings.Replace(body, `"version":7`, `"version":8`, 1), http.StatusOK, `{"applied":false}`)
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", worker)
	assert(token, body, http.StatusBadRequest, "")
	var retained bool
	if err := pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM forge_origin_cooldowns WHERE worker_id=$1 AND reservation_run_id=$2)", worker, run).Scan(&retained); err != nil || !retained {
		t.Fatalf("rejected reports changed reservation: retained=%v err=%v", retained, err)
	}
	cliMustExec(t, pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, []string{capability.ForgeCloneSuccessV1})
	assert(token, body, http.StatusOK, `{"applied":true}`)
	assert(token, body, http.StatusOK, `{"applied":false}`)
	h.wsvc.SetTxBeginner(nil)
	assert(token, body, http.StatusServiceUnavailable, "")
	var unchanged bool
	if err := pool.QueryRow(t.Context(), `SELECT status='running' AND completion_final_head IS NULL
 AND source_publication_intent IS NULL AND claim_generation=1 FROM runs WHERE id=$1`, run).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("advisory success altered run: unchanged=%v err=%v", unchanged, err)
	}
}
