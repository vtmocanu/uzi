package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of PRD #68 M5: the file-issue POST and the draft GET are unreachable
// from a fake store — Handler.pool/.q are concrete types — and the things that MUST be
// right are invisible to a fake: the claim-first ON CONFLICT (concurrent double-POST), the
// settle+cache transaction, and the write-boundary sanitizer over the CLIENT body. The
// forge is stubbed by an httptest GitLab the real driver talks to, so the description that
// reaches CreateIssue is exactly what GitLab would receive.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// forgeStub is an httptest GitLab that captures every CreateIssue (title/description/labels
// as they cross the wire) and answers it — or 403s when fail is set (the forge-reject path).
type forgeStub struct {
	mu      sync.Mutex
	server  *httptest.Server
	fail    bool
	creates []forgeCreate
	nextIID int64
}

type forgeCreate struct {
	Title       string
	Description string
	Labels      []string
}

func newForgeStub(t *testing.T) *forgeStub {
	t.Helper()
	fs := &forgeStub{nextIID: 100}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			title, _ := m["title"].(string)
			desc, _ := m["description"].(string)
			var labels []string
			switch lv := m["labels"].(type) {
			case string:
				if lv != "" {
					labels = strings.Split(lv, ",")
				}
			case []any:
				for _, x := range lv {
					if s, ok := x.(string); ok {
						labels = append(labels, s)
					}
				}
			}
			fs.mu.Lock()
			fs.creates = append(fs.creates, forgeCreate{Title: title, Description: desc, Labels: labels})
			fail := fs.fail
			fs.nextIID++
			iid := fs.nextIID
			fs.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"403 Forbidden"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d,"iid":%d,"project_id":1,"title":%q,"description":%q,"state":"opened","web_url":"https://forge.example/g/ra/-/issues/%d","labels":["PRD","PRDLESS"]}`,
				iid, iid, title, desc, iid)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *forgeStub) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.creates)
}

func fileIssueLiveDB(t *testing.T) (*Handler, *pgxpool.Pool, *store.Queries, *secretbox.Box, *forgeStub) {
	t.Helper()
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
	fs := newForgeStub(t)
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyPRDLabel, Value: "PRD"},
			{Key: settings.KeyPrdlessLabel, Value: "PRDLESS"},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}
	return h, pool, q, box, fs
}

// fileFixture is one seeded review with a single recommendation, plus the two actors
// (owner + a distinct admin) and their repos. Fresh uuids per call — the LiveDB runner
// shares one database across the suite.
type fileFixture struct {
	owner     store.User
	admin     store.User
	stranger  store.User
	ownerRepo uuid.UUID
	adminRepo uuid.UUID
	runID     uuid.UUID
	reviewID  uuid.UUID
	recID     uuid.UUID
	category  string
	target    string
}

func seedFileFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, forgeURL string) fileFixture {
	t.Helper()
	f := fileFixture{
		owner:     store.User{ID: uuid.New(), Email: fmt.Sprintf("owner-%s@e2e", uuid.NewString()[:8])},
		admin:     store.User{ID: uuid.New(), Email: fmt.Sprintf("admin-%s@e2e", uuid.NewString()[:8]), IsAdmin: true},
		stranger:  store.User{ID: uuid.New(), Email: fmt.Sprintf("stranger-%s@e2e", uuid.NewString()[:8])},
		ownerRepo: uuid.New(),
		adminRepo: uuid.New(),
		runID:     uuid.New(),
		category:  "improve_agent",
		target:    "reviewer",
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	ownerConn, adminConn := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`, f.admin.ID, f.admin.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.stranger.ID, f.stranger.Email)
	// Both connections point their base_url at the stub forge, so a CreateIssue from
	// either owner's driver lands on the httptest server.
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-o', 1, $4)`, ownerConn, f.owner.ID, forgeURL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-a', 2, $4)`, adminConn, f.admin.ID, forgeURL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/ra', 'https://forge.example/g/ra', 'main', true)`, f.ownerRepo, ownerConn)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'g/rb', 'https://forge.example/g/rb', 'main', true)`, f.adminRepo, adminConn)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'd', 'completed', 'issue')`, f.runID, f.owner.ID, f.ownerRepo)

	recs, _ := json.Marshal([]map[string]string{{"category": f.category, "target": f.target, "rationale_md": "the reviewer skipped a check", "confidence": "medium"}})
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: f.runID, UserID: f.owner.ID, Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete", Recommendations: recs,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	f.reviewID = reviewID
	list, err := q.ListRecommendationsForReview(ctx, reviewID)
	if err != nil || len(list) != 1 {
		t.Fatalf("seed recommendation: %d rows, %v", len(list), err)
	}
	f.recID = list[0].ID
	return f
}

