package handler

import (
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #422 M6 — the N-1 old-worker ↔ new-API compatibility contract, proven end to
// end against a HEAD-migrated schema. This is the runnable half of the "an old worker
// keeps working across a rolling upgrade" invariant: during a release the api rolls
// ahead of the worker fleet, so a worker still running the previous image must be able
// to claim → run → report → complete against the NEW api and the NEW schema.
//
// The mechanism under test is a convention, not a shim: every worker request decodes
// through httpx.DecodeJSON, which sets DisallowUnknownFields, and an OLD worker sends a
// SMALLER, older body (it simply has not learned the fields added since). The invariant
// is therefore ONE-DIRECTIONAL and asymmetric — the new api must accept the old body
// (fewer fields, all long-standing) and every response must still carry the fields the
// old worker reads. This test sends deliberately MINIMAL, pre-current-era JSON on every
// call: only fields that have existed since the worker protocol's first releases, with
// none of the additive-optional fields (mr_web_url, report_only, milestones,
// prd_done_path, capabilities, max_concurrent_runs, stats, …) an old image never sends.
// A 4xx on any of these would mean DisallowUnknownFields rejected a body it must accept,
// or a required-field tightening broke the skew — the exact regression M6 guards.
//
// It drives REAL HTTP through the worker router: h.WorkerRoutes mounts the worker
// protocol under RequireWorker (Bearer join-token auth, sha256 lookup), and each request
// carries the worker's real join token — so the join-token auth, the chi routing, and
// httpx.DecodeJSON's strict decode are all on the path, not bypassed. The alternative
// (calling handlers with a hand-built worker context) would skip the very decoder whose
// strictness is the thing under test.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package (…LiveDB) so this auto-runs in CI's
// test-api-store-it job with no workflow edit.

type skewFixture struct {
	pool  *pgxpool.Pool
	srv   http.Handler
	token string // the worker's plaintext join token (Bearer credential)
	owner uuid.UUID
	runID uuid.UUID
}

// newSkewFixture migrates to head, builds the real Handler + worker router, and seeds a
// user, forge connection, repo, an issue, a QUEUED issue run, and a worker whose
// token_hash matches a freshly-issued join token. The PAT and the Anthropic token are
// sealed with the fixture's own box so the claim can decrypt and attach both.
func newSkewFixture(ctx context.Context, t *testing.T) skewFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
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
	h := &Handler{
		pool:      pool,
		q:         q,
		box:       box,
		wsvc:      workersvc.New(q, box, workersvc.Params{}),
		version:   "dev",
		now:       time.Now,
		startedAt: time.Now(),
	}
	// The same router the worker (and the TLS listener) dial: mountWorkerRoutes under
	// RequireWorker, at /api/worker/*. A generous no-op limiter so rate limiting never
	// interferes with the protocol assertions.
	noLimit := mw.NewLimiter(1000, time.Minute, nil)
	srv := h.WorkerRoutes(noLimit)

	f := skewFixture{
		pool:  pool,
		srv:   srv,
		owner: uuid.New(),
		runID: uuid.New(),
	}

	// A unique forge project id per fixture so parallel LiveDB rows on the shared
	// database never collide on forge_connections' project/bot uniqueness.
	projectID := int64(uuid.New().ID())
	sealedPAT, err := box.Seal([]byte("glpat-dummy-skew-token-000000")) //gitleaks:allow synthetic PAT fixture, sealed, never a real credential
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedTok, err := box.Seal([]byte("anthropic-oauth-skew-fixture-000000"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}

	connID, repoID := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.owner, fmt.Sprintf("skew-owner-%s@e2e", uuid.NewString()[:8]))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.example', 'uzi-bot', $3, $4)`,
		connID, f.owner, projectID, sealedPAT)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, 'g/skew', 'https://forge.example/g/skew', 'main', true)`,
		repoID, connID, projectID)
	mustExecT(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'skew issue', 'opened', '["PRD"]'::jsonb, 'https://x', true, now(), now())`,
		repoID, 42)
	mustExecT(ctx, t, pool, `INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'default', true, $3, 'master')`,
		f.owner, store.KindAnthropicToken, sealedTok)

	// The QUEUED issue run the old worker will claim. Inserted directly (not via
	// CreateRun) — the claim only needs a claimable queued row for this user.
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, $4, 'skew issue', 'do the thing', 'queued', 'issue')`,
		f.runID, f.owner, repoID, 42)

	// The worker + its join token. token_hash is UNIQUE and the LiveDB runner shares one
	// database, so the token is freshly generated per fixture. A NULL heartbeat is
	// claimable now, and being this user's only worker it never defers under the
	// fleet-aware spread.
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("generate join token: %v", err)
	}
	f.token = token
	mustExecT(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'skew-worker', $3, 'online')`,
		uuid.New(), f.owner, hash)
	return f
}

