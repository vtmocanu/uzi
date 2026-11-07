package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWorkerRecordCodexCatalogDefaultRouteLiveDB drives POST /api/worker/runs/{id}/codex-catalog-default
// (issue #3251) through the REAL router with real worker Bearer tokens and pins the PRD section 4
// status table: 204 stored, 400 claim_generation_required, 400 invalid_catalog_default, the typed
// stale 404, 409 stale_claim and 409 stale_context, with nothing stored on any refusal.
//
// MUTATION CHECK: dropping the context comparison reddens the stale_context cases; dropping the
// harness check reddens the Claude run case; dropping the claim fence reddens both stale_claim
// cases; mounting the route without refuseJobRuns reddens TestWorkerJobRouteRefusalsLiveDB.
func TestWorkerRecordCodexCatalogDefaultRouteLiveDB(t *testing.T) {
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
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("catalog-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 7, $3, 'https://forge.e2e/g/catalog', 'main', true)`, repoID, connID, "g/catalog-"+repoID.String())
	seedWorker := func() (uuid.UUID, string) {
		id := uuid.New()
		tok, hash, err := jointoken.Generate()
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`, id, userID, "catalog-"+id.String(), hash)
		return id, tok
	}
	workerID, tok := seedWorker()
	_, otherTok := seedWorker()

	// A static OpenAI API key alias, the user's Codex default.
	aliasID := uuid.New()
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with, is_default)
	      VALUES ($1, $2, 'openai_api_key', $3, $4, 'master', true)`, aliasID, userID, "key-"+aliasID.String(), []byte{0x2})
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: aliasID, UserID: userID, Status: "static"}); err != nil {
		t.Fatalf("insert state: %v", err)
	}
	seedCodexRun := func(iid int) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, harness)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, 3, 'codex')`, id, userID, repoID, iid, workerID)
		if err := wsvc.FreezeCodexBinding(ctx, userID, id, aliasID, "api_key"); err != nil {
			t.Fatalf("freeze: %v", err)
		}
		return id
	}
	runID := seedCodexRun(1)
	releasedRun := seedCodexRun(2)
	exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, releasedRun)
	claudeRun := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	      VALUES ($1, $2, $3, 'issue', 3, 't', 'd', 'running', $4, 3)`, claudeRun, userID, repoID, workerID)

	// The echo a worker sends is the claim's context: the PRD section 1 layout, derived here
	// independently of the code under test (workersvc pins the same layout against fixtures).
	var secretID uuid.UUID
	var rev int64
	if err := pool.QueryRow(ctx, `SELECT codex_secret_id, codex_material_revision FROM runs WHERE id = $1`, runID).Scan(&secretID, &rev); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{"uzi-codex-catalog-ctx-v1", userID.String(), secretID.String(), "api_key", strconv.FormatInt(rev, 10), ""}, "\x00")))
	echo := hex.EncodeToString(sum[:])

	do := func(token string, id uuid.UUID, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+id.String()+"/codex-catalog-default", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	body := func(mod map[string]any) []byte {
		m := map[string]any{"claim_generation": 3, "catalog_context": echo, "model": "gpt-6.1-sol", "runtime_version": "0.164.0", "default_effort": "low"}
		for k, v := range mod {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		b, _ := json.Marshal(m)
		return b
	}
	rowCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM codex_catalog_defaults WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	reasonOf := func(rec *httptest.ResponseRecorder) string {
		var b map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		return b["reason"]
	}
	disposition := func(rec *httptest.ResponseRecorder) string {
		var b map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		return b["disposition"]
	}

	if rec := do("", runID, body(nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", rec.Code)
	}

	// 400 claim_generation_required.
	for name, mod := range map[string]map[string]any{"missing": {"claim_generation": nil}, "zero": {"claim_generation": 0}, "negative": {"claim_generation": -1}} {
		rec := do(tok, runID, body(mod))
		if rec.Code != http.StatusBadRequest || reasonOf(rec) != "claim_generation_required" {
			t.Errorf("%s generation = %d %s, want 400 claim_generation_required", name, rec.Code, rec.Body.String())
		}
	}
	// 400 invalid_catalog_default.
	invalid := map[string][]byte{
		"unknown field":     body(map[string]any{"extra": 1}),
		"short context":     body(map[string]any{"catalog_context": strings.Repeat("a", 63)}),
		"non-hex context":   body(map[string]any{"catalog_context": strings.Repeat("z", 64)}),
		"empty model":       body(map[string]any{"model": ""}),
		"long model":        body(map[string]any{"model": strings.Repeat("m", 101)}),
		"empty version":     body(map[string]any{"runtime_version": ""}),
		"non-semver":        body(map[string]any{"runtime_version": "latest"}),
		"long version":      body(map[string]any{"runtime_version": "1.0.0-" + strings.Repeat("a", 30)}),
		"persistent effort": body(map[string]any{"default_effort": "persistent"}),
		"empty effort":      body(map[string]any{"default_effort": nil}),
		"not json":          []byte(`{`),
		"trailing value":    append(body(nil), []byte(` {}`)...),
		// Every declared field is length-bounded below 1024 bytes, so only the body size bound can
		// refuse a body that is otherwise valid: trailing JSON whitespace is accepted at any size.
		"over 1024 bytes": append(body(nil), []byte(strings.Repeat(" ", 1100))...),
	}
	for name, b := range invalid {
		rec := do(tok, runID, b)
		if rec.Code != http.StatusBadRequest || reasonOf(rec) != "invalid_catalog_default" {
			t.Errorf("%s = %d %s, want 400 invalid_catalog_default", name, rec.Code, rec.Body.String())
		}
	}
	if rec := do(tok, claudeRun, body(nil)); rec.Code != http.StatusBadRequest || reasonOf(rec) != "invalid_catalog_default" {
		t.Errorf("claude run = %d %s, want 400 invalid_catalog_default", rec.Code, rec.Body.String())
	}
	// 404 typed stale: a run another worker holds, and one that does not exist.
	for name, c := range map[string]struct {
		token string
		id    uuid.UUID
	}{"foreign worker": {otherTok, runID}, "unknown run": {tok, uuid.New()}} {
		rec := do(c.token, c.id, body(nil))
		if rec.Code != http.StatusNotFound || reasonOf(rec) != workersvc.ReceiptStale {
			t.Errorf("%s = %d %s, want 404 reason %q", name, rec.Code, rec.Body.String(), workersvc.ReceiptStale)
		}
	}
	// 409 stale_claim: a generation that is not current, and a released claim.
	for name, c := range map[string]struct {
		id  uuid.UUID
		mod map[string]any
	}{"stale generation": {runID, map[string]any{"claim_generation": 2}}, "released claim": {releasedRun, nil}} {
		rec := do(tok, c.id, body(c.mod))
		if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != `{"disposition":"stale_claim"}` {
			t.Errorf("%s = %d %s, want 409 {\"disposition\":\"stale_claim\"}", name, rec.Code, rec.Body.String())
		}
	}
	// 409 stale_context: the echo is not the frozen context.
	rec := do(tok, runID, body(map[string]any{"catalog_context": strings.Repeat("0", 64)}))
	if rec.Code != http.StatusConflict || disposition(rec) != "stale_context" {
		t.Errorf("echo != frozen = %d %s, want 409 stale_context", rec.Code, rec.Body.String())
	}
	if n := rowCount(); n != 0 {
		t.Fatalf("refused reports stored %d rows", n)
	}

	// 204 stored, and a repeat is idempotent.
	for i := 0; i < 2; i++ {
		if rec := do(tok, runID, body(nil)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("report %d = %d %s, want an empty 204", i, rec.Code, rec.Body.String())
		}
	}
	var gotEffort, gotVersion, gotModel string
	if err := pool.QueryRow(ctx, `SELECT default_effort, runtime_version, model FROM codex_catalog_defaults WHERE user_id = $1 AND context_id = $2`, userID, echo).Scan(&gotEffort, &gotVersion, &gotModel); err != nil {
		t.Fatalf("stored row: %v", err)
	}
	if gotEffort != "low" || gotVersion != "0.164.0" || gotModel != "gpt-6.1-sol" || rowCount() != 1 {
		t.Fatalf("stored row = %q %q %q (%d rows)", gotEffort, gotVersion, gotModel, rowCount())
	}

	// The credential is replaced after the claim: the same report is now stale_context.
	if _, err := q.BumpCodexMaterialRevision(ctx, store.BumpCodexMaterialRevisionParams{Status: "static", UserSecretID: aliasID, UserID: userID}); err != nil {
		t.Fatal(err)
	}
	rec = do(tok, runID, body(map[string]any{"model": "gpt-6-sol"}))
	if rec.Code != http.StatusConflict || disposition(rec) != "stale_context" {
		t.Errorf("report after rotation = %d %s, want 409 stale_context", rec.Code, rec.Body.String())
	}
	if n := rowCount(); n != 1 {
		t.Fatalf("rows after the rotated report = %d, want the original 1", n)
	}
}
