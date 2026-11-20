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

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// followupFinalizeFixture is fixtures/worker-protocol/followup-finalize.json: the request the
// worker sends and the response shapes the api answers with.
type followupFinalizeFixture struct {
	requestRaw json.RawMessage
	Request    map[string]any `json:"request"`
	ParkAck    struct {
		HTTPStatus      int   `json:"http_status"`
		FollowupParkSeq int64 `json:"followup_park_seq"`
	} `json:"park_ack"`
	Success struct {
		HTTPStatus  int    `json:"http_status"`
		Applied     bool   `json:"applied"`
		Disposition string `json:"disposition"`
		RunStatus   string `json:"run_status"`
	} `json:"success"`
	Refusals []struct {
		HTTPStatus int    `json:"http_status"`
		Applied    bool   `json:"applied"`
		Reason     string `json:"reason"`
	} `json:"refusals"`
	StaleClaim struct {
		HTTPStatus  int    `json:"http_status"`
		Disposition string `json:"disposition"`
	} `json:"stale_claim"`
	OwnerFollowupRefusal struct {
		HTTPStatus int    `json:"http_status"`
		Reason     string `json:"reason"`
	} `json:"owner_followup_refusal"`
}

func readFollowupFinalizeFixture(t *testing.T) followupFinalizeFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/worker-protocol/followup-finalize.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	var fx followupFinalizeFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	var parts struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatal(err)
	}
	fx.requestRaw = parts.Request
	return fx
}

