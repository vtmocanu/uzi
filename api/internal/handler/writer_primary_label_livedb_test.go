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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #196 M4 guard test 1: EVERY writer keeps writing the PRIMARY label, never the
// run-eligible set. The compiler cannot help here — the primary and every member of
// the set are the same `string` type — so each writer needs a behavioural assertion.
//
// The design of the guard: settings whose run-eligible set DIFFERS from the primary
// (primary "PRD", eligible {PRD, bug}). A writer that had been handed the set would
// start labelling issues "bug"; asserting it applies exactly "PRD" catches that. The
// four writer call sites are Promote (board.go), the judge draft and the judge file
// (review_issue_draft.go / review_issue_file.go), and board issue creation
// (issues.go). The judge pair is covered here plus in review_issue_livedb_test.go,
// whose harness now carries the same differing eligible set so its [PRD, PRDLESS]
// assertions are writer guards too.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// boardWriterStub is an httptest GitLab that answers the forge calls the board writers
// make and captures the labels crossing the wire: CreateIssue (POST .../issues),
// EnsureLabels (GET/POST .../labels) and UpdateIssueLabels (PUT .../issues/:iid). Its
// only job is to record what label uzi chose to write.
type boardWriterStub struct {
	mu sync.Mutex
	// createLabels is the label list on the last CreateIssue (issues.go writer).
	createLabels []string
	// labelCreates is every label NAME EnsureLabels auto-created (Promote writer).
	labelCreates []string
	// issueUpdateAdds is the add_labels of the last UpdateIssue (Promote writer).
	issueUpdateAdds []string
	nextIID         int64
}

// splitLabels parses a go-gitlab LabelOptions field, which marshals to a
// comma-joined JSON STRING (client-go v2 types.go), tolerating the array form too.
func splitLabels(v any) []string {
	switch lv := v.(type) {
	case string:
		if lv == "" {
			return nil
		}
		return strings.Split(lv, ",")
	case []any:
		var out []string
		for _, x := range lv {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func newBoardWriterServer(t *testing.T, s *boardWriterStub) *httptest.Server {
	t.Helper()
	s.nextIID = 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/labels"):
			// EnsureLabels lists first; return none so it proceeds to create.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/labels"):
			name, _ := m["name"].(string)
			s.mu.Lock()
			s.labelCreates = append(s.labelCreates, name)
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":1,"name":%q}`, name)
		case r.Method == http.MethodPut && strings.Contains(path, "/issues/"):
			s.mu.Lock()
			s.issueUpdateAdds = splitLabels(m["add_labels"])
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":1,"iid":5,"state":"opened","web_url":"https://forge.example/x","labels":["PRD"]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/issues"):
			s.mu.Lock()
			s.createLabels = splitLabels(m["labels"])
			s.nextIID++
			iid := s.nextIID
			s.mu.Unlock()
			title, _ := m["title"].(string)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"id":%d,"iid":%d,"project_id":1,"title":%q,"state":"opened","web_url":"https://forge.example/g/w/-/issues/%d","labels":["PRD"]}`, iid, iid, title, iid)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// boardWriterFixture seeds a user, a forge connection pointing at the stub, and a repo
// the user owns, plus a handler wired with settings whose eligible set differs from
// the primary. The handler mirrors fileIssueLiveDB's wiring.
type boardWriterFixture struct {
	h      *Handler
	pool   *pgxpool.Pool
	user   store.User
	repoID uuid.UUID
}

func newBoardWriterFixture(ctx context.Context, t *testing.T, stub *boardWriterStub) boardWriterFixture {
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
	srv := newBoardWriterServer(t, stub)

	f := boardWriterFixture{
		pool:   pool,
		user:   store.User{ID: uuid.New(), Email: fmt.Sprintf("wr-%s@e2e", uuid.NewString()[:8])},
		repoID: uuid.New(),
	}
	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.user.ID, f.user.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-w', 9, $4)`, connID, f.user.ID, srv.URL, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/w', 'https://forge.example/g/w', 'main', true)`, f.repoID, connID)

	f.h = &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyPRDLabel, Value: "PRD"},
			{Key: settings.KeyPrdlessLabel, Value: "PRDLESS"},
			{Key: settings.KeyRunEligibleLabels, Value: "PRD,bug"},
			{Key: settings.KeyEligibleLabelWaivesPRDLink, Value: "true"},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}
	return f
}