func mustExecT(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// fileIssueReq builds a POST FileIssue request authenticated as user, with chi {id}/{recID}
// and a JSON body. Calling FileIssue directly exercises the handler without the router.
func fileIssueReq(user store.User, runID, recID uuid.UUID, repoID uuid.UUID, title, description string) *http.Request {
	body, _ := json.Marshal(map[string]string{"repo_id": repoID.String(), "title": title, "description": description})
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(body)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	rctx.URLParams.Add("recID", recID.String())
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

func filedRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_filed_issues WHERE review_id = $1`, reviewID).Scan(&n); err != nil {
		t.Fatalf("count filed rows: %v", err)
	}
	return n
}

// ── Happy path: owner files against their own repo, forge succeeds ───────────────────────
func TestFileIssueOwnerHappyPathLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "Improve the reviewer", "make it report skipped checks"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Issue struct {
			IID int64 `json:"iid"`
		} `json:"issue"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Issue.IID == 0 || resp.Warning != "" {
		t.Fatalf("want a created issue with no warning, got %+v", resp)
	}
	// The forge saw exactly one create, labelled server-side PRD+PRDLESS (never from body).
	if fs.count() != 1 {
		t.Fatalf("forge CreateIssue calls = %d, want 1", fs.count())
	}
	if got := fs.creates[0].Labels; len(got) != 2 || got[0] != "PRD" || got[1] != "PRDLESS" {
		t.Fatalf("labels sent to forge = %v, want [PRD PRDLESS]", got)
	}
	// The claim settled: one filed row with filed_at set + the issue iid/url.
	var filedAt bool
	var iid int64
	var url string
	if err := pool.QueryRow(ctx,
		`SELECT filed_at IS NOT NULL, filed_issue_iid, filed_issue_url FROM recommendation_filed_issues WHERE review_id = $1`,
		f.reviewID).Scan(&filedAt, &iid, &url); err != nil {
		t.Fatalf("read filed row: %v", err)
	}
	if !filedAt || iid != resp.Issue.IID {
		t.Fatalf("filed row not settled: filed_at=%v iid=%d (issue %d)", filedAt, iid, resp.Issue.IID)
	}
	// The issues cache was upserted so the board card appears without a poll.
	var cached int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.ownerRepo, iid).Scan(&cached); err != nil {
		t.Fatalf("count cached issue: %v", err)
	}
	if cached != 1 {
		t.Errorf("issues cache rows = %d, want 1 (the just-created issue)", cached)
	}
}

// ── Authz: a non-owner non-admin cannot even see the review → 404 (never a leak) ─────────
func TestFileIssueNonOwnerNotFoundLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.stranger, f.runID, f.recID, f.ownerRepo, "t", "d"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-owner non-admin; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 0 {
		t.Errorf("a refused caller reached the forge %d times, want 0", fs.count())
	}
	if n := filedRowCount(ctx, t, pool, f.reviewID); n != 0 {
		t.Errorf("a refused caller left %d claim rows, want 0", n)
	}
}

