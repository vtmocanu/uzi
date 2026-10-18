package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// These are API transaction proofs. Source context is deliberately seeded here;
// this fixture does not prove worker delivery, source pinning or finalization.
type policyStopConfirmEnv struct {
	*wallParkExtendHTTPEnv
	id         uuid.UUID
	humanID    int64
	ccID       uuid.UUID
	holdToken  string
	setFeature func(bool)
}

func newPolicyStopConfirmEnv(t *testing.T, completed int, verdict string) *policyStopConfirmEnv {
	t.Helper()
	return newPolicyStopConfirmFixture(t, completed, verdict, true)
}

// No feedback is inserted in this variant. The source is a seeded V1 fixture,
// not evidence of runtime source authority.
func newPolicyStopConfirmNoFeedbackEnv(t *testing.T) *policyStopConfirmEnv {
	t.Helper()
	return newPolicyStopConfirmFixture(t, 1, "", false)
}

func newPolicyStopConfirmFixture(t *testing.T, completed int, verdict string, feedback bool) *policyStopConfirmEnv {
	t.Helper()
	e := newWallParkExtendHTTPEnv(t, 57600)
	ctx := context.Background()
	pool, err := store.OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &policyStopConfirmEnv{wallParkExtendHTTPEnv: e, id: e.providerHold(completed), ccID: uuid.New()}
	f.setFeature = func(enabled bool) {
		e.h.wsvc = workersvc.New(e.race, nil, workersvc.Params{
			PolicyStopNegotiated: enabled, PolicyHoldNegotiated: true,
			PolicyHoldSnapshotEnabled: true, TerminalPendingLease: time.Hour, RunTimeout: 16 * time.Hour,
		})
		e.h.wsvc.SetTxBeginner(pool)
	}
	f.setFeature(true)
	presentation := uuid.New()
	payload := `{"plan_md":"exact candidate\n","milestones":[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"},{"id":"m3","title":"m3"}],"required_capabilities":[],"required_tools":[],"size_class":"s"}`
	digest := sha256.Sum256([]byte(payload))
	e.exec(`UPDATE runs SET harness='codex',hold_reason='provider_policy_refusal',policy_refusal_count=1,
 policy_last_outcome_id=$2,policy_last_source_generation=1,claim_generation=1,session_id='preserved-test-thread',
 policy_last_tag='cyberPolicy',policy_last_phase='planning',policy_last_correlation_id=$6,policy_last_refused_at=now(),
 gate_revision=3,gate_presentation_id=$3,gate_presented_payload=$4::jsonb,gate_payload_digest=$5,
 plan_md=E'exact candidate\n',milestones_candidate=milestones_frozen,size_class='s',
 started_at=status_since-interval '1 minute',budget_wall_seconds=57600 WHERE id=$1`,
		f.id, uuid.New(), presentation, payload, digest[:], "pr-"+strings.Repeat("a", 32)+"-1")
	if feedback {
		if err := e.query(`INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision,consumed_at,consumed_claim_generation,consumed_worker_id)
 VALUES($1,'revise_plan',$2,'bound',3,now(),1,$3) RETURNING id`, f.id, "  exact human body\n\n", f.worker).Scan(&f.humanID); err != nil {
			t.Fatal(err)
		}
		source := fmt.Sprintf(`{"version":1,"gate":{"revision":"3","presentation_id":"%s","payload_digest":"%s"},"primary_revision":{"source":"run_user_input","input_id":"%d"},"associated_cross_checks":[{"source":"plan_cross_check","cross_check_id":"%s"}]}`,
			presentation, hex.EncodeToString(digest[:]), f.humanID, f.ccID)
		e.exec("UPDATE runs SET policy_revision_source=$2::jsonb WHERE id=$1", f.id, source)
		frozen := json.RawMessage(`[{"id":"m1","title":"m1"},{"id":"m2","title":"m2"},{"id":"m3","title":"m3"}]`)
		candidate := workersvc.PlanCrossCheckCandidate{PlanMd: "exact candidate\n", Milestones: frozen,
			RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
			BaseCommit: strings.Repeat("a", 40), PlanningDiff: "exact diff\n"}
		ccDigest, err := candidate.Digest()
		if err != nil {
			t.Fatal(err)
		}
		e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
 required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,findings,
 created_at,decided_at,deadline_at,checker_harness,checker_model,checker_effort,automatic_rounds_enabled,automatic_revision_limit)
 VALUES($1,$2,'plan',1,1,$3,$4::jsonb,'{}','{}','s',$5,$6,$7,$8,
 '[{"body":"exact finding\n"}]',now(),now(),now()+interval '1 hour','claude','checker-model','high',true,2)`,
			f.ccID, f.id, candidate.PlanMd, string(frozen), candidate.BaseCommit, candidate.PlanningDiff, ccDigest, verdict)
	} else {
		source := fmt.Sprintf(`{"version":1,"gate":{"revision":"3","presentation_id":"%s","payload_digest":"%s"},"primary_revision":null,"associated_cross_checks":[]}`,
			presentation, hex.EncodeToString(digest[:]))
		e.exec("UPDATE runs SET policy_revision_source=$2::jsonb WHERE id=$1", f.id, source)
	}
	r, err := e.race.GetRunByID(ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	f.holdToken = workersvc.PolicyHoldActionToken(r)
	return f
}

func (f *policyStopConfirmEnv) scalar(sql string, args ...any) string {
	f.t.Helper()
	var v string
	if err := f.query(sql, args...).Scan(&v); err != nil {
		f.t.Fatal(err)
	}
	return v
}

// Snapshot the entire run and complete child inventories, including audit rows.
// Each comparison's baseline is taken after any intentional external mutation.
func (f *policyStopConfirmEnv) snapshot() string {
	f.t.Helper()
	return f.scalar(`SELECT jsonb_build_object(
 'run',to_jsonb(r),
 'inputs',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.id),'[]'::jsonb) FROM run_user_inputs i WHERE i.run_id=r.id),
 'checks',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.id),'[]'::jsonb) FROM cross_checks c WHERE c.lead_run_id=r.id))::text
 FROM runs r WHERE r.id=$1`, f.id)
}

func (f *policyStopConfirmEnv) review() apitypes.PolicyStopReview {
	f.t.Helper()
	before := f.snapshot()
	req := inputReq(f.owner, f.id, "")
	req.Method = http.MethodGet
	req.Header.Set("If-Match", f.holdToken)
	rec := httptest.NewRecorder()
	f.h.PolicyStopReview(rec, req)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("review status=%d body=%s", rec.Code, rec.Body)
	}
	var response struct {
		Review apitypes.PolicyStopReview `json:"review"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		f.t.Fatal(err)
	}
	if f.snapshot() != before {
		f.t.Fatal("GET review changed database")
	}
	if !strings.HasPrefix(response.Review.StopIfMatch, `"policy-stop-v1-`) || response.Review.StopIfMatch == f.holdToken {
		f.t.Fatalf("review did not issue distinct Stop consent: %q", response.Review.StopIfMatch)
	}
	return response.Review
}

