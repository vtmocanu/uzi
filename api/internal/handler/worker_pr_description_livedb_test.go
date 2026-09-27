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

// TestWorkerPrDescriptionRoutesLiveDB drives PRD #1798's stage -> bind -> ack through the REAL
// h.Routes() table with a real worker Bearer token (RequireWorker), against the real schema,
// then reads the run back through GetRun and checks the pr_description overlay. A request with
// no credential is refused before any handler runs.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.
func TestWorkerPrDescriptionRoutesLiveDB(t *testing.T) {
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
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("prdesc-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 7, $3, 'https://forge.e2e/g/prdesc', 'main', true)`, repoID, connID, "g/prdesc-"+repoID.String())
	tok, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, "prdesc-"+workerID.String(), hash)
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
	      VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'running', $4, 6)`, runID, userID, repoID, workerID)

	post := func(op, token string, body any, out any) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+runID.String()+"/pr-description/"+op, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if out != nil && rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatalf("decode %s: %v (%s)", op, err, rec.Body.String())
			}
		}
		return rec
	}

	stageBody := apitypes.PrDescriptionStageRequest{
		ClaimGeneration: i64(6), Source: "lead_only",
		Fields: apitypes.PrDescriptionFields{
			Summary: "Adds a Delivered card. Resolves owner/repo#3 <img src=x onerror=y>",
			Changes: []string{"Web: a new card", "API: a new route"},
		},
		BaseSha: strings.Repeat("a", 40), HeadSha: strings.Repeat("b", 40), TargetBranch: "main",
	}
	if rec := post("stage", "", stageBody, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", rec.Code)
	}

	var staged apitypes.PrDescriptionStageResponse
	if rec := post("stage", tok, stageBody, &staged); rec.Code != http.StatusOK {
		t.Fatalf("stage = %d %s", rec.Code, rec.Body.String())
	}
	if s := staged.Version.Fields.Summary; strings.Contains(s, "Resolves owner") || strings.Contains(s, "<img") {
		t.Fatalf("staged summary not sanitized: %q", s)
	}
	var bound apitypes.PrDescriptionBindResponse
	if rec := post("bind", tok, apitypes.PrDescriptionBindRequest{
		ClaimGeneration: i64(6), VersionID: staged.Version.ID, MrIid: 77, RenderedRegionSha256: strings.Repeat("d", 64),
	}, &bound); rec.Code != http.StatusOK || bound.PR.LockVersion != 0 {
		t.Fatalf("bind = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("ack", tok, apitypes.PrDescriptionAckRequest{
		ClaimGeneration: i64(5), VersionID: staged.Version.ID, Outcome: "published",
	}, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"stale_claim"`) {
		t.Fatalf("stale ack = %d %s, want 409 stale_claim", rec.Code, rec.Body.String())
	}
	var acked apitypes.PrDescriptionAckResponse
	if rec := post("ack", tok, apitypes.PrDescriptionAckRequest{
		ClaimGeneration: i64(6), VersionID: staged.Version.ID, Outcome: "published", ExpectedLockVersion: 0,
	}, &acked); rec.Code != http.StatusOK || acked.PR.PublishedVersion == nil || acked.PR.LockVersion != 1 {
		t.Fatalf("ack = %d %s", rec.Code, rec.Body.String())
	}

	// GetRun's overlay carries the published description and the outcome.
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
	dto := body.Run
	if dto.PrDescription == nil || dto.PrDescription.MrIid != 77 || dto.PrDescription.Source != "lead_only" ||
		len(dto.PrDescription.Fields.Changes) != 2 || dto.PrDescription.HeadSha != strings.Repeat("b", 40) ||
		dto.PrDescriptionOutcome == nil || *dto.PrDescriptionOutcome != "published" {
		t.Fatalf("GetRun pr_description = %+v outcome=%v", dto.PrDescription, dto.PrDescriptionOutcome)
	}
	// Stage rides the per-worker proposal limiter on the REAL worker router (auditor M4): with a
	// budget of 1 the second stage is a 429 that stores nothing; bind/lookup/ack are not limited.
	router = h.WorkerRoutes(mw.NewLimiter(1, time.Hour, nil))
	countVersions := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pr_description_versions WHERE run_id = $1`, runID).Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	before := countVersions()
	if rec := post("stage", tok, stageBody, nil); rec.Code != http.StatusOK {
		t.Fatalf("first limited stage = %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("stage", tok, stageBody, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second limited stage = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if n := countVersions(); n != before+1 {
		t.Fatalf("versions = %d, want %d (the limited stage must store nothing)", n, before+1)
	}
	for i := 0; i < 3; i++ {
		if rec := post("lookup", tok, apitypes.PrDescriptionLookupRequest{ClaimGeneration: i64(6), MrIid: 77, RegionSha256: strings.Repeat("d", 64)}, nil); rec.Code != http.StatusOK {
			t.Fatalf("lookup %d = %d, want unlimited", i, rec.Code)
		}
	}
}