// ── Authz: an admin may READ another user's review but WRITE only to a repo they own ─────
func TestFileIssueAdminNonOwnedRepoNotFoundLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	// Admin can see the review, but files against the OWNER's repo, which the admin does
	// not own → caller-owns-repo (GetRepoForUser) 404s, before any claim or forge call.
	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.admin, f.runID, f.recID, f.ownerRepo, "t", "d"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("admin filing a non-owned repo: status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 0 || filedRowCount(ctx, t, pool, f.reviewID) != 0 {
		t.Errorf("a non-owned-repo write must touch neither forge (%d) nor claim table (%d)", fs.count(), filedRowCount(ctx, t, pool, f.reviewID))
	}
}

func TestFileIssueAdminOwnRepoFilesLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	// The accepted confused-deputy path (Decision 8): an admin files ANOTHER user's review
	// text into the admin's OWN repo. Allowed, with provenance surfaced in the UI.
	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.admin, f.runID, f.recID, f.adminRepo, "Improve the reviewer", "d"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin filing their own repo: status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 1 {
		t.Errorf("forge creates = %d, want 1", fs.count())
	}
}

// ── 409: a coordinate already filed (or mid-filing) cannot be filed again ────────────────
func TestFileIssueAlreadyFiledConflictLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr1 := httptest.NewRecorder()
	h.FileIssue(rr1, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "t", "d"))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first file: status = %d, want 201; body=%s", rr1.Code, rr1.Body.String())
	}
	rr2 := httptest.NewRecorder()
	h.FileIssue(rr2, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "t", "d"))
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second file: status = %d, want 409; body=%s", rr2.Code, rr2.Body.String())
	}
	if fs.count() != 1 {
		t.Errorf("forge creates = %d, want 1 (the 409 must not reach the forge)", fs.count())
	}
}

// ── Concurrency: N overlapping POSTs on one coordinate file EXACTLY ONE issue ─────────────
func TestFileIssueConcurrentDoublePostLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	const racers = 8
	var wg sync.WaitGroup
	codes := make([]int, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rr := httptest.NewRecorder()
			h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "t", "d"))
			codes[i] = rr.Code
		}(i)
	}
	close(start)
	wg.Wait()

	var created, conflict int
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if created != 1 {
		t.Errorf("%d POSTs succeeded, want exactly 1 — the claim-first guard is not atomic", created)
	}
	if conflict != racers-1 {
		t.Errorf("%d POSTs 409'd, want %d", conflict, racers-1)
	}
	// The database is the real assertion: exactly one forge issue and one settled row.
	if fs.count() != 1 {
		t.Errorf("forge CreateIssue calls = %d, want exactly 1", fs.count())
	}
	if n := filedRowCount(ctx, t, pool, f.reviewID); n != 1 {
		t.Errorf("filed rows = %d, want exactly 1", n)
	}
}

// ── Forge reject: the claim reverts, nothing persists, the coordinate is fileable again ───
func TestFileIssueForgeRejectRevertsLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	fs.mu.Lock()
	fs.fail = true
	fs.mu.Unlock()
	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "t", "d"))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("forge reject: status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if n := filedRowCount(ctx, t, pool, f.reviewID); n != 0 {
		t.Fatalf("the claim must revert on forge failure; got %d rows", n)
	}
	// The coordinate is fileable again once the forge recovers.
	fs.mu.Lock()
	fs.fail = false
	fs.mu.Unlock()
	rr2 := httptest.NewRecorder()
	h.FileIssue(rr2, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "t", "d"))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("retry after forge recovery: status = %d, want 201; body=%s", rr2.Code, rr2.Body.String())
	}
}

