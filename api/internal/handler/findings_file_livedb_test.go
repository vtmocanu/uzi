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

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The live-DB half of PRD #333 M5: the file-finding POST and the dismiss POST are unreachable
// from a fake store — Handler.pool/.q are concrete types — and the things that MUST be right are
// invisible to a fake: the claim-first guarded UPDATE (concurrent double-file), the
// EnsureLabels-before-CreateIssue ordering (D5/R5), the server-assembled marker label, and the
// revert-on-forge-failure. The forge is stubbed by an httptest GitLab the real driver talks to,
// so the title/description/labels that reach CreateIssue — and the ORDER of EnsureLabels vs
// CreateIssue — are exactly what GitLab would observe. No live forge call is ever made.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

// findingForgeStub is an httptest GitLab that answers the three calls FileFinding drives —
// ListLabels (GET .../labels), CreateLabel (POST .../labels) and CreateIssue (POST .../issues) —
// recording each CreateIssue and the SEQUENCE of label-ensure vs issue-create so the test can
// assert the marker was ensured BEFORE the issue was filed. 403s CreateIssue when fail is set.
type findingForgeStub struct {
	mu           sync.Mutex
	server       *httptest.Server
	fail         bool
	creates      []forgeCreate
	seq          int
	markerSeq    int // sequence number of the CreateLabel that created the marker
	firstIssueAt int // sequence number of the first CreateIssue
	nextIID      int64
}

func newFindingForgeStub(t *testing.T, marker string) *findingForgeStub {
	t.Helper()
	fs := &findingForgeStub{nextIID: 200}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			// No labels exist yet, so EnsureLabels must CREATE the marker.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			body, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			name, _ := m["name"].(string)
			fs.mu.Lock()
			fs.seq++
			if name == marker {
				fs.markerSeq = fs.seq
			}
			fs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":7,"name":%q,"color":"#888888"}`, name)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
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
			fs.seq++
			if fs.firstIssueAt == 0 {
				fs.firstIssueAt = fs.seq
			}
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
			_, _ = fmt.Fprintf(w, `{"id":%d,"iid":%d,"project_id":1,"title":%q,"description":%q,"state":"opened","web_url":"https://forge.example/g/ra/-/issues/%d","labels":%s}`,
				iid, iid, title, desc, iid, jsonStr(labels))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(fs.server.Close)
	return fs
}

func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }

func (fs *findingForgeStub) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.creates)
}

// findingFileFixture is one seeded finding (evidence + open disposition) with the owner, a
// stranger, and the owner's connected repo. Fresh uuids per call — the LiveDB runner shares one
// database across the suite.
type findingFileFixture struct {
	owner    store.User
	stranger store.User
	repoID   uuid.UUID
	runID    uuid.UUID
	finding  store.IncidentalFinding
	location string
}

const findingTestMarker = "agent-found"

func fileFindingLiveDB(t *testing.T) (*Handler, *pgxpool.Pool, *store.Queries, *secretbox.Box, *findingForgeStub) {
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
	fs := newFindingForgeStub(t, findingTestMarker)
	h := &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		// The marker is the DEFAULT (agent-found); no finding_label row is set, so this also
		// proves the accessor's fallback drives the ensured + filed label.
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyUziLabel, Value: "uzi"},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}
	return h, pool, q, box, fs
}

func seedFindingFileFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, forgeURL string) findingFileFixture {
	t.Helper()
	f := findingFileFixture{
		owner:    store.User{ID: uuid.New(), Email: fmt.Sprintf("f5-owner-%s@e2e", uuid.NewString()[:8])},
		stranger: store.User{ID: uuid.New(), Email: fmt.Sprintf("f5-stranger-%s@e2e", uuid.NewString()[:8])},
		repoID:   uuid.New(),
		runID:    uuid.New(),
		location: "internal/sweep.go#sweeploop",
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.stranger.ID, f.stranger.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-o', 1, $4)`, connID, f.owner.ID, forgeURL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/ra', 'https://forge.example/g/ra', 'main', true)`, f.repoID, connID)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'd', 'completed', 'issue')`, f.runID, f.owner.ID, f.repoID)

	// One evidence row + an OPEN disposition at the coordinate (M2's write shape, done directly).
	finding, err := q.InsertFinding(ctx, store.InsertFindingParams{
		RunID: f.runID, UserID: f.owner.ID, RepoID: f.repoID,
		Location:      f.location,
		Title:         "leaked ticker in sweepLoop",
		DescriptionMd: "the sweeper starts a ticker it never stops",
		Labels:        []byte(`["perf"]`), Confidence: "high",
	})
	if err != nil {
		t.Fatalf("InsertFinding: %v", err)
	}
	f.finding = finding
	if _, err := q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
		UserID: f.owner.ID, RepoID: f.repoID, Location: f.location,
		ContentHash: "hash-1", LastTitle: finding.Title,
	}); err != nil {
		t.Fatalf("UpsertOpenDisposition: %v", err)
	}
	return f
}

