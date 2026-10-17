package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestPolicyStopReviewOwnerLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	ctx := context.Background()
	pool, err := store.OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	params := workersvc.Params{PolicyStopNegotiated: true, PolicyHoldNegotiated: true, PolicyHoldSnapshotEnabled: true, TerminalPendingLease: time.Hour, RunTimeout: 16 * time.Hour}
	service := func(p workersvc.Params) { e.h.wsvc = workersvc.New(e.race, nil, p); e.h.wsvc.SetTxBeginner(pool) }
	service(params)
	id := e.providerHold(1)
	outcome, presentation, ccID := uuid.New(), uuid.New(), uuid.New()
	payload := `{"plan_md":"exact candidate\n","milestones":[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"},{"id":"m3","title":"m3"}],"required_capabilities":[],"required_tools":[],"size_class":"s"}`
	// gatePresentedPayload canonicalizes milestone object keys; this literal has that order.
	digest := sha256.Sum256([]byte(payload))
	inputID := strconv.FormatInt(time.Now().UnixNano(), 10)
	const sourceGen = "9007199254740995"
	const reviewGen = "9007199254740997"
	const gateRev = "9007199254740993"
	source := `{"version":1,"gate":{"revision":"` + gateRev + `","presentation_id":"` + presentation.String() + `","payload_digest":"` + hex.EncodeToString(digest[:]) + `"},"primary_revision":{"source":"run_user_input","input_id":"` + inputID + `"},"associated_cross_checks":[{"source":"plan_cross_check","cross_check_id":"` + ccID.String() + `"}]}`
	e.exec(`UPDATE runs SET harness='codex',hold_reason='provider_policy_refusal',policy_refusal_count=1,
 policy_last_outcome_id=$2,policy_last_source_generation=$3,claim_generation=$4,gate_revision=$5,gate_presentation_id=$6,
 gate_presented_payload=$7::jsonb,gate_payload_digest=$8,policy_revision_source=$9::jsonb,
 plan_md=E'exact candidate\n',milestones_candidate=milestones_frozen,size_class='s',started_at=status_since-interval '1 minute',budget_wall_seconds=57600
 WHERE id=$1`, id, outcome, sourceGen, reviewGen, gateRev, presentation, payload, digest[:], source)
	human := "  exact human body\n\n"
	e.exec(`INSERT INTO run_user_inputs(id,run_id,kind,body,gate_binding,gate_revision) VALUES($1,$2,'revise_plan',$3,'bound',$4)`, inputID, id, human, gateRev)
	frozen := json.RawMessage(`[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"},{"id":"m3","title":"m3"}]`)
	candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "exact candidate\n", Milestones: frozen, RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "exact diff\n"}
	ccDigest, err := candidate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,findings,created_at,decided_at,deadline_at,checker_harness,checker_model,checker_effort,automatic_rounds_enabled,automatic_revision_limit)
 VALUES($1,$2,'plan',1,2,$3,$4::jsonb,'{}','{}','s',$5,$6,$7,'revise','[{"body":"exact finding"}]',now(),now(),now()+interval '1 hour','claude','checker-model','high',true,2)`, ccID, id, candidate.PlanMd, string(frozen), candidate.BaseCommit, candidate.PlanningDiff, ccDigest)
	held, err := e.race.GetRunByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	holdToken := workersvc.PolicyHoldActionToken(held)
	call := func(owner store.User, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/runs/"+id.String()+"/policy-stop-review", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", id.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), owner), chi.RouteCtxKey, route))
		req.Header.Set("If-Match", token)
		rec := httptest.NewRecorder()
		e.h.PolicyStopReview(rec, req)
		return rec
	}
	snapshot := func() string {
		var r, i, c string
		if err := e.query("SELECT row_to_json(runs)::text FROM runs WHERE id=$1", id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		if err := e.query("SELECT row_to_json(run_user_inputs)::text FROM run_user_inputs WHERE id=$1", inputID).Scan(&i); err != nil {
			t.Fatal(err)
		}
		if err := e.query("SELECT row_to_json(cross_checks)::text FROM cross_checks WHERE id=$1", ccID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return r + i + c
	}
	review := func() apitypes.PolicyStopReview {
		before := snapshot()
		rec := call(e.owner, holdToken)
		if rec.Code != 200 {
			t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body)
		}
		if snapshot() != before {
			t.Fatal("preview mutated rows")
		}
		var body struct {
			Review apitypes.PolicyStopReview `json:"review"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Review
	}
	first := review()
	if len(first.Manifest.HumanFeedback) != 1 || first.Manifest.HumanFeedback[0].Body == nil || *first.Manifest.HumanFeedback[0].Body != human || string(first.Manifest.HumanFeedback[0].InputID) != inputID ||
		first.Manifest.ReviewGeneration != reviewGen || first.Manifest.SourceGeneration != sourceGen || first.Manifest.Gate.Revision != gateRev ||
		len(first.Manifest.CrossCheckFeedback) != 1 || first.Manifest.CrossCheckFeedback[0].CrossCheckID != ccID.String() || first.Manifest.CrossCheckFeedback[0].CandidateGeneration != "2" || !first.Manifest.CrossCheckFeedback[0].Supersede {
		t.Fatal("mixed source manifest or decimal precision mismatch")
	}
	unchanged := first.StopIfMatch
	e.exec("UPDATE runs SET updated_at=now(),last_activity_at=now(),last_seq=last_seq+1 WHERE id=$1", id)
	if review().StopIfMatch != unchanged {
		t.Fatal("bookkeeping changed review token")
	}
	reject := func(owner store.User, token, reason string) {
		t.Helper()
		before := snapshot()
		rec := call(owner, token)
		if reason == "404" {
			if rec.Code != 404 {
				t.Fatalf("foreign owner status=%d", rec.Code)
			}
		} else if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"reason":"`+reason+`"`) {
			t.Fatalf("refusal status=%d body=%s", rec.Code, rec.Body)
		}
		if snapshot() != before {
			t.Fatal("refusal mutated rows")
		}
	}
	t.Run("paused_hold_identity_changes_token", func(t *testing.T) {
		e.exec("UPDATE runs SET status_since=status_since+interval '1 second' WHERE id=$1", id)
		defer e.exec("UPDATE runs SET status_since=status_since-interval '1 second' WHERE id=$1", id)
		if review().StopIfMatch == unchanged {
			t.Fatal("a new paused hold reused the old Stop confirmation")
		}
	})
	t.Run("hold_reason_changes_token", func(t *testing.T) {
		e.exec("UPDATE runs SET hold_reason='provider_unavailable' WHERE id=$1", id)
		defer e.exec("UPDATE runs SET hold_reason=$2 WHERE id=$1", id, held.HoldReason)
		if review().StopIfMatch == unchanged {
			t.Fatal("a changed hold reason reused the old Stop confirmation")
		}
	})
	t.Run("scope_ceiling_changes_token", func(t *testing.T) {
		e.exec("UPDATE runs SET scope_ceiling=3 WHERE id=$1", id)
		defer e.exec("UPDATE runs SET scope_ceiling=$2 WHERE id=$1", id, held.ScopeCeiling)
		if review().StopIfMatch == unchanged {
			t.Fatal("a changed accepted scope reused the old Stop confirmation")
		}
	})
	t.Run("ordinary_spent_hold_retains_preview", func(t *testing.T) {
		e.exec("UPDATE runs SET budget_wall_seconds=1,budget_paused_seconds=0,budget_extension_seconds=0,budget_finalize_seconds=0 WHERE id=$1", id)
		defer e.exec("UPDATE runs SET budget_wall_seconds=$2,budget_paused_seconds=$3,budget_extension_seconds=$4,budget_finalize_seconds=$5 WHERE id=$1", id, held.BudgetWallSeconds, held.BudgetPausedSeconds, held.BudgetExtensionSeconds, held.BudgetFinalizeSeconds)
		spent, err := e.race.GetRunByID(ctx, id)
		if err != nil || workersvc.PolicyOwnerBudgetAvailable(spent, params.RunTimeout) {
			t.Fatalf("control did not exhaust the served budget: %v", err)
		}
		rec := call(e.owner, holdToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("ordinary policy hold changed existing Stop budget admission: status=%d body=%s", rec.Code, rec.Body)
		}
	})
	t.Run("ownership_and_hold_token", func(t *testing.T) {
		reject(store.User{ID: uuid.New()}, holdToken, "404")
		reject(e.owner, "", "policy_stop_hold_token_required")
		reject(e.owner, "\"policy-old-1\"", "policy_stop_hold_token_stale")
		reject(e.owner, "\"policy-stop-v0-old\"", "policy_stop_hold_token_required")
	})
	t.Run("activation_defaults_off", func(t *testing.T) {
		off := params
		off.PolicyStopNegotiated = false
		service(off)
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		service(params)
	})
	t.Run("unknown_context", func(t *testing.T) {
		e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", id)
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE runs SET policy_revision_source=$2::jsonb WHERE id=$1", id, strings.Replace(source, `"version":1`, `"version":99`, 1))
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE runs SET policy_revision_source=$2::jsonb WHERE id=$1", id, source)
	})
	t.Run("ambiguous_unbound_input", func(t *testing.T) {
		var extra int64
		if err := pool.QueryRow(ctx, "INSERT INTO run_user_inputs(run_id,kind,body,gate_binding) VALUES($1,'revise_plan','ambiguous','unbound') RETURNING id", id).Scan(&extra); err != nil {
			t.Fatal(err)
		}
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("DELETE FROM run_user_inputs WHERE id=$1", extra)
	})
	t.Run("completed_membership", func(t *testing.T) {
		for _, bad := range []string{`["m1","m1"]`, `["foreign"]`} {
			e.exec("UPDATE runs SET milestones_completed=$2::jsonb WHERE id=$1", id, bad)
			reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		}
		e.exec(`UPDATE runs SET milestones_completed='["m1"]' WHERE id=$1`, id)
	})
	t.Run("exact_cc_not_latest", func(t *testing.T) {
		e.exec("UPDATE runs SET policy_revision_source=jsonb_set(policy_revision_source,'{associated_cross_checks,0,cross_check_id}',to_jsonb($2::text)) WHERE id=$1", id, uuid.NewString())
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE runs SET policy_revision_source=$2::jsonb WHERE id=$1", id, source)
		nonplan := uuid.New()
		e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,verdict,outcome,reason_class,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,created_at,deadline_at)
        VALUES($1,$2,'code',1,1,'failed','failed','snapshot_failed','{}','','',sha256(''::bytea),'',sha256(''::bytea),now(),now())`, nonplan, id)
		e.exec("UPDATE runs SET policy_revision_source=jsonb_set(policy_revision_source,'{associated_cross_checks,0,cross_check_id}',to_jsonb($2::text)) WHERE id=$1", id, nonplan.String())
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE runs SET policy_revision_source=$2::jsonb WHERE id=$1", id, source)
		other := e.providerHold(1)
		e.exec("UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", ccID, other)
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", ccID, id)
		e.exec("UPDATE cross_checks SET candidate_digest=$2 WHERE id=$1", ccID, make([]byte, 32))
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE cross_checks SET candidate_digest=$2 WHERE id=$1", ccID, ccDigest)
	})
	t.Run("historical_candidate_and_approved_witness", func(t *testing.T) {
		altered := candidate
		altered.PlanMd = "historical candidate"
		d, err := altered.Digest()
		if err != nil {
			t.Fatal(err)
		}
		e.exec("UPDATE cross_checks SET plan_md=$2,candidate_digest=$3 WHERE id=$1", ccID, altered.PlanMd, d)
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE cross_checks SET plan_md=$2,candidate_digest=$3 WHERE id=$1", ccID, candidate.PlanMd, ccDigest)
		e.exec("UPDATE cross_checks SET verdict='approve' WHERE id=$1", ccID)
		approved := review()
		if len(approved.Manifest.CrossCheckFeedback) != 1 || approved.Manifest.CrossCheckFeedback[0].Supersede || approved.Manifest.CrossCheckFeedback[0].Verdict != "approve" {
			t.Fatal("approved witness treated as superseded feedback")
		}
		e.exec("UPDATE cross_checks SET verdict='revise' WHERE id=$1", ccID)
	})
	t.Run("bound_history_and_primary_unbound_evidence", func(t *testing.T) {
		var old int64
		if err := pool.QueryRow(ctx, "INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision) VALUES($1,'revise_plan','history','bound',1) RETURNING id", id).Scan(&old); err != nil {
			t.Fatal(err)
		}
		if review().StopIfMatch != unchanged {
			t.Fatal("known bound history changed current cohort")
		}
		e.exec("DELETE FROM run_user_inputs WHERE id=$1", old)
		e.exec("UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", inputID)
		reject(e.owner, holdToken, "policy_stop_review_unverifiable")
		e.exec("UPDATE run_user_inputs SET consumed_at=now(),consumed_claim_generation=$2,consumed_worker_id=$3 WHERE id=$1", inputID, sourceGen, e.worker)
		if len(review().Manifest.HumanFeedback) != 1 {
			t.Fatal("proven primary omitted")
		}
		e.exec("UPDATE run_user_inputs SET gate_binding='bound',gate_revision=$2,consumed_at=NULL,consumed_claim_generation=NULL,consumed_worker_id=NULL WHERE id=$1", inputID, gateRev)
	})
	t.Run("contract_revision_changes_token", func(t *testing.T) {
		contract := `{"profile":"structural","revision":1,"criteria":[{"id":"m1.c1","milestone_id":"m1","text":"m1","audit":null,"finding_ids":[]},{"id":"m2.c1","milestone_id":"m2","text":"m2","audit":null,"finding_ids":[]},{"id":"m3.c1","milestone_id":"m3","text":"m3","audit":null,"finding_ids":[]}]}`
		e.exec("UPDATE runs SET completion_contract_version=1,contract_revision=1,completion_contract=$2::jsonb WHERE id=$1", id, contract)
		first := review()
		if first.Manifest.Execution.ContractRevision == nil || *first.Manifest.Execution.ContractRevision != "1" {
			t.Fatal("contract revision missing")
		}
		e.exec("UPDATE runs SET contract_revision=2,completion_contract=jsonb_set(completion_contract,'{revision}','2') WHERE id=$1", id)
		if review().StopIfMatch == first.StopIfMatch {
			t.Fatal("contract change did not change token")
		}
		e.exec("UPDATE runs SET completion_contract_version=NULL,contract_revision=NULL,completion_contract=NULL WHERE id=$1", id)
	})
	t.Run("new_input_and_body_change_token", func(t *testing.T) {
		var extra int64
		if err := pool.QueryRow(ctx, "INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision) VALUES($1,'revise_plan','new feedback','bound',$2) RETURNING id", id, gateRev).Scan(&extra); err != nil {
			t.Fatal(err)
		}
		if review().StopIfMatch == unchanged {
			t.Fatal("new input did not change token")
		}
		e.exec("DELETE FROM run_user_inputs WHERE id=$1", extra)
		e.exec("UPDATE run_user_inputs SET body=$2 WHERE id=$1", inputID, human+" ")
		if review().StopIfMatch == unchanged {
			t.Fatal("body bytes did not change token")
		}
		e.exec("UPDATE run_user_inputs SET body=$2 WHERE id=$1", inputID, human)
	})
	t.Run("gate_identity_changes_token", func(t *testing.T) {
		next := uuid.NewString()
		e.exec("UPDATE runs SET gate_presentation_id=$2::uuid,policy_revision_source=jsonb_set(policy_revision_source,'{gate,presentation_id}',to_jsonb(($2::uuid)::text)) WHERE id=$1", id, next)
		if review().StopIfMatch == unchanged {
			t.Fatal("gate change did not change token")
		}
		e.exec("UPDATE runs SET gate_presentation_id=$2,policy_revision_source=$3::jsonb WHERE id=$1", id, presentation, source)
	})
	if review().StopIfMatch != unchanged {
		t.Fatal("restored manifest token changed")
	}
}

// This route-level positive control was first run against the baseline router.
func TestPolicyStopReviewRoute(t *testing.T) {
	h := &Handler{}
	ls := newProbeLimiters()
	router := h.Routes(ls[0], ls[1], ls[2], ls[3], ls[4], ls[5], ls[6], ls[7], ls[8], ls[9], ls[10])
	r := httptest.NewRequest(http.MethodGet, "/api/runs/00000000-0000-0000-0000-000000000001/policy-stop-review", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("preview GET route is absent: status=%d", rec.Code)
	}
	found := false
	err := chi.Walk(router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet && route == "/api/runs/{id}/policy-stop-review" {
			found = true
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("owner GET missing from router: found=%v err=%v", found, err)
	}
}
