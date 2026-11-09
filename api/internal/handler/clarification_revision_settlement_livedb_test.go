package handler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestClarificationRevisionSettlementOutcomeLiveDB (issue #3985) drives the REAL owner and worker
// routes through the clarification-during-human-revision flow of a gated plan cross-check run:
//
//	plan A presented -> human revise (bound) consumed and ACKed -> the lead asks Q (awaiting_input)
//	-> the owner answers Q -> the worker ACKs and APPLIES the answer.
//
// The generic running report is refused there (an answer never implies approval), and the
// worker's settlement report, read verbatim from fixtures/worker-protocol/clarification-settlement.json,
// must return the run to awaiting_approval with the revise still pending. The OUTCOME is asserted
// first, then the ACK; plan B then allocates the next gate revision, an approval bound to the old
// revision is refused and one bound to the new revision is applied by the next running report.
//
// Only wire JSON reaches the new behaviour: the settlement is a decoded fixture with values
// substituted, so a key renamed in the fixture or in the Go wire type reddens this test.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestClarificationRevisionSettlementOutcomeLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	raw, err := os.ReadFile("../../../fixtures/worker-protocol/clarification-settlement.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	var orig map[string]any
	if err := json.Unmarshal(raw, &orig); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	wantKeys := strings.Join(fixtureKeys(orig), ",")

	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), RunGateRefusalMax: 3, PlanMaxRevisions: 3, WorkerHeartbeatStale: time.Minute,
	})
	svc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: svc, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	ownerRouter := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	workerRouter := h.WorkerRoutes(mw.NewLimiter(100000, time.Minute, nil))

	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	owner, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner, fmt.Sprintf("clarsettle-%s@e2e", owner))
	cliMustExec(t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, uniq(repoID), fmt.Sprintf("g/clarsettle-%s", repoID))
	workerToken, tokenHash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, protocol_capabilities)
	      VALUES ($1, $2, $3, $4, 'online', now(), $5)`,
		workerID, owner, "clarsettle-"+workerID.String()[:8], tokenHash,
		[]string{capability.InputReceiptsV1, capability.GateRevisionV1, capability.PlanRevisionRetryV1})
	const gen = int64(1)
	cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
	        claim_generation, auto_approve, plan_source, session_id, plan_cross_check_gated)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, $6, false, 'agent', 'sess', true)`,
		runID, owner, repoID, uniq(runID), workerID, gen)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)

	runPath := "/api/runs/" + runID.String()
	workerPath := "/api/worker/runs/" + runID.String()
	workerCall := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, workerPath+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+workerToken)
		rec := httptest.NewRecorder()
		workerRouter.ServeHTTP(rec, req)
		var out map[string]any
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %s %s %q: %v", method, path, rec.Body.String(), err)
			}
		}
		return rec.Code, out
	}
	ownerPost := func(body string) (int, map[string]any) {
		t.Helper()
		rec := bearerReqBody(ownerRouter, http.MethodPost, runPath+"/inputs", uzc, body)
		var out map[string]any
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode owner input %q: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, out
	}
	mustCode := func(what string, got, want int, body map[string]any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: status %d, want %d: %v", what, got, want, body)
		}
	}
	inputID := func(kind string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `SELECT id FROM run_user_inputs WHERE run_id = $1 AND kind = $2 ORDER BY id DESC LIMIT 1`, runID, kind).Scan(&id); err != nil {
			t.Fatalf("read %s input: %v", kind, err)
		}
		return id
	}
	runStatus := func() (status string, openQuestion *string, paused int32) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, open_question_id, budget_paused_seconds FROM runs WHERE id = $1`, runID).Scan(&status, &openQuestion, &paused); err != nil {
			t.Fatal(err)
		}
		return
	}

	// 1-2. Plan A is presented: gate revision N = 1.
	planA := uuid.New()
	code, out := workerCall(http.MethodPost, "/state", fmt.Sprintf(`{"status":"awaiting_approval","plan_md":"# Plan A","session_id":"sess","claim_generation":%d,"presentation_id":%q}`, gen, planA))
	mustCode("plan A presentation", code, 200, out)
	if out["gate_revision"] != float64(1) {
		t.Fatalf("plan A gate_revision = %v, want 1", out["gate_revision"])
	}
	const gateN = int64(1)

	// 3. The human revise bound to N is consumed (replay) and ACKed by the worker.
	code, out = ownerPost(`{"kind":"revise_plan","body":"please split milestone two","expected_gate_revision":1}`)
	mustCode("owner revise", code, http.StatusAccepted, out)
	reviseID := inputID("revise_plan")
	code, out = workerCall(http.MethodGet, fmt.Sprintf("/inputs?claim_generation=%d", gen), "")
	mustCode("revise replay", code, 200, out)
	code, out = workerCall(http.MethodPost, "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":%d}`, reviseID, gen))
	mustCode("revise ack", code, 200, out)

	// 4. The lead asks a clarification: the run parks at awaiting_input on question Q.
	const questionID = "question-1"
	code, out = workerCall(http.MethodPost, "/state", fmt.Sprintf(`{"status":"awaiting_input","open_question_id":%q,"session_id":"sess","claim_generation":%d}`, questionID, gen))
	mustCode("question park", code, 200, out)

	// 5. The owner answers Q; the worker ACKs and APPLIES the answer.
	answerBody, _ := json.Marshal(map[string]any{"question_id": questionID, "answers": []string{"yes, split it"}})
	reqBody, _ := json.Marshal(map[string]any{"kind": "answer", "body": string(answerBody)})
	code, out = ownerPost(string(reqBody))
	mustCode("owner answer", code, http.StatusAccepted, out)
	answerID := inputID("answer")
	code, out = workerCall(http.MethodGet, fmt.Sprintf("/inputs?claim_generation=%d", gen), "")
	mustCode("answer replay", code, 200, out)
	code, out = workerCall(http.MethodPost, "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":%d}`, answerID, gen))
	mustCode("answer ack", code, 200, out)
	code, out = workerCall(http.MethodPost, "/inputs/applied", fmt.Sprintf(`{"ids":[%d],"claim_generation":%d}`, answerID, gen))
	mustCode("answer applied", code, 200, out)

	// 6. Make the awaiting_input gap observable, then snapshot every other column.
	cliMustExec(t, pool, `UPDATE runs SET status_since = now() - interval '5 seconds' WHERE id = $1`, runID)
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT (to_jsonb(runs) - ARRAY['status','status_since','updated_at','open_question_id','completion_question_at','budget_paused_seconds'])::text FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	status, openQ, pausedBefore := runStatus()
	if status != "awaiting_input" || openQ == nil || *openQ != questionID {
		t.Fatalf("precondition: status %q open_question_id %v, want awaiting_input on %q", status, openQ, questionID)
	}

	// 7. Negative control: the generic running report stays refused (an answer is not an approval).
	code, out = workerCall(http.MethodPost, "/state", fmt.Sprintf(`{"status":"running","session_id":"sess","claim_generation":%d}`, gen))
	mustCode("generic running", code, http.StatusConflict, out)
	if status, _, _ = runStatus(); status != "awaiting_input" {
		t.Fatalf("a refused running report left status %q, want awaiting_input", status)
	}

	// 8. The fixture settlement, with only the values substituted.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	settlement, ok := m["clarification_settlement"].(map[string]any)
	if !ok {
		t.Fatalf("fixture clarification_settlement is not an object: %T", m["clarification_settlement"])
	}
	m["claim_generation"] = gen
	settlement["question_id"] = questionID
	settlement["revision_input_id"] = reviseID
	settlement["gate_revision"] = gateN
	if got := strings.Join(fixtureKeys(m), ","); got != wantKeys {
		t.Fatalf("key set changed: %v, want %v", got, wantKeys)
	}
	settleBody, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	code, ack := workerCall(http.MethodPost, "/state", string(settleBody))

	// 9. OUTCOME first, so a baseline failure reports the wrong outcome.
	status, openQ, pausedAfter := runStatus()
	if status != "awaiting_approval" {
		t.Fatalf("outcome: status %q, want awaiting_approval (settlement response %d %v)", status, code, ack)
	}
	if openQ != nil {
		t.Fatalf("outcome: open_question_id %q, want NULL", *openQ)
	}
	if pausedAfter < pausedBefore+5 {
		t.Fatalf("outcome: budget_paused_seconds %d -> %d, want the awaiting_input gap (>= 5s) banked", pausedBefore, pausedAfter)
	}
	var reviseApplied, reviseDisposition *string
	if err := pool.QueryRow(ctx, `SELECT applied_at::text, disposition FROM run_user_inputs WHERE id = $1`, reviseID).Scan(&reviseApplied, &reviseDisposition); err != nil {
		t.Fatal(err)
	}
	if reviseApplied != nil || reviseDisposition != nil {
		t.Fatalf("outcome: revise applied_at %v disposition %v, want both NULL (the revise stays pending)", reviseApplied, reviseDisposition)
	}
	var approvals int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'approve_plan'`, runID).Scan(&approvals); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 {
		t.Fatalf("outcome: %d approve_plan rows exist, want none (an answer is not an approval)", approvals)
	}
	if after := snapshot(); after != before {
		t.Fatalf("outcome: columns outside the settlement set changed:\nbefore %s\nafter  %s", before, after)
	}

	// 10. Only then the ACK.
	mustCode("settlement", code, 200, ack)
	if ack["disposition"] != "clarification_settled" {
		t.Fatalf("settlement ack disposition = %v, want clarification_settled: %v", ack["disposition"], ack)
	}

	// 11. Plan B allocates N+1; an approval bound to N is refused, one bound to N+1 is applied.
	planB := uuid.New()
	code, out = workerCall(http.MethodPost, "/state", fmt.Sprintf(`{"status":"awaiting_approval","plan_md":"# Plan B","session_id":"sess","claim_generation":%d,"presentation_id":%q,"expected_gate_revision":%d,"revision_input_id":%d}`, gen, planB, gateN, reviseID))
	mustCode("plan B presentation", code, 200, out)
	if out["gate_revision"] != float64(2) {
		t.Fatalf("plan B gate_revision = %v, want 2", out["gate_revision"])
	}
	code, out = ownerPost(`{"kind":"approve_plan","body":"","expected_gate_revision":1}`)
	if code != http.StatusConflict || out["reason"] != "gate_revision_mismatch" {
		t.Fatalf("approval bound to the old revision: status %d %v, want 409 gate_revision_mismatch", code, out)
	}
	code, out = ownerPost(`{"kind":"approve_plan","body":"","expected_gate_revision":2}`)
	mustCode("approval bound to the new revision", code, http.StatusAccepted, out)
	approveID := inputID("approve_plan")
	code, out = workerCall(http.MethodGet, fmt.Sprintf("/inputs?claim_generation=%d", gen), "")
	mustCode("approve replay", code, 200, out)
	code, out = workerCall(http.MethodPost, "/inputs/ack", fmt.Sprintf(`{"ids":[%d],"claim_generation":%d}`, approveID, gen))
	mustCode("approve ack", code, 200, out)
	code, out = workerCall(http.MethodPost, "/inputs/applied", fmt.Sprintf(`{"ids":[%d],"claim_generation":%d}`, approveID, gen))
	mustCode("approve applied", code, 200, out)
	code, out = workerCall(http.MethodPost, "/state", fmt.Sprintf(`{"status":"running","session_id":"sess","claim_generation":%d}`, gen))
	mustCode("running after the approval", code, 200, out)
	if status, _, _ = runStatus(); status != "running" {
		t.Fatalf("status after the applied approval = %q, want running", status)
	}
}
