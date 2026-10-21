package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerCredentialSwitchHoldAckDispositionLiveDB (issue #3181) drives the REAL
// POST /api/worker/runs/{id}/state handler and pins the wire contract the worker keys on: an
// applied credential_switch_held report answers 200 with disposition "held", the applied
// credential_switch_hold_settled acknowledgement answers 200 with disposition "hold_settled", and a
// not-applied report (here the acknowledgement on a run that is not in a hold) is the ordinary 409
// carrying NO disposition at all.
func TestWorkerCredentialSwitchHoldAckDispositionLiveDB(t *testing.T) {
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
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	wsvc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: wsvc}

	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	const gen = int64(9)
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("credswh-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	const nonce = "nonce-ack-wire"
	exec(`INSERT INTO workers (id, user_id, name, token_hash, snapshot_register_nonce) VALUES ($1, $2, 'credswh', $3, $4)`,
		workerID, userID, append([]byte("credswh-"), workerID[:]...), nonce)
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
	         status_since, worker_id, started_at, claim_generation,
	         credential_switch_requested_at, credential_switch_generation)
	      VALUES ($1, $2, $3, $4, 't', 'd', 'running', now() - interval '5 minutes', $5,
	              now() - interval '10 minutes', $6, now(), $6)`,
		runID, userID, repoID, uniq(runID), workerID, gen)

	wkr := store.Worker{ID: workerID, UserID: userID}
	reportBody := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x", bytes.NewReader([]byte(body)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		rec := httptest.NewRecorder()
		h.WorkerRunState(rec, req.WithContext(context.WithValue(mw.ContextWithWorker(req.Context(), wkr), chi.RouteCtxKey, rctx)))
		return rec
	}
	report := func(state string) *httptest.ResponseRecorder {
		return reportBody(fmt.Sprintf(`{"status":%q,"claim_generation":%d,"register_nonce":%q}`, state, gen, nonce))
	}
	disposition := func(rec *httptest.ResponseRecorder) (string, bool) {
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode ack: %v (body=%s)", err, rec.Body.String())
		}
		raw, ok := body["disposition"]
		var s string
		if ok {
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatalf("decode disposition: %v (body=%s)", err, rec.Body.String())
			}
		}
		return s, ok
	}

	// Not applied: the run is still running, so the acknowledgement matches no hold.
	rec := report("credential_switch_hold_settled")
	if rec.Code != http.StatusConflict {
		t.Fatalf("ack on a run not in a hold = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if d, ok := disposition(rec); ok {
		t.Fatalf("a not-applied 409 carries disposition %q, want none; body=%s", d, rec.Body.String())
	}

	// Wire contract: both reports REQUIRE register_nonce; omitting it is a 400 and changes nothing.
	for _, state := range []string{"credential_switch_held", "credential_switch_hold_settled"} {
		rec := reportBody(fmt.Sprintf(`{"status":%q,"claim_generation":%d}`, state, gen))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s without register_nonce = %d, want 400; body=%s", state, rec.Code, rec.Body.String())
		}
	}

	rec = report("credential_switch_held")
	if rec.Code != http.StatusOK {
		t.Fatalf("hold report = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if d, ok := disposition(rec); !ok || d != "held" {
		t.Fatalf("hold ack disposition = %q (present=%v), want \"held\"; body=%s", d, ok, rec.Body.String())
	}

	rec = report("credential_switch_hold_settled")
	if rec.Code != http.StatusOK {
		t.Fatalf("settle ack = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if d, ok := disposition(rec); !ok || d != "hold_settled" {
		t.Fatalf("settle ack disposition = %q (present=%v), want \"hold_settled\"; body=%s", d, ok, rec.Body.String())
	}
}