// fileFindingReq builds a POST FileFinding request authenticated as user, with chi {id} and an
// optional JSON body (nil body = default, all-fields-omitted). Calling FileFinding directly
// exercises the handler without the router.
func fileFindingReq(user store.User, findingID uuid.UUID, body map[string]any) *http.Request {
	var reader io.Reader = strings.NewReader("{}")
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(http.MethodPost, "/x", reader)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", findingID.String())
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

func dismissFindingReq(user store.User, findingID uuid.UUID, body map[string]any) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(string(b)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", findingID.String())
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

func dispositionStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, location string) (string, *int64) {
	t.Helper()
	status, iid, _ := dispositionRow(ctx, t, pool, userID, repoID, location)
	return status, iid
}

// dispositionRow reads the settle-visible columns of one coordinate: its status, the stamped
// forge iid (nil when unfiled), and the stamped forge web URL (” when unfiled). It is the
// superset dispositionStatus wraps, used where the test also asserts filed_issue_url round-trips.
func dispositionRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, location string) (string, *int64, string) {
	t.Helper()
	var status, url string
	var iid pgtype.Int8
	err := pool.QueryRow(ctx,
		`SELECT status, filed_issue_iid, filed_issue_url FROM finding_dispositions WHERE user_id = $1 AND repo_id = $2 AND location = $3`,
		userID, repoID, location).Scan(&status, &iid, &url)
	if err != nil {
		t.Fatalf("read disposition: %v", err)
	}
	if iid.Valid {
		v := iid.Int64
		return status, &v, url
	}
	return status, nil, url
}

