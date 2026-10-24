package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type policyCancelSnapshot func(*testing.T) map[string]json.RawMessage

// Configure the existing authenticated Routes fixture explicitly: newSettleEnv's
// zero heartbeat threshold cannot exercise hasLivePoller's delivery branch.
// Stop remains off. This is API/Postgres proof, with no provider or RunRunner.
func policyCancelConfigureResume(t *testing.T, e *settleEnv) {
	t.Helper()
	q := store.New(e.pool)
	box := newHandlerTestBox(t)
	e.wsvc = workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), WorkerHeartbeatStale: time.Hour,
		WorkerHeartbeatInterval: time.Minute, ActiveSnapshotMaxEntries: 8,
		WorkerOutboxMaxPending: 8, TerminalPendingLease: time.Hour,
		PolicyHoldNegotiated: true, PolicyHoldSnapshotEnabled: true,
	})
	e.wsvc.SetTxBeginner(e.pool)
	e.wsvc.SetForges(settleForgeBuilder{f: e.fake})
	h := &Handler{pool: e.pool, q: q, box: box, wsvc: e.wsvc,
		cfg:      config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour},
		settings: settings.New(q, time.Minute)}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.workerA,
		[]string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1})
}

// Each route call has one attempt and a ten-second context; no retry, goroutine
// or readiness sleep. The enclosing serial subtests continue after sibling failure.
func policyCancelRoute(t *testing.T, e *settleEnv, token, method, path, body, match string, status int) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if match != "" {
		req.Header.Set("If-Match", match)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if ctx.Err() != nil || rec.Code != status {
		t.Fatalf("route %s %s: status=%d want=%d context=%v response=%s", method, path, rec.Code, status, ctx.Err(), rec.Body.String())
	}
	return rec
}

