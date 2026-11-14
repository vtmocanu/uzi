package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #3250 M1: the start-time model and effort on POST /api/repos/{id}/runs, driven through the
// REAL mounted router (Bearer and cookie callers) against a real Postgres, with every case of the
// shared fixtures/run-start-options/cases.json. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB
// suffix.

type runStartCase struct {
	Name    string  `json:"name"`
	Harness string  `json:"harness"`
	Model   *string `json:"model"`
	Effort  *string `json:"effort"`
	Expect  struct {
		Status  int    `json:"status"`
		Code    string `json:"code"`
		Harness string `json:"harness"`
	} `json:"expect"`
}

func loadRunStartCases(t *testing.T) []runStartCase {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/run-start-options/cases.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc struct {
		Cases []runStartCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return doc.Cases
}

// body renders the request body the case describes (absent keys stay absent).
func (c runStartCase) body(iid int64) string {
	m := map[string]any{"issue_iid": iid, "harness": c.Harness}
	if c.Model != nil {
		m["model"] = *c.Model
	}
	if c.Effort != nil {
		m["effort"] = *c.Effort
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

type notifyRecorder struct {
	mu     sync.Mutex
	queued int
}

func (n *notifyRecorder) Notify(_ uuid.UUID, status string) {
	if status == "queued" {
		n.mu.Lock()
		n.queued++
		n.mu.Unlock()
	}
}

func (n *notifyRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.queued
}

type runStartFixture struct {
	h          *Handler
	router     http.Handler
	pool       *pgxpool.Pool
	owner      uuid.UUID
	connID     uuid.UUID
	forgeURL   string
	issueReads *atomic.Int64
	notes      *notifyRecorder
	nextProj   int64
}

// newRunStartFixture wires a real workersvc with the atomic transaction seam (a Codex resolution
// freezes its binding in the create transaction) and gives the owner a usable credential for BOTH
// harnesses, so an explicit claude or codex request resolves rather than refusing for credentials.
func newRunStartFixture(ctx context.Context, t *testing.T) *runStartFixture {
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

	reads := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/issues/") {
			reads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 9301, "iid": 7, "title": "run start issue", "description": "do the thing",
				"state": "opened", "labels": []string{"uzi"}, "web_url": "https://x",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	svc := forgesvc.New(q, box, 5*time.Second, nil)
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	wsvc.SetForges(svc)
	wsvc.SetTxBeginner(pool)
	notes := &notifyRecorder{}
	wsvc.SetLifecycle(notes)

	h := &Handler{
		pool: pool, q: q, box: box,
		cfg:  config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour},
		svc:  svc,
		wsvc: wsvc,
	}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	f := &runStartFixture{
		h: h, router: h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim),
		pool: pool, owner: uuid.New(), connID: uuid.New(), forgeURL: srv.URL, issueReads: reads, notes: notes,
		nextProj: 9300_000 + time.Now().UnixNano()%1_000_000,
	}
	cliMustExec(t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.owner, fmt.Sprintf("run-start-%s@e2e", f.owner))
	cliMustExec(t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'uzi-bot', 1, $4)`,
		f.connID, f.owner, srv.URL, mustSealT(ctx, t, box, []byte("glpat-dummy")))
	cliMustExec(t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'anthropic_token', 'anthropic-default', true, $3, 'master')`,
		uuid.New(), f.owner, []byte("ct"))
	codexSecret := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'openai_api_key', 'codex-default', true, $3, 'master')`,
		codexSecret, f.owner, []byte("ct"))
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: codexSecret, UserID: f.owner, Status: "static",
	}); err != nil {
		t.Fatalf("insert codex credential state: %v", err)
	}
	return f
}

// seedIssue inserts a fresh enabled repo with one uzi-labelled cached issue and returns its id and
// the create path.
func (f *runStartFixture) seedIssue(ctx context.Context, t *testing.T, iid int64) (uuid.UUID, string) {
	t.Helper()
	f.nextProj++
	repoID := uuid.New()
	cliMustExec(t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, $5, 'main', true)`,
		repoID, f.connID, f.nextProj, fmt.Sprintf("g/rso-%d", f.nextProj), f.forgeURL+"/g/rso")
	cliMustExec(t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'run start issue', 'opened', '["uzi"]'::jsonb, 'https://x', false, now(), now())`,
		repoID, iid)
	return repoID, "/api/repos/" + repoID.String() + "/runs"
}

type runStartRow struct {
	Model        pgtype.Text
	ModelSource  pgtype.Text
	Effort       pgtype.Text
	EffortSource pgtype.Text
	Harness      string
	CodexSecret  pgtype.UUID
}

func (f *runStartFixture) runRows(ctx context.Context, t *testing.T, repoID uuid.UUID) []runStartRow {
	t.Helper()
	rows, err := f.pool.Query(ctx,
		`SELECT model, model_source, effort, effort_source, harness, codex_secret_id FROM runs WHERE repo_id = $1`, repoID)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	defer rows.Close()
	var out []runStartRow
	for rows.Next() {
		var r runStartRow
		if err := rows.Scan(&r.Model, &r.ModelSource, &r.Effort, &r.EffortSource, &r.Harness, &r.CodexSecret); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func wantText(t *testing.T, label string, got pgtype.Text, want *string) {
	t.Helper()
	switch {
	case want == nil && got.Valid:
		t.Errorf("%s = %q, want NULL", label, got.String)
	case want != nil && (!got.Valid || got.String != *want):
		t.Errorf("%s = %v, want %q", label, got, *want)
	}
}

func decodeErrBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	return m
}

// TestRunStartOptionsCasesLiveDB drives every fixture case through the Bearer and the cookie
// caller and asserts every case ran.
func TestRunStartOptionsCasesLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newRunStartFixture(ctx, t)
	cases := loadRunStartCases(t)
	uzc := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
	jwt := cliMintJWT(t, f.pool, f.owner)

	ran := 0
	for i, c := range cases {
		for _, caller := range []string{"bearer", "cookie"} {
			name := fmt.Sprintf("%02d %s/%s", i, c.Name, caller)
			t.Run(name, func(t *testing.T) {
				repoID, path := f.seedIssue(ctx, t, 7)
				readsBefore, notesBefore := f.issueReads.Load(), f.notes.count()
				var rec *httptest.ResponseRecorder
				if caller == "bearer" {
					rec = bearerReqBody(f.router, http.MethodPost, path, uzc, c.body(7))
				} else {
					rec = cookieReq(t, f.router, http.MethodPost, path, jwt, c.body(7))
				}
				ran++
				if rec.Code != c.Expect.Status {
					t.Fatalf("status = %d, want %d (body %s)", rec.Code, c.Expect.Status, rec.Body.String())
				}
				rows := f.runRows(ctx, t, repoID)
				reads := f.issueReads.Load() - readsBefore
				queued := f.notes.count() - notesBefore
				switch c.Expect.Status {
				case http.StatusCreated:
					if len(rows) != 1 || queued != 1 {
						t.Fatalf("rows=%d queued notifies=%d, want 1/1", len(rows), queued)
					}
					r := rows[0]
					wantText(t, "model", r.Model, c.Model)
					wantText(t, "effort", r.Effort, c.Effort)
					var wantMS, wantES *string
					if c.Model != nil {
						wantMS = strp("run_override")
					}
					if c.Effort != nil {
						wantES = strp("run_override")
					}
					wantText(t, "model_source", r.ModelSource, wantMS)
					wantText(t, "effort_source", r.EffortSource, wantES)
					if r.Harness != c.Harness {
						t.Errorf("runs.harness = %q, want %q", r.Harness, c.Harness)
					}
					if c.Harness == "codex" && !r.CodexSecret.Valid {
						t.Errorf("a codex run must freeze its binding")
					}
				case http.StatusUnprocessableEntity:
					body := decodeErrBody(t, rec.Body.Bytes())
					if body["code"] != c.Expect.Code || body["harness"] != c.Expect.Harness {
						t.Errorf("422 body = %v, want code %q harness %q", body, c.Expect.Code, c.Expect.Harness)
					}
					if msg, _ := body["error"].(string); !strings.Contains(msg, *c.Model) {
						t.Errorf("error %q must name the refused model %q", msg, *c.Model)
					}
					// An explicit harness is refused by the handler fast-fail before the forge read.
					if reads != 0 {
						t.Errorf("forge issue reads = %d, want 0 before an explicit-harness 422", reads)
					}
					if len(rows) != 0 || queued != 0 {
						t.Errorf("rows=%d queued notifies=%d, want 0/0 after a refusal", len(rows), queued)
					}
				case http.StatusBadRequest:
					if reads != 0 {
						t.Errorf("forge issue reads = %d, want 0 before a shape 400", reads)
					}
					if len(rows) != 0 || queued != 0 {
						t.Errorf("rows=%d queued notifies=%d, want 0/0 after a refusal", len(rows), queued)
					}
					if msg, _ := decodeErrBody(t, rec.Body.Bytes())["error"].(string); msg == "" {
						t.Errorf("a 400 must carry an error message")
					}
				default:
					t.Fatalf("fixture case has unsupported expected status %d", c.Expect.Status)
				}
			})
		}
	}
	if want := len(cases) * 2; ran != want {
		t.Fatalf("ran %d fixture creates, want %d (every case, both callers)", ran, want)
	}
}

// TestRunStartOptionsShapeMessagesLiveDB pins the 400 texts.
func TestRunStartOptionsShapeMessagesLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newRunStartFixture(ctx, t)
	uzc := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
	_, path := f.seedIssue(ctx, t, 7)
	for body, want := range map[string]string{
		`{"issue_iid":7,"model":""}`:         "model must not contain whitespace; omit it to use your default",
		`{"issue_iid":7,"model":" opus"}`:    "model must not contain whitespace; omit it to use your default",
		`{"issue_iid":7,"model":"op us"}`:    "model must not contain whitespace; omit it to use your default",
		`{"issue_iid":7,"effort":"HIGH"}`:    "effort must be one of low, medium, high, xhigh, max",
		`{"issue_iid":7,"effort":""}`:        "effort must be one of low, medium, high, xhigh, max",
		`{"issue_iid":7,"effort":"runtime"}`: "effort must be one of low, medium, high, xhigh, max",
	} {
		rec := bearerReqBody(f.router, http.MethodPost, path, uzc, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
			continue
		}
		if got, _ := decodeErrBody(t, rec.Body.Bytes())["error"].(string); got != want {
			t.Errorf("%s: error %q, want %q", body, got, want)
		}
	}
	if n := f.issueReads.Load(); n != 0 {
		t.Errorf("forge issue reads = %d, want 0 for shape refusals", n)
	}
}

// TestRunStartOptionsOneClickPinsNothingLiveDB covers the web one-click row: no model and no effort
// leaves all four columns NULL; effort alone stamps only the effort pair; model alone only the
// model pair. No harness is named, so D11 resolves it.
func TestRunStartOptionsOneClickPinsNothingLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newRunStartFixture(ctx, t)
	jwt := cliMintJWT(t, f.pool, f.owner)
	for _, tc := range []struct {
		name, body        string
		model, modelSrc   *string
		effort, effortSrc *string
	}{
		{"nothing", `{"issue_iid":7}`, nil, nil, nil, nil},
		{"effort only", `{"issue_iid":7,"effort":"xhigh"}`, nil, nil, strp("xhigh"), strp("run_override")},
		{"model only", `{"issue_iid":7,"model":"opus"}`, strp("opus"), strp("run_override"), nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoID, path := f.seedIssue(ctx, t, 7)
			rec := cookieReq(t, f.router, http.MethodPost, path, jwt, tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
			}
			rows := f.runRows(ctx, t, repoID)
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			wantText(t, "model", rows[0].Model, tc.model)
			wantText(t, "model_source", rows[0].ModelSource, tc.modelSrc)
			wantText(t, "effort", rows[0].Effort, tc.effort)
			wantText(t, "effort_source", rows[0].EffortSource, tc.effortSrc)
		})
	}
}

// TestRunStartModelRefusalRollsBackLiveDB covers the authoritative in-transaction check: no harness
// is named, so the handler cannot fast-fail and the create resolves D11 first. The refusal is 422
// with the resolved harness and leaves no run row, no Codex binding and no queued notify.
func TestRunStartModelRefusalRollsBackLiveDB(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, defaultHarness, model, wantHarness string
	}{
		{"codex default refuses a claude alias", "codex", "opus", "codex"},
		{"codex default refuses a non-curated id", "codex", "gpt-foo", "codex"},
		{"claude default refuses a codex model", "claude", "gpt-6-sol", "claude"},
		{"claude default refuses gpt-6-luna", "claude", "gpt-6-luna", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunStartFixture(ctx, t)
			cliMustExec(t, f.pool, `UPDATE users SET default_harness = $2 WHERE id = $1`, f.owner, tc.defaultHarness)
			repoID, path := f.seedIssue(ctx, t, 7)
			rec := bearerReqBody(f.router, http.MethodPost, path, cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser),
				fmt.Sprintf(`{"issue_iid":7,"model":%q}`, tc.model))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
			}
			body := decodeErrBody(t, rec.Body.Bytes())
			if body["code"] != "model_incompatible_with_harness" || body["harness"] != tc.wantHarness {
				t.Errorf("body = %v, want model_incompatible_with_harness on %s", body, tc.wantHarness)
			}
			if f.issueReads.Load() == 0 {
				t.Errorf("forge issue reads = 0, want the authoritative check to run after the read")
			}
			if rows := f.runRows(ctx, t, repoID); len(rows) != 0 {
				t.Errorf("run rows = %d, want 0 after the rollback", len(rows))
			}
			var bound int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND codex_secret_id IS NOT NULL`, f.owner).Scan(&bound); err != nil {
				t.Fatal(err)
			}
			if bound != 0 {
				t.Errorf("codex bindings frozen = %d, want 0", bound)
			}
			if f.notes.count() != 0 {
				t.Errorf("queued notifies = %d, want 0", f.notes.count())
			}
		})
	}
}

