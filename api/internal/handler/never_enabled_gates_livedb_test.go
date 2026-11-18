package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD 4173 D13: an operation on a repo that has never been enabled is refused with a
// 409 BEFORE any forge or issue tracker call. The fixture's forge is a tripwire: every
// request it receives is counted, and each test asserts the count stayed zero.

type neverEnabledFixture struct {
	h        *Handler
	pool     *pgxpool.Pool
	admin    store.User
	repoID   uuid.UUID // awaiting_first_enable = true
	goodRepo uuid.UUID // an ordinary enabled repo of the same owner
	runID    uuid.UUID
	recID    uuid.UUID
	findID   uuid.UUID
	dispID   uuid.UUID
	hits     *atomic.Int64
}

func seedNeverEnabledFixture(t *testing.T) *neverEnabledFixture {
	t.Helper()
	h, pool, q, box, _ := fileIssueLiveDB(t)
	ctx := context.Background()
	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	f := &neverEnabledFixture{
		h: h, pool: pool, hits: hits,
		admin:    store.User{ID: uuid.New(), Email: fmt.Sprintf("ne-%s@e2e", uuid.NewString()[:8]), IsAdmin: true},
		repoID:   uuid.New(),
		goodRepo: uuid.New(),
		runID:    uuid.New(),
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`, f.admin.ID, f.admin.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-ne', 1, $4)`, connID, f.admin.ID, srv.URL, sealed)
	// The marker is set only at INSERT (the guard refuses setting it back later), and no
	// issue number may be stored against this repo, so the review's run lives on goodRepo.
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled, awaiting_first_enable)
		 VALUES ($1, $2, 1, $3, 'https://forge.example/g/ne', 'main', false, true)`, f.repoID, connID, "g/ne-"+f.repoID.String()[:8])
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, $3, 'https://forge.example/g/ok', 'main', true)`, f.goodRepo, connID, "g/ok-"+f.goodRepo.String()[:8])
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'd', 'completed', 'issue')`, f.runID, f.admin.ID, f.goodRepo)

	recs, _ := json.Marshal([]map[string]string{{"category": "improve_agent", "target": "ne-" + f.repoID.String(), "rationale_md": "r", "confidence": "medium"}})
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: f.runID, UserID: f.admin.ID, Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete", Recommendations: recs,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	list, err := q.ListRecommendationsForReview(ctx, reviewID)
	if err != nil || len(list) != 1 {
		t.Fatalf("seed recommendation: %d rows, %v", len(list), err)
	}
	f.recID = list[0].ID

	finding, err := q.InsertFinding(ctx, store.InsertFindingParams{
		RunID: f.runID, UserID: f.admin.ID, RepoID: f.repoID,
		Location: "internal/ne.go#loop", Title: "t", DescriptionMd: "d", Labels: []byte(`[]`), Confidence: "high",
	})
	if err != nil {
		t.Fatalf("InsertFinding: %v", err)
	}
	f.findID = finding.ID
	if err := f.pool.QueryRow(ctx, `INSERT INTO finding_dispositions (user_id, repo_id, location, content_hash, last_title)
		VALUES ($1, $2, $3, 'h', 't') RETURNING id`, f.admin.ID, f.repoID, finding.Location).Scan(&f.dispID); err != nil {
		t.Fatalf("seed disposition: %v", err)
	}
	return f
}

func (f *neverEnabledFixture) request(method string, body any, params map[string]string) *http.Request {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "/x", rdr)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.admin), chi.RouteCtxKey, rctx))
}

func (f *neverEnabledFixture) wantRefused(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "never been enabled") {
		t.Errorf("body = %s, want the never-enabled message", rr.Body.String())
	}
	if n := f.hits.Load(); n != 0 {
		t.Errorf("the forge saw %d request(s), want 0", n)
	}
}

func TestNeverEnabledRepoRefusedBeforeForgeCallLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	ctx := context.Background()
	repoParam := map[string]string{"id": f.repoID.String()}

	cases := []struct {
		name string
		call func(w http.ResponseWriter)
	}{
		{"GetBoard", func(w http.ResponseWriter) { f.h.GetBoard(w, f.request(http.MethodGet, nil, repoParam)) }},
		{"SyncRepo", func(w http.ResponseWriter) { f.h.SyncRepo(w, f.request(http.MethodPost, nil, repoParam)) }},
		{"CreateIssue", func(w http.ResponseWriter) {
			f.h.CreateIssue(w, f.request(http.MethodPost, map[string]string{"title": "t", "description": "d"}, repoParam))
		}},
		{"CreateSchedule", func(w http.ResponseWriter) {
			f.h.CreateSchedule(w, f.request(http.MethodPost, map[string]any{"target": "prompt"}, repoParam))
		}},
		{"EnableCatalogSchedule", func(w http.ResponseWriter) {
			f.h.EnableCatalogSchedule(w, f.request(http.MethodPost, nil, map[string]string{"id": f.repoID.String(), "slug": "anything"}))
		}},
		{"FileFinding", func(w http.ResponseWriter) {
			f.h.FileFinding(w, f.request(http.MethodPost, map[string]any{}, map[string]string{"id": f.findID.String()}))
		}},
		{"FileFindingGroup", func(w http.ResponseWriter) {
			f.h.FileFindingGroup(w, f.request(http.MethodPost, map[string]any{"ids": []string{f.dispID.String()}}, nil))
		}},
		{"FileIssue", func(w http.ResponseWriter) {
			f.h.FileIssue(w, f.request(http.MethodPost, map[string]string{"repo_id": f.repoID.String(), "title": "t", "description": "d"},
				map[string]string{"id": f.runID.String(), "recID": f.recID.String()}))
		}},
		{"AdminFileJudgeIssue", func(w http.ResponseWriter) {
			f.h.AdminFileJudgeIssue(w, f.request(http.MethodPost, adminFileIssueRequest{
				Category: "improve_agent", Target: "ne-" + f.repoID.String(), RepoID: f.repoID.String(), Title: "t", Description: "d",
			}, nil))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.call(rr)
			f.wantRefused(t, rr)
		})
	}

	// A refused filing leaves its claim released: the coordinate is still open.
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM finding_dispositions WHERE repo_id = $1`, f.repoID).Scan(&status); err != nil || status != "open" {
		t.Errorf("finding disposition = %q (%v), want open", status, err)
	}
	var filed int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_filed_issues WHERE filed_repo_id = $1`, f.repoID).Scan(&filed); err != nil || filed != 0 {
		t.Errorf("recommendation filed rows = %d (%v), want 0", filed, err)
	}
}

func TestPatchScheduleRepointToNeverEnabledRepoLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	ctx := context.Background()
	schedID := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled)
		 VALUES ($1, $2, $3, 'prompt', 'do a thing', 'recurring', '0 3 * * *', 'UTC', now() + interval '1 day', true, 'active', true)`,
		schedID, f.admin.ID, f.goodRepo)

	rr := httptest.NewRecorder()
	f.h.PatchSchedule(rr, f.request(http.MethodPatch, map[string]any{"repo_id": f.repoID.String()}, map[string]string{"id": schedID.String()}))
	f.wantRefused(t, rr)
	var repo uuid.UUID
	if err := f.pool.QueryRow(ctx, `SELECT repo_id FROM run_schedules WHERE id = $1`, schedID).Scan(&repo); err != nil || repo != f.goodRepo {
		t.Errorf("schedule repo = %s (%v), want it unchanged", repo, err)
	}
}