// ── created-with-warning: a settle that touches 0 rows (swept mid-flight) is NOT an error ─
// Exercised deterministically at the settle helper — the 0-rows branch is unreachable from
// the HTTP path without a fault seam, but this is the exact created-with-warning contract.
func TestFileIssueSettleZeroRowsWarnsLiveDB(t *testing.T) {
	h, pool, q, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	repo, err := q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: f.ownerRepo, UserID: f.owner.ID})
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	created := forge.Issue{IID: 999, WebURL: "https://forge.example/g/ra/-/issues/999", Title: "t", State: "opened", Labels: []string{"PRD", "PRDLESS"}}

	// A random claim id that does not exist → settle updates 0 rows → warnReclaimed.
	if warn := h.settleFiledIssue(ctx, uuid.New(), repo, created, "body"); warn == "" {
		t.Fatal("settleFiledIssue with a missing claim must return a created-with-warning message, got empty")
	}
	// A real, freshly settled claim → no warning.
	claimID, err := q.ClaimRecommendationFiledIssue(ctx, store.ClaimRecommendationFiledIssueParams{
		ReviewID: f.reviewID, Category: f.category, Target: f.target, FiledByUserID: pgUUIDOf(f.owner.ID),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if warn := h.settleFiledIssue(ctx, claimID, repo, created, "body"); warn != "" {
		t.Fatalf("a clean settle must return no warning, got %q", warn)
	}
	_ = fs
}

// ── Write-boundary: the sanitizer re-runs on the CLIENT body at the POST (Decision 10) ───
// Every UNFENCED quick-action family (incl. a CRLF-hidden one) is stripped; a FENCED
// quick-action + beacon survive as inert code (fence-aware, breakout-proof); a secret is
// redacted; and the labels reaching the forge are server-side PRD+PRDLESS, never the body.
func TestFileIssueWriteBoundarySanitizesClientBodyLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	awsKey := "AKIA" + strings.Repeat("A", 16)
	hostile := strings.Join([]string{
		"## What the judge found",
		"",
		"`````", // a 5-backtick fence; the inner ``` below must NOT close it
		"beacon ![](https://evil.example/p.png) and link [x](https://evil.example)",
		"```",
		"/label ~inside-fence-kept", // fenced quick-action → kept, inert (GitLab skips fenced)
		"`````",
		"/label ~unfenced-autopilot", // every UNFENCED family below → stripped
		"/relabel ~relabelme",
		"/assign @adminuser",
		"/close",
		"/move other/project",
		"/confidential",
		"a leaked key " + awsKey,
		"trailing text",
	}, "\n") +
		"\r\n/label ~crlf-sneaky\r\n" + // a CRLF (\r\n) -hidden quick-action → stripped
		"\r/label ~bare-cr\r" // a BARE-\r-hidden quick-action → stripped (MEDIUM-1 PoC parity)

	rr := httptest.NewRecorder()
	// The client may attach anything in the body; labels are NEVER taken from it.
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "Improve the reviewer", hostile))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 1 {
		t.Fatalf("forge creates = %d, want 1", fs.count())
	}
	got := fs.creates[0].Description

	// Fenced content is kept verbatim (inert): the quick-action and the beacon survive
	// ONLY because they are inside the fence — proving the strip is fence-aware and the
	// inner ``` did not break the fence out.
	for _, keep := range []string{"~inside-fence-kept", "![](https://evil.example/p.png)"} {
		if !strings.Contains(got, keep) {
			t.Errorf("fenced content %q was lost:\n%s", keep, got)
		}
	}
	// Every unfenced quick-action family — and the CRLF-hidden one — is stripped.
	for _, gone := range []string{"~unfenced-autopilot", "/relabel", "@adminuser", "/close", "/move other/project", "/confidential", "~crlf-sneaky", "~bare-cr"} {
		if strings.Contains(got, gone) {
			t.Errorf("unfenced quick-action %q survived the write-boundary strip:\n%s", gone, got)
		}
	}
	// The secret is redacted.
	if strings.Contains(got, awsKey) {
		t.Errorf("a secret in the client body reached the forge:\n%s", got)
	}
	// Labels are assembled server-side regardless of the body.
	if l := fs.creates[0].Labels; len(l) != 2 || l[0] != "PRD" || l[1] != "PRDLESS" {
		t.Errorf("labels to forge = %v, want [PRD PRDLESS]", l)
	}
}

