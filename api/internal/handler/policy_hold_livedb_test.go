package handler

import (
	"context"
	"encoding/json"
	"net/http"
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

func TestPolicyHoldStrictAndOwnerResumeLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	// The helper owns the database pool through its Queries wrapper.
	e.h.wsvc = workersvc.New(e.race, nil, workersvc.Params{PolicyHoldNegotiated: true, PolicyHoldSnapshotEnabled: true, TerminalPendingLease: time.Hour})
	// wallParkRaceStore wraps Queries but cannot begin transactions; open our own pool.
	pool, err := store.OpenPool(context.Background(), os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	e.h.wsvc.SetTxBeginner(pool)
	id := e.providerHold(0)
	e.exec("UPDATE runs SET harness='codex',status='running',hold_reason=NULL WHERE id=$1", id)
	worker := store.Worker{ID: e.worker, UserID: e.owner.ID, ProtocolCapabilities: []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}}
	call := func(workerRoute bool, body, token string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/", strings.NewReader(body))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", id.String())
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, route)
		if auth {
			if workerRoute {
				ctx = mw.ContextWithWorker(ctx, worker)
			} else {
				ctx = mw.ContextWithUser(ctx, e.owner)
			}
		}
		req = req.WithContext(ctx)
		req.Header.Set("If-Match", token)
		rec := httptest.NewRecorder()
		if workerRoute {
			e.h.WorkerRunPolicyHold(rec, req)
		} else {
			e.h.ResumeRunNow(rec, req)
		}
		return rec
	}
	req := workersvc.PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: 1, ClaimGeneration: 1, Detail: "resume_rejected"}
	// Strict parsing is checked before the service; malformed input cannot mutate the run.
	for _, body := range []string{"null", "{}", `{"unknown":true}`, `{} {}`, "{}" + strings.Repeat(" ", 17000)} {
		if body == "{}" {
			continue
		}
		if rec := call(true, body, "", true); rec.Code != 400 {
			t.Fatalf("worker malformed %q code=%d body=%s", body[:min(len(body), 30)], rec.Code, rec.Body)
		}
	}
	if rec := call(true, "{}", "", false); rec.Code != 401 {
		t.Fatalf("worker auth=%d", rec.Code)
	}
	tag, phase, corr := "cyberPolicy", "implementation", "pr-00000000000000000000000000000000-1"
	req.Detail = "refused"
	req.Tag = &tag
	req.Phase = &phase
	req.CorrelationID = &corr
	raw, _ := json.Marshal(req)
	rec := call(true, string(raw), "", true)
	if rec.Code != 200 {
		t.Fatalf("hold=%d %s", rec.Code, rec.Body)
	}
	var ack policyHoldAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || ack.Disposition != workersvc.PolicyHoldApplied || ack.OutcomeID != req.OutcomeID || ack.Status != "paused" || ack.HoldReason == nil || *ack.HoldReason != "provider_policy_refusal" || ack.ClaimGeneration != 1 {
		t.Fatalf("hold acknowledgement: %+v err=%v", ack, err)
	}
	if strings.Contains(rec.Body.String(), "plan_md") {
		t.Fatal("worker acknowledgement contains plan content")
	}
	held, err := e.race.GetRunByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	token := workersvc.PolicyHoldActionToken(held)
	for _, body := range []string{"null", `{"unknown":true}`, `{} {}`, "{}" + strings.Repeat(" ", 17000), `{"guidance":"` + strings.Repeat("a", 16001) + `"}`, `{"mode":"fresh"}`} {
		rec := call(false, body, token, true)
		if rec.Code != 400 {
			t.Fatalf("owner malformed code=%d body=%s", rec.Code, rec.Body)
		}
	}
	if rec := call(false, `{"guidance":"narrow"}`, "", true); rec.Code != 409 {
		t.Fatalf("missing token=%d %s", rec.Code, rec.Body)
	}
	if rec := call(false, `{"guidance":"narrow"}`, token, true); rec.Code != 200 {
		t.Fatalf("preserve=%d %s", rec.Code, rec.Body)
	}
	if rec := call(false, `{"guidance":"duplicate"}`, token, true); rec.Code != 409 {
		t.Fatalf("duplicate=%d", rec.Code)
	}
	var inputs, audits int
	if err := e.query("SELECT count(*) FILTER(WHERE kind='follow_up'),count(*) FILTER(WHERE kind='resume') FROM run_user_inputs WHERE run_id=$1", id).Scan(&inputs, &audits); err != nil || inputs != 1 || audits != 1 {
		t.Fatalf("inputs=%d audit=%d err=%v", inputs, audits, err)
	}
	// Bodyless ordinary owner resume remains supported.
	ordinary := e.providerHold(0)
	if rec := e.post(e.h.ResumeRunNow, ordinary, ""); rec.Code != http.StatusOK {
		t.Fatalf("ordinary resume=%d %s", rec.Code, rec.Body)
	}
}