// TestRunStartCredentialRefusalsKeepPrecedenceLiveDB proves the model check runs after the
// credential-override validation: the same incompatible model, with a credential refusal also
// present, reports the credential refusal.
func TestRunStartCredentialRefusalsKeepPrecedenceLiveDB(t *testing.T) {
	ctx := context.Background()
	t.Run("override on a codex resolution", func(t *testing.T) {
		f := newRunStartFixture(ctx, t)
		cliMustExec(t, f.pool, `UPDATE users SET default_harness = 'codex' WHERE id = $1`, f.owner)
		repoID, path := f.seedIssue(ctx, t, 7)
		rec := bearerReqBody(f.router, http.MethodPost, path, cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser),
			`{"issue_iid":7,"model":"opus","credential_override":{"mode":"auto"}}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
		}
		body := decodeErrBody(t, rec.Body.Bytes())
		if body["code"] == "model_incompatible_with_harness" || !strings.Contains(fmt.Sprint(body["error"]), "credential override") {
			t.Errorf("body = %v, want the credential-override refusal, not the model refusal", body)
		}
		if len(f.runRows(ctx, t, repoID)) != 0 {
			t.Errorf("a refusal must leave no run row")
		}
	})
	t.Run("malformed secret_id keeps its 400 over an explicit-harness model refusal", func(t *testing.T) {
		f := newRunStartFixture(ctx, t)
		repoID, path := f.seedIssue(ctx, t, 7)
		rec := bearerReqBody(f.router, http.MethodPost, path, cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser),
			`{"issue_iid":7,"harness":"codex","model":"opus","credential_override":{"mode":"pinned","secret_id":"not-a-uuid"}}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "credential_override.secret_id must be a valid uuid") {
			t.Fatalf("status = %d body %s, want the secret_id 400", rec.Code, rec.Body.String())
		}
		if len(f.runRows(ctx, t, repoID)) != 0 {
			t.Errorf("a refusal must leave no run row")
		}
	})
	t.Run("pinned secret that does not exist", func(t *testing.T) {
		f := newRunStartFixture(ctx, t)
		repoID, path := f.seedIssue(ctx, t, 7)
		rec := bearerReqBody(f.router, http.MethodPost, path, cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser),
			fmt.Sprintf(`{"issue_iid":7,"model":"gpt-6-sol","credential_override":{"mode":"pinned","secret_id":%q}}`, uuid.NewString()))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
		}
		if len(f.runRows(ctx, t, repoID)) != 0 {
			t.Errorf("a refusal must leave no run row")
		}
	})
}
