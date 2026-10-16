package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestPolicyLateDeliveryAckLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	pool, err := store.OpenPool(context.Background(), os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	e.h.wsvc = workersvc.New(e.race, nil, workersvc.Params{PolicyHoldNegotiated: true, PolicyHoldSnapshotEnabled: true, TerminalPendingLease: time.Hour})
	e.h.wsvc.SetTxBeginner(pool)
	id := e.providerHold(0)
	e.exec("UPDATE runs SET harness='codex',status='running',hold_reason=NULL,claim_generation=2,claim_released_at=NULL,session_id='new-live-session' WHERE id=$1", id)
	worker := store.Worker{ID: e.worker, UserID: e.owner.ID, ProtocolCapabilities: []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}}
	tag, phase, corr := "cyberPolicy", "planning", "pr-00000000000000000000000000000000-1"
	body := workersvc.PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 1, ClaimGeneration: 1, Detail: "refused", Tag: &tag, Phase: &phase, CorrelationID: &corr}
	raw, _ := json.Marshal(body)
	wantStatus := "running"
	call := func(wantCode int, wantDisposition string) {
		req := httptest.NewRequest("POST", "/", strings.NewReader(string(raw)))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", id.String())
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		req = req.WithContext(mw.ContextWithWorker(ctx, worker))
		rec := httptest.NewRecorder()
		e.h.WorkerRunPolicyHold(rec, req)
		var ack policyHoldAck
		if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
			t.Fatalf("untyped ACK code=%d body=%s", rec.Code, rec.Body)
		}
		if rec.Code != wantCode || string(ack.Disposition) != wantDisposition || ack.ClaimGeneration != 1 || ack.Status != wantStatus || ack.OutcomeID != body.OutcomeID {
			t.Fatalf("ACK code=%d body=%s", rec.Code, rec.Body)
		}
	}
	call(409, "unauthenticated_historical_outcome")
	hold := uuid.New()
	e.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
 SELECT $1,user_id,repo_id,id,1,'open',$2,'policy-late-http' FROM runs WHERE id=$3`, hold, e.worker, id)
	call(200, "recorded")
	call(200, "already_applied")
	e.exec("UPDATE recovery_custody_holds SET state='discarded' WHERE id=$1", hold)
	call(409, "unauthenticated_historical_outcome")
	e.exec("UPDATE runs SET status='cancelled' WHERE id=$1", id)
	wantStatus = "cancelled"
	call(409, "unauthenticated_historical_outcome")
}
