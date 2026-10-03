package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerRunUsageRouteLiveDB drives the estimated-usage-tail seam (issue #2014, ADR-2014)
// end to end through the REAL router with a real worker Bearer token: the route body frozen in
// workersvc/testdata/usage_tail_contract.json, the init and result stamps carried by /messages
// frames, and the usage_estimated_tail field GetRun returns. It also pins the route's guards:
// the claim fence (409 stale_claim), the typed stale 404, the missing-generation 409, the request
// bounds (400) and body limit (413), the chat/Codex refusal, the lane-worker 403 and the 401.
//
// MUTATION CHECK: dropping the RecordRunUsage fence reddens the stale cases; routing a not-owned
// run to an untyped 404 reddens the typed-stale case; adding the route to laneWorkerAllowlist
// reddens the lane case (and TestLaneWorkerRouteTable).
func TestWorkerRunUsageRouteLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	wsvc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}, wsvc: wsvc}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("usagetail-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 7, $3, 'https://forge.e2e/g/usagetail', 'main', true)`, repoID, connID, "g/usagetail-"+repoID.String())
	seedWorker := func(capabilities string) (uuid.UUID, string) {
		id := uuid.New()
		tok, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities) VALUES ($1, $2, $3, $4, 'online', $5::text[])`,
			id, userID, "usagetail-"+id.String(), hash, capabilities)
		return id, tok
	}
	workerID, tok := seedWorker("{}")
	capWorker, capTok := seedWorker("{credential_switch_v1}")
	otherWorker, otherTok := seedWorker("{}")
	_ = otherWorker
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	      VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'running', $4, 3)`, runID, userID, repoID, workerID)
	capRun := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	      VALUES ($1, $2, $3, 'issue', 2, 't', 'd', 'running', $4, 3)`, capRun, userID, repoID, capWorker)
	chatRun := uuid.New()
	exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, worker_id, claim_generation)
	      VALUES ($1, $2, 'chat', 't', 'd', 'running', $3, 1)`, chatRun, userID, workerID)
	codexRun := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, harness)
	      VALUES ($1, $2, $3, 'issue', 3, 't', 'd', 'running', $4, 3, 'codex')`, codexRun, userID, repoID, workerID)
	// An isolated-lane worker (a hosted, ephemeral worker bound to a run).
	laneID := uuid.New()
	laneTok, laneHash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	exec(`INSERT INTO workers (id, user_id, name, token_hash, kind, template_declared, hosted_size, docker_enabled, ephemeral, ephemeral_run_id, isolated_lane)
	      VALUES ($1, $2, $3, $4, 'hosted', 'base', 'm', false, true, $5, true)`, laneID, userID, "lane-"+laneID.String(), laneHash, codexRun)

	fixture, err := os.ReadFile("../workersvc/testdata/usage_tail_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Request            json.RawMessage `json:"request"`
		InitFramePayload   json.RawMessage `json:"init_frame_payload"`
		ResultFramePayload json.RawMessage `json:"result_frame_payload"`
	}
	if err := json.Unmarshal(fixture, &contract); err != nil {
		t.Fatal(err)
	}

	do := func(token, path string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	usagePath := func(id uuid.UUID) string { return "/api/worker/runs/" + id.String() + "/usage" }
	msgPath := "/api/worker/runs/" + runID.String() + "/messages"

	// No credential: refused before any handler.
	if rec := do("", usagePath(runID), contract.Request); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", rec.Code)
	}

	// The init and result stamps ride /messages frames (the fold reads them).
	frames, _ := json.Marshal(map[string]any{
		"claim_generation": 3,
		"messages": []map[string]any{
			{"seq": 1, "kind": "status", "agent": "lead", "payload": contract.InitFramePayload},
			{"seq": 2, "kind": "status", "agent": "lead", "payload": contract.ResultFramePayload},
		},
	})
	if rec := do(tok, msgPath, frames); rec.Code != http.StatusNoContent {
		t.Fatalf("frames = %d %s, want 204", rec.Code, rec.Body.String())
	}

	// The frozen request body lands: 204.
	if rec := do(tok, usagePath(runID), contract.Request); rec.Code != http.StatusNoContent {
		t.Fatalf("contract request = %d %s, want 204", rec.Code, rec.Body.String())
	}
	// One more record past the result's usage_through (3): ordinal 4 is the uncovered tail.
	var leg string
	{
		var req struct {
			Legs []struct {
				LegID string `json:"leg_id"`
			} `json:"legs"`
		}
		if err := json.Unmarshal(contract.Request, &req); err != nil {
			t.Fatal(err)
		}
		leg = req.Legs[0].LegID
	}
	tailBody, _ := json.Marshal(map[string]any{
		"claim_generation": 3,
		"messages": []map[string]any{{
			"message_id": "msg_tail4", "leg_id": leg, "ordinal": 4, "model": "claude-sonnet-5-5", "subagent": true,
			"input_tokens": 1000000, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
			"output_tokens": 1000000, "output_final": true,
		}},
	})
	if rec := do(tok, usagePath(runID), tailBody); rec.Code != http.StatusNoContent {
		t.Fatalf("tail post = %d %s, want 204", rec.Code, rec.Body.String())
	}
	// A re-post of the same bodies is idempotent.
	if rec := do(tok, usagePath(runID), tailBody); rec.Code != http.StatusNoContent {
		t.Fatalf("re-post = %d, want 204", rec.Code)
	}

	// GetRun carries the estimated tail apart from the metered usage.
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(store.User{ID: userID}, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("GetRun = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	tail := body.Run.UsageEstimatedTail
	if tail == nil {
		t.Fatalf("GetRun has no usage_estimated_tail: %s", rec.Body.String())
	}
	// Sonnet 5.5 at $2 in / $10 out per MTok: 1M in + 1M out = $12, exactly.
	if tail.InputTokens != 1_000_000 || tail.OutputTokens != 1_000_000 || tail.CostUSD == nil || *tail.CostUSD != 12 ||
		tail.CostStatus != "estimated" || tail.PriceTableVersion == "" {
		t.Fatalf("tail = %+v, want ordinal 4 only, $12 estimated", tail)
	}
	// The first contract leg closed at 5 but only ordinals 1 and 4 arrived (a gap), and the
	// contract's second leg marker never closed.
	if tail.Coverage != "partial" || strings.Join(tail.CoverageReasons, ",") != workersvc.UsageTailReasonLegNotClosed+","+workersvc.UsageTailReasonOrdinalGap {
		t.Fatalf("coverage = %s %v, want partial [leg_not_closed ordinal_gap]", tail.Coverage, tail.CoverageReasons)
	}
	if body.Run.Usage == nil || body.Run.Usage.InputTokens != 120 {
		t.Fatalf("metered usage = %+v, want the result frame's 120 input tokens, untouched by the tail", body.Run.Usage)
	}
	if !strings.Contains(rec.Body.String(), `"usage_estimated_tail"`) {
		t.Fatal("usage_estimated_tail key missing from the wire body")
	}
	// A run with no recorded legs omits the field entirely.
	rec = httptest.NewRecorder()
	h.GetRun(rec, runReq(store.User{ID: userID}, capRun))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "usage_estimated_tail") {
		t.Fatalf("a run with no usage legs must omit usage_estimated_tail: %d %s", rec.Code, rec.Body.String())
	}

	// ---- the fence: a stale generation is the same 409 stale_claim body /messages answers, and
	// nothing is written.
	stale, _ := json.Marshal(map[string]any{"claim_generation": 2, "messages": []map[string]any{{
		"message_id": "msg_stale", "leg_id": leg, "ordinal": 9, "model": "claude-sonnet-5-5", "input_tokens": 1, "output_final": true,
	}}})
	rec = do(tok, usagePath(runID), stale)
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != `{"disposition":"stale_claim"}` {
		t.Fatalf("stale generation = %d %s, want 409 {\"disposition\":\"stale_claim\"}", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_usage_messages WHERE run_id=$1 AND message_id='msg_stale'`, runID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a stale post wrote %d rows (%v), want 0", n, err)
	}
	// /messages answers the identical body, so the worker's one stale handler covers both.
	staleFrames, _ := json.Marshal(map[string]any{"claim_generation": 2, "messages": []map[string]any{{"seq": 50, "kind": "text", "payload": map[string]any{"text": "x"}}}})
	if mrec := do(tok, msgPath, staleFrames); mrec.Code != rec.Code || mrec.Body.String() != rec.Body.String() {
		t.Fatalf("/messages stale = %d %s, want the same as /usage (%d %s)", mrec.Code, mrec.Body.String(), rec.Code, rec.Body.String())
	}

	// ---- ownership: another worker's run is a TYPED stale 404, not an untyped one.
	rec = do(otherTok, usagePath(runID), contract.Request)
	var errBody map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if rec.Code != http.StatusNotFound || errBody["reason"] != workersvc.ReceiptStale {
		t.Fatalf("foreign run = %d %s, want 404 reason %q", rec.Code, rec.Body.String(), workersvc.ReceiptStale)
	}

	// ---- a capability worker that omits claim_generation is refused 409 (like /messages).
	noGen, _ := json.Marshal(map[string]any{"messages": []map[string]any{}})
	if rec := do(capTok, usagePath(capRun), noGen); rec.Code != http.StatusConflict || strings.Contains(rec.Body.String(), "stale_claim") {
		t.Fatalf("capability worker without generation = %d %s, want a 409 that is not stale_claim", rec.Code, rec.Body.String())
	}

	// ---- chat and Codex runs carry no tail.
	for name, id := range map[string]uuid.UUID{"chat": chatRun, "codex": codexRun} {
		if rec := do(tok, usagePath(id), []byte(`{"claim_generation":1,"legs":[],"messages":[]}`)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s run = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}

	// ---- request bounds and decode limits.
	rec2 := func(n int) *httptest.ResponseRecorder {
		var msgs []map[string]any
		for i := 1; i <= n; i++ {
			msgs = append(msgs, map[string]any{"message_id": fmt.Sprintf("bulk%d", i), "leg_id": leg, "ordinal": i, "model": "claude-sonnet-5-5", "output_final": true})
		}
		b, _ := json.Marshal(map[string]any{"claim_generation": 3, "messages": msgs})
		return do(tok, usagePath(runID), b)
	}
	if rec := rec2(501); rec.Code != http.StatusBadRequest {
		t.Fatalf("501 records = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := rec2(500); rec.Code != http.StatusNoContent {
		t.Fatalf("500 records = %d %s, want 204", rec.Code, rec.Body.String())
	}
	var legs []map[string]any
	for i := 0; i < 17; i++ {
		legs = append(legs, map[string]any{"leg_id": uuid.NewString()})
	}
	b, _ := json.Marshal(map[string]any{"claim_generation": 3, "legs": legs})
	if rec := do(tok, usagePath(runID), b); rec.Code != http.StatusBadRequest {
		t.Fatalf("17 leg markers = %d %s, want 400", rec.Code, rec.Body.String())
	}
	if rec := do(tok, usagePath(runID), []byte(`{"claim_generation":3,"bogus":1}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400 (DisallowUnknownFields)", rec.Code)
	}
	if rec := do(tok, usagePath(runID), []byte(`{"claim_generation":3,"messages":[{"message_id":"x","leg_id":"nope","ordinal":1,"model":"m"}]}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad leg uuid = %d, want 400", rec.Code)
	}
	huge := []byte(`{"claim_generation":3,"messages":[{"message_id":"` + strings.Repeat("a", 2<<20) + `"}]}`)
	if rec := do(tok, usagePath(runID), huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body = %d, want 413", rec.Code)
	}

	// ---- an isolated-lane worker is refused 403: the route is not in laneWorkerAllowlist.
	if rec := do(laneTok, usagePath(codexRun), contract.Request); rec.Code != http.StatusForbidden {
		t.Fatalf("lane worker = %d %s, want 403", rec.Code, rec.Body.String())
	}
}
