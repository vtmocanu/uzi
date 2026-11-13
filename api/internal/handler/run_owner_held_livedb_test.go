package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestSetTokenOnMemoryPressureHoldLiveDB drives the real set-token route on an owner-held
// worker_memory_pressure recovery_wait run: 409 naming the hold, nothing written.
func TestSetTokenOnMemoryPressureHoldLiveDB(t *testing.T) {
	fx := newCompactPolicyFx(t)
	run := uuid.New()
	fx.exec(`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,harness,worker_id,claim_generation,started_at,recovery_wait_cause)
		VALUES($1,$2,$3,$4,'owner held fixture','desc','issue','recovery_wait','claude',$5,1,now(),'worker_memory_pressure')`,
		run, fx.owner, fx.repo, 36300+int64(run[0])+int64(run[1])*256, fx.workerID)
	const want = "this owner hold prevents switching the token; inspect the run's recovery options: Resume may re-hold it, Cancel ends the run (hold: worker memory pressure)"
	before := fx.state(run)
	rec := bearerReqBody(fx.router, http.MethodPost, "/api/runs/"+run.String()+"/credential", fx.ownerUzc, `{"mode":"auto"}`)
	var msg struct {
		Error string `json:"error"`
	}
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &msg) != nil || msg.Error != want {
		t.Fatalf("set-token = %d %s, want 409 %q", rec.Code, rec.Body.String(), want)
	}
	if fx.state(run) != before {
		t.Fatal("refused set-token changed the run row or its inputs")
	}
}