// The database guard refuses an issue number on an awaiting repo with UZ413; the
// handler mapping turns that into the same 409 instead of a 500.
func TestNeverEnabledGuardErrorMapsTo409LiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 7, 'x', 'd', 'queued', 'issue')`, uuid.New(), f.admin.ID, f.repoID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "UZ413" {
		t.Fatalf("direct insert error = %v, want SQLSTATE UZ413", err)
	}
	if !errors.Is(store.MapNeverEnabled(err), store.ErrRepoNeverEnabled) {
		t.Fatalf("MapNeverEnabled(%v) did not map to ErrRepoNeverEnabled", err)
	}
	rr := httptest.NewRecorder()
	if !writeIfNeverEnabled(rr, err) {
		t.Fatal("writeIfNeverEnabled did not recognise the guard error")
	}
	f.wantRefused(t, rr)
	if writeIfNeverEnabled(httptest.NewRecorder(), errors.New("boom")) {
		t.Error("an unrelated error was mapped to the never-enabled refusal")
	}
}

// insertSchedule seeds a recurring schedule of the given target on repo for the fixture user.
func (f *neverEnabledFixture) insertSchedule(t *testing.T, repo uuid.UUID, target string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if target == "issue" {
		mustExecT(context.Background(), t, f.pool,
			`INSERT INTO run_schedules (id, user_id, repo_id, target, issue_iid, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled)
			 VALUES ($1, $2, $3, 'issue', 7, 'recurring', '0 3 * * *', 'UTC', now() + interval '1 day', true, 'active', true)`, id, f.admin.ID, repo)
		return id
	}
	mustExecT(context.Background(), t, f.pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled)
		 VALUES ($1, $2, $3, 'prompt', 'do a thing', 'recurring', '0 3 * * *', 'UTC', now() + interval '1 day', true, 'active', true)`, id, f.admin.ID, repo)
	return id
}

