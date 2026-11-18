package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type linkedHandlerFixture struct {
	h      *Handler
	pool   *pgxpool.Pool
	srv    *youtracktest.Server
	rec    *forgetest.Recorder
	user   store.User
	repo   uuid.UUID
	issueN int64
}

// newLinkedHandlerFixture seeds one enabled repo linked to the YouTrack fake, a cached
// card for one of its issues, and a Handler whose forgesvc resolves through the real
// issuetrackersvc. Every forge call lands in rec.
func newLinkedHandlerFixture(t *testing.T, withColumns bool) *linkedHandlerFixture {
	t.Helper()
	h, pool, q, box, _ := fileIssueLiveDB(t)
	ctx := context.Background()
	srv := youtracktest.NewServer(t)
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })

	rec := forgetest.NewRecorder(&collisionForge{})
	svc := forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return rec, nil })
	itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	svc.SetIssueTrackerDrivers(itSvc)
	h.svc = svc
	sealed, err := svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}
	f := &linkedHandlerFixture{h: h, pool: pool, srv: srv, rec: rec, repo: uuid.New(),
		user: store.User{ID: uuid.New(), Email: fmt.Sprintf("lw-%s@e2e", uuid.NewString()[:8])}}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	conn := uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.user.ID, f.user.Email)
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, f.user.ID, "https://"+uuid.NewString()+".forge.test", sealed)
	yt, err := itSvc.Connect(ctx, f.user.ID, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, 77, $3, 'https://forge.test/g/l', 'main', true, $4, $5, $6, $7, $8, now())`,
		f.repo, conn, "g/linked-"+f.repo.String(), yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	f.issueN = srv.SeedIssue("a youtrack card", "yt body", youtracktest.EligibilityTagID)
	exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, $2, 'card', 'opened', '[]', 'https://yt.test/i', now(), now())`, f.repo, f.issueN)
	if withColumns {
		exec(`INSERT INTO board_columns(repo_id,label_name,position) VALUES($1,'In Progress',0),($1,'Human Review',1)`, f.repo)
	}
	srv.ResetRequests()
	return f
}

func (f *linkedHandlerFixture) call(handler http.HandlerFunc, method string, body any) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "/x", rdr)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.repo.String())
	rctx.URLParams.Add("iid", strconv.FormatInt(f.issueN, 10))
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.user), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	handler(rr, r)
	return rr
}

func (f *linkedHandlerFixture) assertNothingSent(t *testing.T) {
	t.Helper()
	if n := len(f.srv.Requests()); n != 0 {
		t.Errorf("the issue tracker saw %d request(s), want none: %v", n, f.srv.Requests())
	}
	if c := f.rec.Calls(); len(c) != 0 {
		t.Errorf("the forge saw calls %v, want none", c)
	}
}

// Every issue write on a linked repo is a clear 409 and sends nothing anywhere.
func TestLinkedRepoIssueWritesAreRefusedWith409LiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	cases := []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"create issue", func() *httptest.ResponseRecorder {
			return f.call(f.h.CreateIssue, http.MethodPost, map[string]string{"title": "t", "description": "d"})
		}},
		{"move issue", func() *httptest.ResponseRecorder {
			return f.call(f.h.MoveIssue, http.MethodPost, map[string]string{"to_column": "In Progress"})
		}},
		{"promote issue", func() *httptest.ResponseRecorder {
			return f.call(f.h.PromoteIssue, http.MethodPost, nil)
		}},
		{"ensure labels", func() *httptest.ResponseRecorder {
			return f.call(f.h.EnsureRepoLabels, http.MethodPost, map[string][]string{"labels": {"uzi"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := tc.do()
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "issue writes on a repo linked to an issue tracker are not offered yet") {
				t.Fatalf("status = %d body = %s, want the 409 refusal", rr.Code, rr.Body.String())
			}
			f.assertNothingSent(t)
		})
	}
}

// assertNoWrites fails when the issue tracker saw anything but a read (GET) or the
// forge saw any call: the linked intake and the board seed only read.
func (f *linkedHandlerFixture) assertNoWrites(t *testing.T) {
	t.Helper()
	for _, r := range f.srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("the issue tracker saw a write request %s %s", r.Method, r.Path)
		}
	}
	if c := f.rec.Calls(); len(c) != 0 {
		t.Errorf("the forge saw calls %v, want none", c)
	}
}

// A manual issue sync on a linked repo is the linked intake: it reads the eligibility
// tag's listing and writes nothing to the issue tracker.
func TestLinkedRepoSyncRunsTheLinkedIntakeLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	rr := f.call(f.h.SyncRepo, http.MethodPost, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	if len(f.srv.Requests()) == 0 {
		t.Fatal("the linked sync never read the issue tracker")
	}
	f.assertNoWrites(t)
}

// Opening the board of a linked repo seeds its column rows without any label write; the
// intake it triggers is read-only.
func TestLinkedRepoBoardSeedsColumnsWithoutLabelWritesLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, false)
	rr := f.call(f.h.GetBoard, http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	n, err := f.h.q.CountBoardColumns(context.Background(), f.repo)
	if err != nil || n != int64(len(forgesvc.DefaultColumns)) {
		t.Fatalf("board columns = %d (%v), want the %d defaults", n, err, len(forgesvc.DefaultColumns))
	}
	f.assertNoWrites(t)
}

// The forgeFactory test seam is for an unlinked row only: a linked row through it is an
// error, so a test or wiring gap can never reach the forge for a linked repo's issues.
func TestForgeFactorySeamRefusesALinkedRow(t *testing.T) {
	built := 0
	h := &Handler{forgeFactory: func(string, string, []byte) (forge.Forge, error) {
		built++
		return &collisionForge{}, nil
	}}
	linked := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if tr, err := h.issueTrackerForRepo(context.Background(), uuid.New(), uuid.New(), linked, "gitlab", "https://x.test", nil); err == nil || tr != nil || built != 0 {
		t.Fatalf("linked row through the seam = (%v, %v), forge built %d times", tr, err, built)
	}
	if tr, err := h.issueTrackerForRepo(context.Background(), uuid.New(), uuid.New(), pgtype.UUID{}, "gitlab", "https://x.test", nil); err != nil || tr == nil || built != 1 {
		t.Fatalf("unlinked row through the seam = (%v, %v), built %d", tr, err, built)
	}
	if ch, err := h.codeHostForRepo(context.Background(), uuid.New(), uuid.New(), linked, "gitlab", "https://x.test", nil); err != nil || ch == nil {
		t.Fatalf("the code-host side ignores the link: (%v, %v)", ch, err)
	}
}