// ── The TITLE is defanged at the POST handler too (SanitizeTitle, not only the body) ─────
func TestFileIssueTitleSanitizedAtPostLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	// A hostile title carrying a leading quick-action and a CRLF line-split must be
	// neutralized server-side: the forge-received title has NO leading "/" (the
	// quick-action cannot take effect) and NO CR/LF (it cannot split into a second line).
	// A regression that dropped SanitizeTitle(req.Title) at the handler fails HERE.
	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "/label ~x\r\nreal title", "d"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 1 {
		t.Fatalf("forge creates = %d, want 1", fs.count())
	}
	got := fs.creates[0].Title
	if strings.HasPrefix(got, "/") {
		t.Errorf("filed title must not open with a quick-action slash: %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("filed title must be a single line (no CR/LF): %q", got)
	}
}

func TestFileIssueEmptyTitleRejectedLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	// A title that sanitizes to empty (only slashes/whitespace) cannot file → 400, before
	// any repo lookup, claim, or forge call.
	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "/////", "d"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty-after-sanitize title: status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 0 || filedRowCount(ctx, t, pool, f.reviewID) != 0 {
		t.Errorf("a rejected title must touch neither forge (%d) nor claim table (%d)", fs.count(), filedRowCount(ctx, t, pool, f.reviewID))
	}
}

func pgUUIDOf(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// ── Draft GET authz (owner-or-admin) + the empty-picker (no-default) case ────────────────
func draftReq(user store.User, runID, recID uuid.UUID) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	rctx.URLParams.Add("recID", recID.String())
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

type draftResp struct {
	Draft struct {
		DefaultRepoID string   `json:"default_repo_id"`
		Title         string   `json:"title"`
		Labels        []string `json:"labels"`
		Provenance    string   `json:"provenance"`
		DefaultNote   string   `json:"default_note"`
	} `json:"draft"`
}

func TestGetIssueDraftAuthzLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	// Owner: 200, and the improve_agent default resolves to the judged run's repo (owned).
	rrOwner := httptest.NewRecorder()
	h.GetIssueDraft(rrOwner, draftReq(f.owner, f.runID, f.recID))
	if rrOwner.Code != http.StatusOK {
		t.Fatalf("owner draft: status = %d, want 200; body=%s", rrOwner.Code, rrOwner.Body.String())
	}
	var owner draftResp
	if err := json.Unmarshal(rrOwner.Body.Bytes(), &owner); err != nil {
		t.Fatalf("decode owner draft: %v", err)
	}
	if owner.Draft.DefaultRepoID != f.ownerRepo.String() {
		t.Errorf("owner default_repo_id = %q, want the run's repo %s", owner.Draft.DefaultRepoID, f.ownerRepo)
	}
	if len(owner.Draft.Labels) != 2 || owner.Draft.Labels[0] != "PRD" {
		t.Errorf("draft labels = %v, want [PRD PRDLESS]", owner.Draft.Labels)
	}

	// Admin reads another user's review: 200, but the run's repo is not the admin's, so the
	// default cannot resolve — empty picker with a reason (mock state D).
	rrAdmin := httptest.NewRecorder()
	h.GetIssueDraft(rrAdmin, draftReq(f.admin, f.runID, f.recID))
	if rrAdmin.Code != http.StatusOK {
		t.Fatalf("admin draft: status = %d, want 200; body=%s", rrAdmin.Code, rrAdmin.Body.String())
	}
	var admin draftResp
	_ = json.Unmarshal(rrAdmin.Body.Bytes(), &admin)
	if admin.Draft.DefaultRepoID != "" {
		t.Errorf("admin default_repo_id = %q, want empty (the run repo is not the admin's)", admin.Draft.DefaultRepoID)
	}
	if admin.Draft.DefaultNote == "" {
		t.Error("an empty default must carry a reason (mock state D)")
	}

	// Non-owner non-admin: 404 (a run they cannot see).
	rrStranger := httptest.NewRecorder()
	h.GetIssueDraft(rrStranger, draftReq(f.stranger, f.runID, f.recID))
	if rrStranger.Code != http.StatusNotFound {
		t.Fatalf("stranger draft: status = %d, want 404", rrStranger.Code)
	}
}
