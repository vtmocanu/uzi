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

// TestWorkerCredentialSwitchSignalLiveDB drives the REAL worker endpoints and proves the
// transport half of the held-state switch protocol (PRD #1247 M5, D3/D4, step 2): while a switch
// is pending for the run's CURRENT claim, GET .../inputs carries the worker-facing
// `credential_switch {generation}` field on an EMPTY-inputs poll (and drains nothing), and the
// state-report ack carries the same signal on the applied 200. The signal is scoped to the exact
// claim: a run whose claim is released (or reclaimed past the stamp) carries no signal.
func TestWorkerCredentialSwitchSignalLiveDB(t *testing.T) {
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
	wsvc.SetTxBeginner(pool) // REQUIRED: the fence opens a tx when a report carries a generation
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("credsw-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'credsw', $3)`,
		workerID, userID, append([]byte("credsw-"), workerID[:]...))
	// A running run held by the worker with a switch PENDING for the current claim (stamp targets
	// claim_generation = gen; claim_released_at NULL).
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
	         status_since, worker_id, started_at, claim_generation,
	         credential_switch_requested_at, credential_switch_generation)
	      VALUES ($1, $2, $3, $4, 't', 'd', 'running', now() - interval '5 minutes', $5,
	              now() - interval '10 minutes', $6, now(), $6)`,
		runID, userID, repoID, uniq(runID), workerID, gen)

	wkr := store.Worker{ID: workerID, UserID: userID}
	newReq := func(method, body string) *http.Request {
		req := httptest.NewRequest(method, "/api/worker/runs/x", bytes.NewReader([]byte(body)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		return req.WithContext(context.WithValue(mw.ContextWithWorker(req.Context(), wkr), chi.RouteCtxKey, rctx))
	}

	t.Run("inputs empty poll carries the signal and drains nothing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.WorkerRunInputs(rec, newReq(http.MethodGet, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Inputs           []json.RawMessage                 `json:"inputs"`
			CredentialSwitch *workersvc.CredentialSwitchSignal `json:"credential_switch"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode inputs body: %v (body=%s)", err, rec.Body.String())
		}
		if body.Inputs == nil {
			t.Fatal("inputs must always be present as an array, even on an empty poll")
		}
		if len(body.Inputs) != 0 {
			t.Fatalf("inputs = %v, want empty (nothing buffered)", body.Inputs)
		}
		if body.CredentialSwitch == nil || body.CredentialSwitch.Generation != gen {
			t.Fatalf("credential_switch = %+v, want generation %d", body.CredentialSwitch, gen)
		}
	})

	t.Run("state-report ack carries the signal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.WorkerRunState(rec, newReq(http.MethodPost, fmt.Sprintf(`{"status":"running","claim_generation":%d}`, gen)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (applied running report); body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Run              json.RawMessage                   `json:"run"`
			CredentialSwitch *workersvc.CredentialSwitchSignal `json:"credential_switch"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode state ack: %v (body=%s)", err, rec.Body.String())
		}
		if len(body.Run) == 0 {
			t.Fatal("the state ack must still carry the run")
		}
		if body.CredentialSwitch == nil || body.CredentialSwitch.Generation != gen {
			t.Fatalf("credential_switch = %+v, want generation %d", body.CredentialSwitch, gen)
		}
	})

	t.Run("no signal once the claim is released", func(t *testing.T) {
		exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, runID)
		rec := httptest.NewRecorder()
		h.WorkerRunInputs(rec, newReq(http.MethodGet, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			CredentialSwitch *workersvc.CredentialSwitchSignal `json:"credential_switch"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode inputs body: %v (body=%s)", err, rec.Body.String())
		}
		if body.CredentialSwitch != nil {
			t.Fatalf("credential_switch = %+v, want nil once the claim is released", body.CredentialSwitch)
		}
	})
}