// post drives one worker request through the real router with the worker's Bearer join
// token and the exact old-shaped body bytes — the way an old worker image would.
func (f skewFixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func (f skewFixture) runStatus(ctx context.Context, t *testing.T) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, f.runID).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return status
}

// TestOldWorkerSkewLiveDB walks an OLD worker's full protocol — register, heartbeat,
// claim, running, completed — sending MINIMAL, pre-current-era JSON on every call, and
// asserts each step is 2xx and the run reaches 'completed'. Every 2xx is the skew
// invariant firing once: the new api's DisallowUnknownFields decode ACCEPTED a body
// that omits every field added since the old image was cut.
func TestOldWorkerSkewLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSkewFixture(ctx, t)

	// (1) register — an old worker announces version + name + template and nothing else
	// (no capabilities, no max_concurrent_runs). All three are long-standing declared
	// fields; the newer ones are simply absent.
	if rec := f.post(t, "/api/worker/register", `{"version":"0.1.0","name":"w","template":"base"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /worker/register (old body) = %d, want 200 — the new api must accept a smaller old register body\nbody: %s",
			rec.Code, rec.Body.String())
	}

	// (2) heartbeat — an old worker sends only {version}; the stats object an M+ worker
	// adds is absent, and the decode must not require it.
	if rec := f.post(t, "/api/worker/heartbeat", `{"version":"0.1.0"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /worker/heartbeat (old body) = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	// (3) claim — no body (an old worker sends none, and no ?lane=). The payload must
	// carry the fields an old worker reads to run the card: run_id, kind, status, the
	// issue summary.
	rec := f.post(t, "/api/worker/runs/claim", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /worker/runs/claim = %d, want 200 with a payload for the queued run\nbody: %s", rec.Code, rec.Body.String())
	}
	var claim struct {
		RunID            string `json:"run_id"`
		Kind             string `json:"kind"`
		Status           string `json:"status"`
		IssueIID         *int64 `json:"issue_iid"`
		IssueTitle       string `json:"issue_title"`
		IssueDescription string `json:"issue_description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &claim); err != nil {
		t.Fatalf("decode claim payload: %v\nbody: %s", err, rec.Body.String())
	}
	if claim.RunID != f.runID.String() {
		t.Fatalf("claim run_id = %q, want %q — the claim handed the old worker a different run", claim.RunID, f.runID)
	}
	if claim.Kind != "issue" {
		t.Errorf("claim kind = %q, want \"issue\" — an old worker branches on this field", claim.Kind)
	}
	if claim.Status == "" {
		t.Error("claim carried an empty status — an old worker reads it to know the run state")
	}
	if claim.IssueIID == nil || *claim.IssueIID != 42 {
		t.Errorf("claim issue_iid = %v, want 42 — an old worker works this issue", claim.IssueIID)
	}
	if claim.IssueTitle == "" {
		t.Error("claim carried an empty issue_title — the run must stay displayable/self-contained for an old worker")
	}

	runPath := "/api/worker/runs/" + f.runID.String() + "/state"

	// (4) running — the minimal transition body an old worker sends after checkout:
	// {status} alone. None of the additive companions (repo_agents, milestones,
	// session_id, iteration_count) are present.
	if rec := f.post(t, runPath, `{"status":"running"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /worker/runs/{id}/state {status:running} = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if got := f.runStatus(ctx, t); got != "running" {
		t.Fatalf("run status after running report = %q, want running", got)
	}

	// (5) completed — an old worker reports the terminal transition with only the
	// long-standing completion fields (branch + mr_iid). The R8+ additive-optional
	// fields (mr_web_url, report_only/report_md, prd_done_path, milestones_completed)
	// an old image never learned are all absent, and DisallowUnknownFields must accept
	// the smaller body.
	if rec := f.post(t, runPath, `{"status":"completed","branch":"agent/issue-42","mr_iid":7}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /worker/runs/{id}/state {status:completed, ...minimal} = %d, want 200 — an old worker's "+
			"smaller terminal body must be accepted\nbody: %s", rec.Code, rec.Body.String())
	}
	if got := f.runStatus(ctx, t); got != "completed" {
		t.Fatalf("run status after completed report = %q, want completed — the old worker's terminal report did not land", got)
	}
}
