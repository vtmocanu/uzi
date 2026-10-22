package handler

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

func TestManualCordonResponseProjectionLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	now := time.Now()
	h.now = func() time.Time { return now }
	owner := cliSeedUser(t, pool, false)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	id, _ := manualCordonSeedWorker(t, pool, owner, "external", false)
	run := cliSeedJudgedRun(t, pool, owner)
	cliMustExec(t, pool, "UPDATE runs SET status='running',worker_id=$2 WHERE id=$1", run, id)
	label := strings.Repeat("😀", 200)
	cliMustExec(t, pool, "UPDATE users SET display_name=$2 WHERE id=$1", owner, "\x1b]0;hidden\a"+label+"overflow\x1b[0m")
	for _, method := range []string{http.MethodPost, http.MethodPost, http.MethodDelete, http.MethodDelete} {
		rec := bearerReqBody(router, method, "/api/workers/"+id.String()+"/cordon", token, `{"reason":"\u001b]8;;https://example.com\u0007Inspect 😀\u001b]8;;\u001b\\"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("cordon=%d %s", rec.Code, rec.Body.String())
		}
		var mutated struct {
			Worker apitypes.WorkerDTO `json:"worker"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &mutated); err != nil {
			t.Fatal(err)
		}
		if !mutated.Worker.Busy || mutated.Worker.ActiveRuns != 1 {
			t.Fatalf("lost activity: %+v", mutated.Worker)
		}
		if method == http.MethodPost && (mutated.Worker.ManualCordonedByLabel == nil || *mutated.Worker.ManualCordonedByLabel != label || mutated.Worker.ManualCordonReason == nil || *mutated.Worker.ManualCordonReason != "Inspect 😀") {
			t.Fatalf("lost metadata/activity: %+v", mutated.Worker)
		}
		list := bearerReq(router, http.MethodGet, "/api/workers", token)
		if list.Code != http.StatusOK {
			t.Fatalf("list=%d %s", list.Code, list.Body.String())
		}
		var listed struct {
			Workers []apitypes.WorkerDTO `json:"workers"`
		}
		if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		if len(listed.Workers) != 1 || !reflect.DeepEqual(mutated.Worker, listed.Workers[0]) {
			t.Fatal("mutation/no-op response differs from full list projection and overlays")
		}
	}
}