// TestFollowupFinalizeWireLiveDB (issue #4479) drives the REAL worker /state route and the owner
// inputs route for an interactive task run: the park ACK carries followup_park_seq, the
// followup_finalize report (read from the shared fixture) is admitted with its disposition, every
// refusal has its encoding, a stale claim keeps stale_claim, and the owner's follow-up is a 409
// follow_up_finalizing afterwards. Expected shapes come from the fixture, not from this test.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestFollowupFinalizeWireLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	fx := readFollowupFinalizeFixture(t)
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
	owner, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner, fmt.Sprintf("ffwire-%s@e2e", owner))
	cliMustExec(t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, uniq(repoID), fmt.Sprintf("g/ffwire-%s", repoID))
	workerToken, tokenHash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
	      VALUES ($1, $2, $3, $4, 'online', now())`, workerID, owner, "ffwire-"+workerID.String()[:8], tokenHash)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)

	// newParked inserts an interactive task run claimed at generation 1 and parks it through the
	// real route, returning its id and the ACKed park sequence.
	call := func(runID uuid.UUID, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, "/api/worker/runs/"+runID.String()+path, strings.NewReader(body))
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
	ownerPost := func(runID uuid.UUID, body string) (int, map[string]any) {
		t.Helper()
		rec := bearerReqBody(ownerRouter, http.MethodPost, "/api/runs/"+runID.String()+"/inputs", uzc, body)
		var out map[string]any
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode owner input %q: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, out
	}
	newParked := func() (uuid.UUID, int64) {
		t.Helper()
		runID := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		        claim_generation, auto_approve, plan_source, session_id)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 1, false, 'agent', 'sess')`, runID, owner, repoID, uniq(runID), workerID)
		cliMustExec(t, pool, `UPDATE runs SET kind='task', issue_iid=NULL, branch='uzi/interactive-test', interactive=true WHERE id = $1`, runID)
		code, out := call(runID, http.MethodPost, "/state", `{"status":"awaiting_followup","session_id":"sess","claim_generation":1}`)
		if code != fx.ParkAck.HTTPStatus {
			t.Fatalf("park: status %d, want %d: %v", code, fx.ParkAck.HTTPStatus, out)
		}
		seq, ok := out["followup_park_seq"].(float64)
		if !ok || seq != float64(fx.ParkAck.FollowupParkSeq) {
			t.Fatalf("park ACK followup_park_seq = %v, want %d (the fixture's first park)", out["followup_park_seq"], fx.ParkAck.FollowupParkSeq)
		}
		return runID, int64(seq)
	}
	finalize := func(runID uuid.UUID, mutate func(map[string]any)) (int, map[string]any) {
		t.Helper()
		body := map[string]any{}
		if err := json.Unmarshal(fx.requestRaw, &body); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(body)
		}
		raw, _ := json.Marshal(body)
		return call(runID, http.MethodPost, "/state", string(raw))
	}
	refusal := func(reason string) (int, bool) {
		for _, r := range fx.Refusals {
			if r.Reason == reason {
				return r.HTTPStatus, r.Applied
			}
		}
		t.Fatalf("fixture has no refusal %q", reason)
		return 0, false
	}
	wantRefusal := func(what string, runID uuid.UUID, reason, wantStatus string, mutate func(map[string]any)) {
		t.Helper()
		code, out := finalize(runID, mutate)
		wantCode, wantApplied := refusal(reason)
		if code != wantCode || out["applied"] != wantApplied || out["reason"] != reason {
			t.Fatalf("%s: (%d, applied %v, reason %v), want (%d, %v, %q): %v", what, code, out["applied"], out["reason"], wantCode, wantApplied, reason, out)
		}
		if run, _ := out["run"].(map[string]any); run == nil || run["status"] != wantStatus {
			t.Fatalf("%s: refusal must carry run status %q: %v", what, wantStatus, out)
		}
	}

	t.Run("the finalize report is admitted, replays, and refuses the owner's follow-up", func(t *testing.T) {
		runID, _ := newParked()
		code, out := finalize(runID, nil)
		run, _ := out["run"].(map[string]any)
		if code != fx.Success.HTTPStatus || out["applied"] != fx.Success.Applied || out["disposition"] != fx.Success.Disposition || run == nil || run["status"] != fx.Success.RunStatus {
			t.Fatalf("finalize = (%d, %v), want fixture success %+v", code, out, fx.Success)
		}
		// A lost ACK: the same report converges to the same answer.
		code2, out2 := finalize(runID, nil)
		run2, _ := out2["run"].(map[string]any)
		if code2 != fx.Success.HTTPStatus || out2["applied"] != true || out2["disposition"] != fx.Success.Disposition || run2 == nil || run2["status"] != fx.Success.RunStatus {
			t.Fatalf("replay = (%d, %v)", code2, out2)
		}
		// A delayed park report does not move the admitted run.
		if code, out := call(runID, http.MethodPost, "/state", `{"status":"awaiting_followup","claim_generation":1}`); code != http.StatusConflict {
			t.Fatalf("delayed park = (%d, %v), want 409", code, out)
		} else if r, _ := out["run"].(map[string]any); r == nil || r["status"] != "running" {
			t.Fatalf("delayed park answered %v, want the run still running", out)
		}
		// The owner's follow-up is refused with the typed reason.
		oc, oo := ownerPost(runID, `{"kind":"follow_up","body":"one more thing"}`)
		if oc != fx.OwnerFollowupRefusal.HTTPStatus || oo["reason"] != fx.OwnerFollowupRefusal.Reason {
			t.Fatalf("owner follow-up = (%d, %v), want (%d, reason %q)", oc, oo, fx.OwnerFollowupRefusal.HTTPStatus, fx.OwnerFollowupRefusal.Reason)
		}
	})

	t.Run("a pending follow-up refuses with followup_pending", func(t *testing.T) {
		runID, _ := newParked()
		if oc, oo := ownerPost(runID, `{"kind":"follow_up","body":"not yet"}`); oc != http.StatusAccepted {
			t.Fatalf("owner follow-up = (%d, %v), want 202", oc, oo)
		}
		wantRefusal("pending follow-up", runID, "followup_pending", "awaiting_followup", nil)
	})

	t.Run("a deliverable cancel refuses with cancel_pending", func(t *testing.T) {
		runID, _ := newParked()
		if oc, oo := ownerPost(runID, `{"kind":"cancel"}`); oc != http.StatusAccepted && oc != http.StatusOK {
			t.Fatalf("owner cancel = (%d, %v)", oc, oo)
		}
		wantRefusal("pending cancel", runID, "cancel_pending", "awaiting_followup", nil)
	})

	t.Run("a different park refuses with followup_finalize_refused", func(t *testing.T) {
		runID, seq := newParked()
		wantRefusal("stale seq", runID, "followup_finalize_refused", "awaiting_followup", func(b map[string]any) { b["followup_park_seq"] = seq + 5 })
	})

	t.Run("a superseded claim keeps the stale_claim disposition", func(t *testing.T) {
		runID, _ := newParked()
		code, out := finalize(runID, func(b map[string]any) { b["claim_generation"] = 2 })
		if code != fx.StaleClaim.HTTPStatus || out["disposition"] != fx.StaleClaim.Disposition {
			t.Fatalf("stale finalize = (%d, %v), want (%d, disposition %q)", code, out, fx.StaleClaim.HTTPStatus, fx.StaleClaim.Disposition)
		}
		if _, has := out["reason"]; has {
			t.Fatalf("a stale claim must not carry a refusal reason: %v", out)
		}
	})

	t.Run("a run under an owner hold refuses with followup_finalize_refused", func(t *testing.T) {
		// The service answers an owner-held run with an early no-op (applied false, no error), which
		// must still carry a reason on the wire.
		runID, _ := newParked()
		cliMustExec(t, pool, `UPDATE runs SET status = 'paused', hold_reason = 'iteration_budget_exhausted' WHERE id = $1`, runID)
		wantRefusal("iteration-budget pause", runID, "followup_finalize_refused", "paused", nil)
	})

	t.Run("a malformed report is a 400", func(t *testing.T) {
		runID, _ := newParked()
		for _, mutate := range []func(map[string]any){
			func(b map[string]any) { delete(b, "followup_park_seq") },
			func(b map[string]any) { b["followup_park_seq"] = 0 },
			func(b map[string]any) { delete(b, "claim_generation") },
		} {
			code, out := finalize(runID, mutate)
			if code != http.StatusBadRequest {
				t.Fatalf("malformed finalize = (%d, %v), want 400", code, out)
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, "followup_finalize") {
				t.Fatalf("400 text %q does not list followup_finalize", msg)
			}
		}
	})
}