// The run and schedule entry points that reach the service or the scheduler answer 409 for
// an awaiting repo, with no forge call.
func TestNeverEnabledRunsAndSchedulesRefusedLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	ctx := context.Background()
	f.h.wsvc.SetForges(f.h.svc)
	f.h.scheduler = schedsvc.New(f.h.q, f.h.wsvc, f.h.svc, f.h.settings, nil, nil, 0, slog.Default())
	repoParam := map[string]string{"id": f.repoID.String()}

	// A prompt schedule can sit on an awaiting repo at the database level (no issue number),
	// which is what RunScheduleNow must still refuse.
	awaitingPrompt := f.insertSchedule(t, f.repoID, "prompt")
	goodIssue := f.insertSchedule(t, f.goodRepo, "issue")
	goodPrompt := f.insertSchedule(t, f.goodRepo, "prompt")

	chat, err := f.h.wsvc.CreateChatRun(ctx, f.admin.ID, "hello")
	if err != nil {
		t.Fatalf("CreateChatRun: %v", err)
	}
	prop, err := f.h.q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{
		RunID: chat.ID, RepoID: f.repoID, Title: "t", Description: "d", Labels: []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("CreateIssueProposal: %v", err)
	}

	cases := []struct {
		name string
		call func(w http.ResponseWriter)
	}{
		{"RunScheduleNow", func(w http.ResponseWriter) {
			f.h.RunScheduleNow(w, f.request(http.MethodPost, nil, map[string]string{"id": awaitingPrompt.String()}))
		}},
		{"CreateRun", func(w http.ResponseWriter) {
			f.h.CreateRun(w, f.request(http.MethodPost, map[string]any{"issue_iid": 7}, repoParam))
		}},
		{"CreateTaskRun", func(w http.ResponseWriter) {
			f.h.CreateTaskRun(w, f.request(http.MethodPost, map[string]any{"context": "do it"}, repoParam))
		}},
		{"CreateCIFixRun", func(w http.ResponseWriter) {
			f.h.CreateCIFixRun(w, f.request(http.MethodPost, map[string]any{"ref": "main"}, repoParam))
		}},
		{"ConfigureColumns", func(w http.ResponseWriter) {
			f.h.ConfigureColumns(w, f.request(http.MethodPut, map[string]any{"columns": []string{"a"}}, repoParam))
		}},
		{"ConfirmProposal", func(w http.ResponseWriter) {
			f.h.ConfirmProposal(w, f.request(http.MethodPost, nil, map[string]string{"id": chat.ID.String(), "pid": prop.ID.String()}))
		}},
		{"CloneSchedule prompt", func(w http.ResponseWriter) {
			f.h.CloneSchedule(w, f.request(http.MethodPost, map[string]any{"repo_id": f.repoID.String()}, map[string]string{"id": goodPrompt.String()}))
		}},
		{"CloneSchedule issue", func(w http.ResponseWriter) {
			f.h.CloneSchedule(w, f.request(http.MethodPost, map[string]any{"repo_id": f.repoID.String()}, map[string]string{"id": goodIssue.String()}))
		}},
		{"AddScheduleRepo prompt", func(w http.ResponseWriter) {
			f.h.AddScheduleRepo(w, f.request(http.MethodPost, map[string]any{"repo_id": f.repoID.String()}, map[string]string{"id": goodPrompt.String()}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.call(rr)
			f.wantRefused(t, rr)
		})
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM run_schedules WHERE repo_id = $1`, f.repoID).Scan(&n); err != nil || n != 1 {
		t.Errorf("schedules on the awaiting repo = %d (%v), want only the seeded one", n, err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM issue_proposals WHERE id = $1`, prop.ID).Scan(&status); err != nil || status != "pending" {
		t.Errorf("proposal status = %q (%v), want pending after the refused confirm", status, err)
	}
}