func boardWriterReq(user store.User, repoID uuid.UUID, iid string, body string) *http.Request {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(http.MethodPost, "/x", rdr)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	if iid != "" {
		rctx.URLParams.Add("iid", iid)
	}
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

// TestPromoteWritesPrimaryNotEligibleSetLiveDB: Promote must apply the PRIMARY label,
// never a member of the run-eligible set. The seeded issue carries "bug" (eligible but
// NOT the primary); promoting it must add exactly "PRD".
func TestPromoteWritesPrimaryNotEligibleSetLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &boardWriterStub{}
	f := newBoardWriterFixture(ctx, t, stub)

	// A cached non-primary issue: it is on the board (bug is a default extra) and
	// promotable (not the self-improve tracker).
	mustExecT(ctx, t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, 5, 't', 'opened', '["bug"]'::jsonb, 'https://x', false, now(), now())`, f.repoID)

	rr := httptest.NewRecorder()
	f.h.PromoteIssue(rr, boardWriterReq(f.user, f.repoID, "5", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("promote status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.labelCreates) != 1 || stub.labelCreates[0] != "PRD" {
		t.Fatalf("EnsureLabels created %v, want exactly [PRD] — Promote must write the PRIMARY, never the run-eligible set", stub.labelCreates)
	}
	if len(stub.issueUpdateAdds) != 1 || stub.issueUpdateAdds[0] != "PRD" {
		t.Fatalf("UpdateIssue add_labels = %v, want exactly [PRD]", stub.issueUpdateAdds)
	}
}

// TestCreateIssueWritesPrimaryNotEligibleSetLiveDB: board issue creation must open the
// issue with the PRIMARY label only, never the run-eligible set.
func TestCreateIssueWritesPrimaryNotEligibleSetLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &boardWriterStub{}
	f := newBoardWriterFixture(ctx, t, stub)

	rr := httptest.NewRecorder()
	f.h.CreateIssue(rr, boardWriterReq(f.user, f.repoID, "", `{"title":"a new issue","description":"d"}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.createLabels) != 1 || stub.createLabels[0] != "PRD" {
		t.Fatalf("CreateIssue labels = %v, want exactly [PRD] — issue creation must write the PRIMARY, never the run-eligible set", stub.createLabels)
	}
}

// TestJudgeFileWritesPrimaryNotEligibleSetLiveDB is the explicitly-named M4 guard for
// the judge FILE writer (review_issue_file.go). The harness's eligible set is {PRD,
// bug}; the filed issue must carry exactly [PRD, PRDLESS] and never "bug".
func TestJudgeFileWritesPrimaryNotEligibleSetLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "Improve the reviewer", "d"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("file status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if fs.count() != 1 {
		t.Fatalf("forge creates = %d, want 1", fs.count())
	}
	got := fs.creates[0].Labels
	if len(got) != 2 || got[0] != "PRD" || got[1] != "PRDLESS" {
		t.Fatalf("filed labels = %v, want exactly [PRD PRDLESS] — the judge writer must use the PRIMARY, never the run-eligible set", got)
	}
	for _, l := range got {
		if l == "bug" {
			t.Fatalf("filed labels %v contain a run-eligible-set member; the writer must write the PRIMARY only", got)
		}
	}
}

// TestJudgeDraftWritesPrimaryNotEligibleSetLiveDB is the explicitly-named M4 guard for
// the judge DRAFT writer (review_issue_draft.go): the draft it renders must display
// the PRIMARY label set [PRD, PRDLESS], never a run-eligible-set member.
func TestJudgeDraftWritesPrimaryNotEligibleSetLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)

	rr := httptest.NewRecorder()
	h.GetIssueDraft(rr, draftReq(f.owner, f.runID, f.recID))
	if rr.Code != http.StatusOK {
		t.Fatalf("draft status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp draftResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	got := resp.Draft.Labels
	if len(got) != 2 || got[0] != "PRD" || got[1] != "PRDLESS" {
		t.Fatalf("draft labels = %v, want exactly [PRD PRDLESS] — the draft must use the PRIMARY, never the run-eligible set", got)
	}
}