func policyCancelRunColumns(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func policyCancelPreserveResume(t *testing.T, e *settleEnv, token string, snapshot policyCancelSnapshot) {
	t.Helper()
	before := snapshot(t)
	held, err := store.New(e.pool).GetRunByID(e.ctx, e.run)
	if err != nil {
		t.Fatal(err)
	}
	match := workersvc.PolicyHoldActionToken(held)
	path := "/api/runs/" + e.run.String() + "/resume-now"
	body := `{"mode":"preserve","guidance":"keep remaining scope"}`
	policyCancelRoute(t, e, token, http.MethodPost, path, body, match+"-stale", http.StatusConflict)
	policyCancelEqualJSON(t, "stale owner resume", before, snapshot(t))
	policyCancelRoute(t, e, token, http.MethodPost, path, body, match, http.StatusOK)
	after := snapshot(t)
	prior := policyCancelRunColumns(t, before["run"])
	current := policyCancelRunColumns(t, after["run"])
	if string(current["status"]) != `"queued"` || string(current["policy_fresh_claim_generation"]) != "null" ||
		string(current["session_id"]) != `"preserved-cancel-thread"` {
		t.Fatal("preserve resume did not queue with original session and no fresh grant")
	}
	resumed, err := store.New(e.pool).GetRunByID(e.ctx, e.run)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.BudgetPausedSeconds < held.BudgetPausedSeconds || resumed.CodexClaimEpoch != held.CodexClaimEpoch+1 {
		t.Fatal("resume did not bank pause or invalidate the prior Codex claim epoch")
	}
	// All columns besides these documented ResumePolicyHold writes are exact,
	// including owner/worker/generation, policy source, gate, frozen/completed scope.
	for _, key := range []string{"status", "status_since", "budget_paused_seconds", "codex_cap_hash", "codex_claim_epoch", "updated_at"} {
		delete(prior, key)
		delete(current, key)
	}
	policyCancelEqualJSON(t, "resume preserved run", prior, current)
	for _, key := range []string{"custody", "leases"} {
		policyCancelEqualJSON(t, "resume "+key, before[key], after[key])
	}
	var inputs []struct {
		Kind string `json:"kind"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(after["inputs"], &inputs); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 3 || inputs[0].Kind != "follow_up" || inputs[0].Body != "preserved cancel guidance" ||
		inputs[1].Kind != "follow_up" || inputs[1].Body != "keep remaining scope" ||
		inputs[2].Kind != "resume" || inputs[2].Body != "policy hold: preserve conversation" {
		t.Fatal("resume lost guidance or did not append exactly one preserve audit")
	}
	policyCancelRoute(t, e, token, http.MethodPost, path, body, match, http.StatusConflict)
	policyCancelEqualJSON(t, "duplicate owner resume", after, snapshot(t))
}

func policyCancelActiveSnapshot(t *testing.T, e *settleEnv, snapshot policyCancelSnapshot) {
	t.Helper()
	before := snapshot(t)
	// Claim acquisition is SEEDED, not a ClaimRun/RunRunner observation. Model the
	// next unreleased active claim of the same owner and session, without granting
	// fresh consent or changing source/scope/custody. Heartbeat acceptance below
	// must actually persist its snapshot before the CLI can send Cancel.
	e.exec("UPDATE runs SET status='running',claim_generation=3,status_since=now() WHERE id=$1", e.run)
	e.exec("UPDATE workers SET snapshot_register_nonce='cancel-fixture',snapshot_epoch=0 WHERE id=$1", e.workerA)
	body := `{"active_snapshot":{"register_nonce":"cancel-fixture","snapshot_epoch":1,"active":[{"run_id":"` +
		e.run.String() + `","claim_generation":3,"phase":"running","terminal_pending":false}]}}`
	policyCancelRoute(t, e, e.tokenA, http.MethodPost, "/api/worker/heartbeat", body, "", http.StatusOK)
	var accepted bool
	if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS(
 SELECT 1 FROM workers w JOIN worker_active_runs a ON a.worker_id=w.id
 JOIN runs r ON r.id=a.run_id
 WHERE w.id=$1 AND a.run_id=$2 AND w.snapshot_epoch=1 AND a.snapshot_epoch=1
 AND w.last_heartbeat_at >= now()-interval '1 hour' AND a.claim_generation=3
 AND a.phase='running' AND NOT a.terminal_pending AND a.terminal_pending_until IS NULL
 AND r.status='running' AND r.worker_id=w.id AND r.claim_generation=3 AND r.claim_released_at IS NULL)`,
		e.workerA, e.run).Scan(&accepted); err != nil || !accepted {
		t.Fatalf("authenticated heartbeat did not persist active claim snapshot: accepted=%v err=%v", accepted, err)
	}
	after := snapshot(t)
	prior := policyCancelRunColumns(t, before["run"])
	current := policyCancelRunColumns(t, after["run"])
	for _, key := range []string{"status", "status_since", "claim_generation"} {
		delete(prior, key)
		delete(current, key)
	}
	policyCancelEqualJSON(t, "seeded next claim and heartbeat preserved run", prior, current)
	for _, key := range []string{"custody", "inputs"} {
		policyCancelEqualJSON(t, "active snapshot preserved "+key, before[key], after[key])
	}
}

func policyCancelDeliverAndTerminate(t *testing.T, e *settleEnv, before map[string]json.RawMessage, snapshot policyCancelSnapshot) {
	t.Helper()
	sent := snapshot(t)
	prior := policyCancelRunColumns(t, before["run"])
	current := policyCancelRunColumns(t, sent["run"])
	if string(current["status"]) != `"running"` || string(current["finished_at"]) != "null" ||
		string(current["stop_kind"]) != `"cancelled"` {
		t.Fatal("live Cancel did not stamp cancellation without fabricating terminal completion")
	}
	for _, key := range []string{"stop_kind", "stop_reason", "updated_at"} {
		delete(prior, key)
		delete(current, key)
	}
	policyCancelEqualJSON(t, "live Cancel enqueue preserved run", prior, current)
	for _, key := range []string{"custody", "leases"} {
		policyCancelEqualJSON(t, "live enqueue "+key, before[key], sent[key])
	}
	path := "/api/worker/runs/" + e.run.String()
	rec := policyCancelRoute(t, e, e.tokenA, http.MethodGet, path+"/inputs?claim_generation=3", "", "", http.StatusOK)
	var delivered struct {
		Inputs []workersvc.InputDTO `json:"inputs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delivered); err != nil {
		t.Fatal(err)
	}
	if len(delivered.Inputs) != 3 || delivered.Inputs[0].Kind != "follow_up" ||
		delivered.Inputs[1].Kind != "follow_up" || delivered.Inputs[2].Kind != "cancel" ||
		delivered.Inputs[0].Body == nil || *delivered.Inputs[0].Body != "preserved cancel guidance" ||
		delivered.Inputs[1].Body == nil || *delivered.Inputs[1].Body != "keep remaining scope" {
		t.Fatal("authenticated poll did not deliver preserved guidance and Cancel in FIFO order, excluding the server-only resume audit")
	}
	var settled int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND consumed_at IS NOT NULL AND applied_at IS NOT NULL", e.run).Scan(&settled); err != nil || settled != 3 {
		t.Fatalf("legacy authenticated poll did not commit three consumed/applied rows: count=%d err=%v", settled, err)
	}
	polled := snapshot(t)
	policyCancelInputChanges(t, before["inputs"], sent["inputs"], polled["inputs"])
	policyCancelEqualJSON(t, "input delivery preserved run", sent["run"], polled["run"])
	policyCancelRoute(t, e, e.tokenA, http.MethodPost, path+"/state",
		`{"status":"failed","claim_generation":2,"failure_reason":"late old flight"}`, "", http.StatusConflict)
	policyCancelEqualJSON(t, "stale terminal report", polled, snapshot(t))
	// A real worker reports failed on cancellation; SetState must select
	// CancelRunByWorker from the committed stop_kind instead of agent failure.
	policyCancelRoute(t, e, e.tokenA, http.MethodPost, path+"/state",
		`{"status":"failed","claim_generation":3,"failure_reason":"cancel observed"}`, "", http.StatusOK)
	terminal := snapshot(t)
	run := policyCancelRunColumns(t, terminal["run"])
	if string(run["status"]) != `"cancelled"` || string(run["fail_origin"]) != "null" || string(run["failure_reason"]) != "null" {
		t.Fatal("worker terminal report lost Cancel precedence to agent failure")
	}
	policyCancelEqualJSON(t, "terminal handling kept delivered inputs", polled["inputs"], terminal["inputs"])
	policyCancelRoute(t, e, e.tokenA, http.MethodPost, path+"/state",
		`{"status":"failed","claim_generation":3,"failure_reason":"duplicate"}`, "", http.StatusConflict)
	policyCancelEqualJSON(t, "duplicate worker terminal", terminal, snapshot(t))
}

func policyCancelInputChanges(t *testing.T, before, sent, polled json.RawMessage) {
	t.Helper()
	var original, enqueued, consumed []map[string]json.RawMessage
	for _, item := range []struct {
		raw json.RawMessage
		dst *[]map[string]json.RawMessage
	}{{before, &original}, {sent, &enqueued}, {polled, &consumed}} {
		if err := json.Unmarshal(item.raw, item.dst); err != nil {
			t.Fatal(err)
		}
	}
	if len(original) != 3 || len(enqueued) != 4 || len(consumed) != 4 ||
		string(enqueued[3]["kind"]) != `"cancel"` || string(enqueued[3]["body"]) != "null" {
		t.Fatal("live enqueue did not append exactly one plain Cancel")
	}
	policyCancelEqualJSON(t, "enqueue kept original input rows", original, enqueued[:3])
	// ConsumeRunInputs changes only delivery timestamps on the two follow-ups
	// and Cancel; the resume audit is server-only and remains entirely unchanged.
	for i := range enqueued {
		if string(enqueued[i]["kind"]) != `"resume"` {
			if string(consumed[i]["consumed_at"]) == "null" || string(consumed[i]["applied_at"]) == "null" {
				t.Fatal("delivered row has no committed consumption/application timestamp")
			}
			for _, key := range []string{"consumed_at", "applied_at"} {
				delete(enqueued[i], key)
				delete(consumed[i], key)
			}
		}
		policyCancelEqualJSON(t, "poll preserved input row", enqueued[i], consumed[i])
	}
}

func policyCancelLateResumeRejected(t *testing.T, e *settleEnv, terminal map[string]json.RawMessage, snapshot policyCancelSnapshot) {
	t.Helper()
	run, err := store.New(e.pool).GetRunByID(e.ctx, e.run)
	if err != nil {
		t.Fatal(err)
	}
	// Metadata-free report uses the still-owned exact claim. Terminal disposition
	// retires no custody here and must not resurrect a policy hold or metadata.
	outcome := uuid.NewString()
	body, err := json.Marshal(workersvc.PolicyHoldRequest{
		OutcomeID: outcome, SourceGeneration: run.ClaimGeneration,
		ClaimGeneration: run.ClaimGeneration, Detail: "resume_rejected",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Repeat the identical immutable outcome once: both replies are real route ACKs.
	for attempt := 0; attempt < 2; attempt++ {
		rec := policyCancelRoute(t, e, e.tokenA, http.MethodPost,
			"/api/worker/runs/"+e.run.String()+"/policy-hold", string(body), "", http.StatusOK)
		var ack policyHoldAck
		if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Disposition != workersvc.PolicyHoldTerminal || ack.Status != "cancelled" || ack.OutcomeID != outcome ||
			ack.ClaimGeneration != run.ClaimGeneration {
			t.Fatal("late resume rejection did not acknowledge terminal cancellation")
		}
		policyCancelEqualJSON(t, "late metadata-free resume rejection", terminal, snapshot(t))
	}
}