func (f *policyStopConfirmEnv) stop(owner store.User, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := inputReq(owner, f.id, `{"kind":"stop","body":"keep completed work"}`)
	if token != "" {
		req.Header.Set("If-Match", token)
	}
	rec := httptest.NewRecorder()
	f.h.CreateRunInput(rec, req)
	return rec
}

func (f *policyStopConfirmEnv) accept(token string) apitypes.PolicyStopReceipt {
	f.t.Helper()
	rec := f.stop(f.owner, token)
	if rec.Code != http.StatusAccepted {
		f.t.Fatalf("stop status=%d body=%s", rec.Code, rec.Body)
	}
	var response apitypes.RunInputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		f.t.Fatal(err)
	}
	if response.PolicyStop == nil {
		f.t.Fatalf("missing receipt: %s", rec.Body)
	}
	var wire struct {
		Receipt struct {
			ScopeInputID     string `json:"scope_input_id"`
			ReviewGeneration string `json:"review_generation"`
			Refs             []struct {
				Source       string  `json:"source"`
				InputID      *string `json:"input_id"`
				CrossCheckID *string `json:"cross_check_id"`
			} `json:"superseded_revisions"`
		} `json:"policy_stop"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		f.t.Fatalf("receipt IDs must be wire strings: %v; body=%s", err, rec.Body)
	}
	if wire.Receipt.ScopeInputID != string(response.PolicyStop.ScopeInputID) || wire.Receipt.ReviewGeneration != string(response.PolicyStop.ReviewGeneration) {
		f.t.Fatalf("receipt wire IDs differ: %s", rec.Body)
	}
	for _, ref := range wire.Receipt.Refs {
		if (ref.Source == "run_user_input" && (ref.InputID == nil || ref.CrossCheckID != nil)) ||
			(ref.Source == "plan_cross_check" && (ref.CrossCheckID == nil || ref.InputID != nil)) {
			f.t.Fatalf("receipt ref crossed ID namespaces: %s", rec.Body)
		}
	}
	return *response.PolicyStop
}

func (f *policyStopConfirmEnv) reject(owner store.User, token string, status int, reason string) {
	f.t.Helper()
	before := f.snapshot()
	rec := f.stop(owner, token)
	if rec.Code != status {
		f.t.Fatalf("status=%d want=%d body=%s", rec.Code, status, rec.Body)
	}
	if reason != "" {
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			f.t.Fatal(err)
		}
		if body.Reason != reason {
			f.t.Fatalf("reason=%q want=%q body=%s", body.Reason, reason, rec.Body)
		}
	}
	if f.snapshot() != before {
		f.t.Fatal("rejected Stop mutated run, feedback or audit rows")
	}
}

func TestPolicyStopConfirmAcceptLiveDB(t *testing.T) {
	for _, tc := range []struct {
		completed int
		verdict   string
	}{{1, "revise"}, {3, "revise"}, {1, "approve"}} {
		t.Run(fmt.Sprintf("completed_%d_cc_%s", tc.completed, tc.verdict), func(t *testing.T) {
			f := newPolicyStopConfirmEnv(t, tc.completed, tc.verdict)
			review := f.review()
			// Compare every original feedback field except the three settlement fields.
			humanSQL := `SELECT (to_jsonb(i)-ARRAY['disposition','settled_at','superseded_by_stop_input_id'])::text FROM run_user_inputs i WHERE id=$1`
			ccSQL := `SELECT (to_jsonb(c)-ARRAY['revision_disposition','revision_settled_at','revision_superseded_by_stop_input_id'])::text FROM cross_checks c WHERE id=$1`
			humanBefore, ccBefore := f.scalar(humanSQL, f.humanID), f.scalar(ccSQL, f.ccID)
			invariantSQL := `SELECT jsonb_build_array(milestones_frozen,milestones_candidate,plan_md,gate_revision,
 gate_presentation_id,gate_presented_payload,encode(gate_payload_digest,'hex'),policy_revision_source,
 policy_last_outcome_id,policy_last_source_generation,claim_generation)::text FROM runs WHERE id=$1`
			invariantBefore := f.scalar(invariantSQL, f.id)
			receipt := f.accept(review.StopIfMatch)
			if receipt.Disposition != "accepted" || receipt.ScopeCeiling != tc.completed || receipt.ReviewGeneration != "1" || receipt.AcceptedAt.IsZero() {
				t.Fatalf("bad acceptance receipt: %+v", receipt)
			}
			if _, err := uuid.Parse(receipt.ResolutionID); err != nil {
				t.Fatalf("resolution ID: %v", err)
			}
			if err := receipt.ScopeInputID.Validate(); err != nil {
				t.Fatalf("scope ID is not canonical decimal: %v", err)
			}
			wantRefs := []apitypes.PolicyRevisionRef{{Source: "run_user_input", InputID: func() *apitypes.PolicyStopDecimal {
				v := apitypes.PolicyStopDecimal(strconv.FormatInt(f.humanID, 10))
				return &v
			}()}}
			if tc.verdict == "revise" {
				id := f.ccID.String()
				wantRefs = append(wantRefs, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id})
			}
			if !reflect.DeepEqual(receipt.SupersededRevisions, wantRefs) {
				t.Fatalf("receipt refs=%+v want=%+v", receipt.SupersededRevisions, wantRefs)
			}
			r, err := f.race.GetRunByID(context.Background(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != "queued" || !r.ScopeCeiling.Valid || int(r.ScopeCeiling.Int32) != tc.completed ||
				r.SessionID.String != "preserved-test-thread" || r.PolicyFreshClaimGeneration.Valid || r.PolicyRefusalCount != 1 {
				t.Fatalf("Stop failed preserve/queue contract: %+v", r)
			}
			if f.scalar(invariantSQL, f.id) != invariantBefore || f.scalar(humanSQL, f.humanID) != humanBefore || f.scalar(ccSQL, f.ccID) != ccBefore {
				t.Fatal("Stop rewrote original source, gate, candidate, feedback or provenance")
			}
			scopeID, err := strconv.ParseInt(string(receipt.ScopeInputID), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			var durableReceipt bool
			if err := f.query(`SELECT i.created_at=$3 AND r.policy_stop_resolution->>'resolution_id'=$4
 AND (r.policy_stop_resolution->>'scope_input_id')::bigint=i.id
 AND (r.policy_stop_resolution->>'accepted_at')::timestamptz=i.created_at
 FROM run_user_inputs i JOIN runs r ON r.id=i.run_id WHERE i.id=$1 AND r.id=$2`,
				scopeID, f.id, receipt.AcceptedAt, receipt.ResolutionID).Scan(&durableReceipt); err != nil {
				t.Fatal(err)
			}
			if !durableReceipt {
				t.Fatal("receipt differs from durable scope/resolution identity or acceptance time")
			}
			var humanSettled, ccSettled bool
			if err := f.query(`SELECT disposition='superseded_by_stop' AND settled_at IS NOT NULL
 AND superseded_by_stop_input_id=$2 AND applied_at IS NULL AND included_at IS NULL
 FROM run_user_inputs WHERE id=$1`, f.humanID, scopeID).Scan(&humanSettled); err != nil {
				t.Fatal(err)
			}
			ccPredicate := `revision_disposition='superseded_by_stop' AND revision_settled_at IS NOT NULL AND revision_superseded_by_stop_input_id=$2`
			if tc.verdict == "approve" {
				ccPredicate = `revision_disposition IS NULL AND revision_settled_at IS NULL AND revision_superseded_by_stop_input_id IS NULL AND $2::bigint>0`
			}
			if err := f.query("SELECT "+ccPredicate+" FROM cross_checks WHERE id=$1", f.ccID, scopeID).Scan(&ccSettled); err != nil {
				t.Fatal(err)
			}
			if !humanSettled || !ccSettled {
				t.Fatal("incorrect human/CC settlement or approve witness retirement")
			}
			var scopes, resumes int
			if err := f.query(`SELECT count(*) FILTER(WHERE kind='scope'),count(*) FILTER(WHERE kind='resume') FROM run_user_inputs WHERE run_id=$1`, f.id).Scan(&scopes, &resumes); err != nil {
				t.Fatal(err)
			}
			if scopes != 1 || resumes != 1 {
				t.Fatalf("scope/resume audits=%d/%d", scopes, resumes)
			}
			if f.scalar("SELECT run_id::text FROM run_user_inputs WHERE id=$1 AND kind='scope'", scopeID) != f.id.String() {
				t.Fatal("receipt points outside this run's scope namespace")
			}
			claim, err := f.race.GetRunClaimContext(context.Background(), f.id)
			if err != nil {
				t.Fatal(err)
			}
			if claim.HumanPlanApproved || r.AutoApprove || r.PlanSource == "seeded" {
				t.Fatal("Stop manufactured plan approval")
			}
		})
	}
}

func TestPolicyStopConfirmUnreceivedCohortLiveDB(t *testing.T) {
	f := newPolicyStopConfirmEnv(t, 1, "revise")
	var pendingID int64
	if err := f.query(`INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision)
    VALUES($1,'revise_plan','not delivered yet','bound',3) RETURNING id`, f.id).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	review := f.review()
	if len(review.Manifest.HumanFeedback) != 2 {
		t.Fatal("review omitted pending feedback")
	}
	receipt := f.accept(review.StopIfMatch)
	if len(receipt.SupersededRevisions) != 3 {
		t.Fatal("receipt omitted an applicable human or automatic revision")
	}
	var truthful bool
	if err := f.query(`SELECT consumed_at IS NULL AND consumed_claim_generation IS NULL AND consumed_worker_id IS NULL
    AND applied_at IS NULL AND included_at IS NULL AND settled_at IS NOT NULL
    AND disposition='superseded_by_stop' AND superseded_by_stop_input_id=$2
    FROM run_user_inputs WHERE id=$1`, pendingID, string(receipt.ScopeInputID)).Scan(&truthful); err != nil {
		t.Fatal(err)
	}
	if !truthful {
		t.Fatal("Stop invented delivery/application or omitted settlement")
	}
}

func TestPolicyStopConfirmStaleLiveDB(t *testing.T) {
	for _, change := range []string{"new_bound_feedback", "hold_episode", "scope_ceiling"} {
		t.Run(change, func(t *testing.T) {
			f := newPolicyStopConfirmEnv(t, 1, "revise")
			review := f.review()
			switch change {
			case "new_bound_feedback":
				f.exec(`INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision) VALUES($1,'revise_plan','new feedback','bound',3)`, f.id)
			case "hold_episode":
				f.exec("UPDATE runs SET status_since=status_since+interval '1 second' WHERE id=$1", f.id)
			case "scope_ceiling":
				f.exec("UPDATE runs SET scope_ceiling=2 WHERE id=$1", f.id)
			}
			f.reject(f.owner, review.StopIfMatch, http.StatusConflict, "policy_stop_token_stale")
		})
	}
}

func TestPolicyStopConfirmRejectedLiveDB(t *testing.T) {
	for _, scenario := range []string{"missing_token", "wrong_token", "old_hold_token", "wrong_owner", "source_unknown", "feature_off", "unknown_legacy_stop"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPolicyStopConfirmEnv(t, 1, "revise")
			review := f.review()
			token, owner, status, reason := review.StopIfMatch, f.owner, http.StatusConflict, "policy_stop_token_stale"
			switch scenario {
			case "missing_token":
				token = ""
				reason = "policy_stop_token_required"
			case "wrong_token":
				token = `"policy-stop-v1-` + strings.Repeat("0", 64) + `"`
			case "old_hold_token":
				token = f.holdToken
			case "wrong_owner":
				owner = store.User{ID: uuid.New()}
				status = http.StatusNotFound
				reason = ""
			case "source_unknown":
				f.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", f.id)
				reason = "policy_stop_review_unverifiable"
			case "feature_off":
				f.setFeature(false)
				reason = "policy_stop_review_unverifiable"
			case "unknown_legacy_stop":
				f.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", f.id)
				token = ""
				reason = "policy_stop_token_required"
			}
			f.reject(owner, token, status, reason)
		})
	}
}

func TestPolicyStopConfirmAcceptedReplayLiveDB(t *testing.T) {
	f := newPolicyStopConfirmEnv(t, 1, "revise")
	review := f.review()
	original := f.accept(review.StopIfMatch)
	// Losing the first HTTP response and retrying must return its durable receipt.
	before := f.snapshot()
	repeated := f.accept(review.StopIfMatch)
	want := original
	want.Disposition = "already_accepted"
	if !reflect.DeepEqual(repeated, want) || f.snapshot() != before {
		t.Fatal("lost-response replay changed receipt or database")
	}
	// Metadata-only G+1 rehold fixture: this does not claim real worker delivery.
	f.exec(`UPDATE runs SET status='paused',hold_reason='provider_policy_refusal',claim_generation=2,
 policy_last_outcome_id=$2,status_since=now(),worker_id=$3 WHERE id=$1`, f.id, uuid.New(), f.worker)
	var pendingID int64
	if err := f.query(`INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision)
 VALUES($1,'revise_plan','new pending revision','bound',3) RETURNING id`, f.id).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	before = f.snapshot()
	repeated = f.accept(review.StopIfMatch)
	if !reflect.DeepEqual(repeated, want) || f.snapshot() != before {
		t.Fatal("accepted replay requeued or retired newer feedback after G+1")
	}
	var pending bool
	if err := f.query(`SELECT disposition IS NULL AND settled_at IS NULL AND superseded_by_stop_input_id IS NULL
 AND applied_at IS NULL AND included_at IS NULL FROM run_user_inputs WHERE id=$1`, pendingID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("accepted replay retired new pending revision")
	}
}