// ── Happy path: filing ensures the marker BEFORE CreateIssue, files it with the marker in the
// label set, and settles the coordinate to filed with the iid stamped ───────────────────────
func TestFileFindingHappyPathLiveDB(t *testing.T) {
	h, pool, q, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()
	f := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr := httptest.NewRecorder()
	// User supplies one extra label; the server marker must still be present and first.
	h.FileFinding(rr, fileFindingReq(f.owner, f.finding.ID, map[string]any{"labels": []string{"needs-triage"}}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Issue struct {
			IID    int64  `json:"iid"`
			WebURL string `json:"web_url"`
		} `json:"issue"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Issue.IID == 0 || resp.Warning != "" {
		t.Fatalf("want a created issue with no warning, got %+v", resp)
	}
	if fs.count() != 1 {
		t.Fatalf("forge CreateIssue calls = %d, want 1", fs.count())
	}
	// The marker was EnsureLabels-ed (CreateLabel) BEFORE the CreateIssue (D5/R5).
	fs.mu.Lock()
	markerSeq, issueSeq := fs.markerSeq, fs.firstIssueAt
	fs.mu.Unlock()
	if markerSeq == 0 {
		t.Errorf("the marker label was never ensured on the forge")
	}
	if markerSeq <= 0 || markerSeq >= issueSeq {
		t.Errorf("EnsureLabels(marker) must precede CreateIssue: markerSeq=%d issueSeq=%d", markerSeq, issueSeq)
	}
	// The filed labels include the marker, and never omit it for the client's selection.
	labels := fs.creates[0].Labels
	if !containsStr(labels, findingTestMarker) {
		t.Errorf("filed labels %v must include the server marker %q", labels, findingTestMarker)
	}
	if labels[0] != findingTestMarker {
		t.Errorf("marker must be first in %v", labels)
	}
	if !containsStr(labels, "needs-triage") {
		t.Errorf("filed labels %v must include the sanitised user selection", labels)
	}
	// The coordinate settled to filed with the iid AND the forge web URL stamped (FIX 1: the
	// backlog links a filed coordinate through filed_issue_url, so the settle must record it).
	status, iid, url := dispositionRow(ctx, t, pool, f.owner.ID, f.repoID, f.location)
	if status != "filed" || iid == nil || *iid != resp.Issue.IID {
		t.Fatalf("disposition not settled: status=%s iid=%v (issue %d)", status, iid, resp.Issue.IID)
	}
	if url == "" || url != resp.Issue.WebURL {
		t.Fatalf("settle must stamp filed_issue_url; got %q, want the created issue web_url %q", url, resp.Issue.WebURL)
	}
	_ = q
}

// ── Default text resolves from the STORED row; the agent title is not overridable via an omitted
// body, and a supplied edit is re-sanitised ──────────────────────────────────────────────────
func TestFileFindingResolvesStoredTextAndReSanitizesEditsLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()

	// (a) Omitted body → the stored, template-rendered text is filed (agent text never raw).
	fa := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)
	rr := httptest.NewRecorder()
	h.FileFinding(rr, fileFindingReq(fa.owner, fa.finding.ID, nil))
	if rr.Code != http.StatusCreated {
		t.Fatalf("default file: status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	got := fs.creates[0]
	if got.Title != "leaked ticker in sweepLoop" {
		t.Errorf("default title = %q, want the stored title", got.Title)
	}
	if !strings.Contains(got.Description, "the sweeper starts a ticker it never stops") {
		t.Errorf("default description must carry the stored body, got:\n%s", got.Description)
	}
	if !strings.Contains(got.Description, "Found by") {
		t.Errorf("default description must carry the provenance footer, got:\n%s", got.Description)
	}

	// (b) A supplied hostile title/description edit is re-run through the write-boundary
	// sanitisers: no leading quick-action slash, no CR/LF split in the title; the unfenced
	// quick-action line is stripped from the body.
	fb := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)
	rr2 := httptest.NewRecorder()
	h.FileFinding(rr2, fileFindingReq(fb.owner, fb.finding.ID, map[string]any{
		"title":       "/label ~x\r\nreal title",
		"description": "real body\n/label ~unfenced-autopilot\ntail",
	}))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("edited file: status = %d, want 201; body=%s", rr2.Code, rr2.Body.String())
	}
	edited := fs.creates[1]
	if strings.HasPrefix(edited.Title, "/") || strings.ContainsAny(edited.Title, "\r\n") {
		t.Errorf("edited title not re-sanitised: %q", edited.Title)
	}
	if strings.Contains(edited.Description, "~unfenced-autopilot") {
		t.Errorf("edited body's unfenced quick-action survived the write boundary:\n%s", edited.Description)
	}
}

// ── Concurrency: N overlapping POSTs on one coordinate file EXACTLY ONE issue, the losers 409 ─
func TestFileFindingConcurrentDoubleFileLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()
	f := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

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
			h.FileFinding(rr, fileFindingReq(f.owner, f.finding.ID, nil))
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
	// The database is the real assertion: exactly one forge issue and one filed disposition.
	if fs.count() != 1 {
		t.Errorf("forge CreateIssue calls = %d, want exactly 1", fs.count())
	}
	status, iid := dispositionStatus(ctx, t, pool, f.owner.ID, f.repoID, f.location)
	if status != "filed" || iid == nil {
		t.Errorf("disposition = %s (iid %v), want exactly one filed", status, iid)
	}
}

// ── A sequential second file on an already-filed coordinate is a 409 that never reaches the forge
func TestFileFindingAlreadyFiledConflictLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()
	f := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr1 := httptest.NewRecorder()
	h.FileFinding(rr1, fileFindingReq(f.owner, f.finding.ID, nil))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first file: status = %d, want 201; body=%s", rr1.Code, rr1.Body.String())
	}
	rr2 := httptest.NewRecorder()
	h.FileFinding(rr2, fileFindingReq(f.owner, f.finding.ID, nil))
	if rr2.Code != http.StatusConflict {
		t.Fatalf("second file: status = %d, want 409; body=%s", rr2.Code, rr2.Body.String())
	}
	if fs.count() != 1 {
		t.Errorf("forge creates = %d, want 1 (the 409 must not reach the forge)", fs.count())
	}
}

// ── Forge reject: the claim reverts (coordinate back to open, re-fileable) and returns 502 ────
func TestFileFindingForgeRejectRevertsLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()
	f := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	fs.mu.Lock()
	fs.fail = true
	fs.mu.Unlock()
	rr := httptest.NewRecorder()
	h.FileFinding(rr, fileFindingReq(f.owner, f.finding.ID, nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("forge reject: status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	// The claim reverted: the coordinate is open again, no iid stamped.
	if status, iid := dispositionStatus(ctx, t, pool, f.owner.ID, f.repoID, f.location); status != "open" || iid != nil {
		t.Fatalf("claim must revert to open on forge failure; got status=%s iid=%v", status, iid)
	}
	// And it is fileable again once the forge recovers.
	fs.mu.Lock()
	fs.fail = false
	fs.mu.Unlock()
	rr2 := httptest.NewRecorder()
	h.FileFinding(rr2, fileFindingReq(f.owner, f.finding.ID, nil))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("retry after forge recovery: status = %d, want 201; body=%s", rr2.Code, rr2.Body.String())
	}
}

// ── Non-owner filing a foreign finding id → 404, no forge call ────────────────────────────────
func TestFileFindingNonOwnerNotFoundLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()
	f := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr := httptest.NewRecorder()
	h.FileFinding(rr, fileFindingReq(f.stranger, f.finding.ID, nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-owner file: status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 0 {
		t.Errorf("a refused caller reached the forge %d times, want 0", fs.count())
	}
	if status, _ := dispositionStatus(ctx, t, pool, f.owner.ID, f.repoID, f.location); status != "open" {
		t.Errorf("a refused file must leave the coordinate open, got %s", status)
	}
}

// ── Dismiss: records the reason; missing/invalid → 400; non-owner → 404; the DB CHECK rejects a
// reasonless dismissal ───────────────────────────────────────────────────────────────────────
func TestDismissFindingLiveDB(t *testing.T) {
	h, pool, q, box, fs := fileFindingLiveDB(t)
	ctx := context.Background()

	// Missing reason → 400 (before the DB is touched).
	fMissing := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)
	rr := httptest.NewRecorder()
	h.DismissFinding(rr, dismissFindingReq(fMissing.owner, fMissing.finding.ID, map[string]any{}))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	// Invalid reason → 400.
	rrBad := httptest.NewRecorder()
	h.DismissFinding(rrBad, dismissFindingReq(fMissing.owner, fMissing.finding.ID, map[string]any{"reason": "meh"}))
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("invalid reason: status = %d, want 400; body=%s", rrBad.Code, rrBad.Body.String())
	}

	// Non-owner → 404.
	rrStranger := httptest.NewRecorder()
	h.DismissFinding(rrStranger, dismissFindingReq(fMissing.stranger, fMissing.finding.ID, map[string]any{"reason": "wont_do"}))
	if rrStranger.Code != http.StatusNotFound {
		t.Fatalf("non-owner dismiss: status = %d, want 404; body=%s", rrStranger.Code, rrStranger.Body.String())
	}

	// A valid reason dismisses the coordinate and records the reason.
	fOK := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)
	rrOK := httptest.NewRecorder()
	h.DismissFinding(rrOK, dismissFindingReq(fOK.owner, fOK.finding.ID, map[string]any{"reason": "not_an_issue"}))
	if rrOK.Code != http.StatusOK {
		t.Fatalf("dismiss: status = %d, want 200; body=%s", rrOK.Code, rrOK.Body.String())
	}
	var status, reason string
	if err := pool.QueryRow(ctx,
		`SELECT status, dismiss_reason FROM finding_dispositions WHERE user_id = $1 AND repo_id = $2 AND location = $3`,
		fOK.owner.ID, fOK.repoID, fOK.location).Scan(&status, &reason); err != nil {
		t.Fatalf("read disposition: %v", err)
	}
	if status != "dismissed" || reason != "not_an_issue" {
		t.Fatalf("disposition = (%s, %s), want (dismissed, not_an_issue)", status, reason)
	}

	// The DB CHECK ((status='dismissed') = (reason IS NOT NULL)) is the backstop: a reasonless
	// dismissal at the query layer errors rather than silently landing.
	fCheck := seedFindingFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)
	if _, err := q.DismissFinding(ctx, store.DismissFindingParams{
		DismissReason: pgtype.Text{Valid: false}, // NULL reason
		UserID:        fCheck.owner.ID, RepoID: fCheck.repoID, Location: fCheck.location,
	}); err == nil {
		t.Fatal("DismissFinding with a NULL reason must violate the status/reason CHECK, got nil error")
	}
}

func containsStr(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
