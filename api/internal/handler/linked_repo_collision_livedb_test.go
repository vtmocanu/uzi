package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/poller"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// collisionForge is the forge both connections talk to: it answers one issue and
// accepts the writes, so a call that reaches it is visible in its recorder.
type collisionForge struct{ forgetest.BaseFake }

func (*collisionForge) GetIssue(_ context.Context, _, iid int64) (forge.Issue, error) {
	return forge.Issue{IID: iid, Title: "forge card", State: "opened", Description: "from the forge", Labels: []string{"uzi"}, UpdatedAt: time.Now()}, nil
}
func (*collisionForge) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, nil
}
func (*collisionForge) ListLabels(context.Context, int64) ([]forge.Label, error) { return nil, nil }
func (*collisionForge) GetMergeRequest(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{IID: iid, State: forge.MRStateOpened}, nil
}

type pairRepoStore struct {
	q     *store.Queries
	repos map[uuid.UUID]bool
}

func (p pairRepoStore) ListEnabledReposWithConnections(ctx context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	rows, err := p.q.ListEnabledReposWithConnections(ctx)
	var out []store.ListEnabledReposWithConnectionsRow
	for _, r := range rows {
		if p.repos[r.ID] {
			out = append(out, r)
		}
	}
	return out, err
}

func issueSideCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		for _, p := range []string{"ListIssues(", "GetIssue(", "ListLabels(", "EnsureLabels(", "UpdateIssueLabels(", "ListIssueLabelEvents(", "ListIssueComments("} {
			if strings.HasPrefix(c, p) {
				out = append(out, c)
			}
		}
	}
	return out
}

// PRD #4173 M4 acceptance "collision": two forge connections, two repos with the SAME
// forge project id, one linked to YouTrack and one not. Each repo's issue side, a poller
// tick and a handler issue read reach only its own fake.
func TestLinkedAndUnlinkedRepoWithTheSameForgeProjectIDDoNotCollideLiveDB(t *testing.T) {
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

	recA := forgetest.NewRecorder(&collisionForge{})
	recB := forgetest.NewRecorder(&collisionForge{})
	urlA := "https://" + uuid.NewString() + ".forge-a.test"
	urlB := "https://" + uuid.NewString() + ".forge-b.test"
	svc := forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, func(_ forge.Type, u, _ string, _ time.Duration) (forge.Forge, error) {
		if u == urlA {
			return recA, nil
		}
		return recB, nil
	})
	itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	svc.SetIssueTrackerDrivers(itSvc)
	h.svc = svc

	sealed, err := svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}
	userA := store.User{ID: uuid.New(), Email: fmt.Sprintf("col-a-%s@e2e", uuid.NewString()[:8])}
	userB := store.User{ID: uuid.New(), Email: fmt.Sprintf("col-b-%s@e2e", uuid.NewString()[:8])}
	connA, connB, repoL, repoP := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x'), ($3, $4, 'x')`, userA.ID, userA.Email, userB.ID, userB.Email)
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot-a', 1, $4), ($5, $6, 'gitlab', $7, 'bot-b', 2, $4)`, connA, userA.ID, urlA, sealed, connB, userB.ID, urlB)
	yt, err := itSvc.Connect(ctx, userA.ID, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	const projectID = 77 // the SAME forge project id on both connections
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, $3, $4, 'https://forge-a.test/g/l', 'main', true, $5, $6, $7, $8, $9, now())`,
		repoL, connA, projectID, "g/linked-"+repoL.String(), yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge-b.test/g/p', 'main', true)`, repoP, connB, projectID, "g/plain-"+repoP.String())
	issueN := srv.SeedIssue("a youtrack card", "yt body", youtracktest.EligibilityTagID)

	// 1. The resolver. The linked repo reaches only YouTrack, the plain one only its forge.
	srv.ResetRequests()
	trL, err := svc.IssueTrackerForRepo(ctx, userA.ID, repoL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := trL.GetIssue(ctx, projectID, issueN)
	if err != nil {
		t.Fatalf("linked GetIssue: %v", err)
	}
	if got.Title != "a youtrack card" {
		t.Fatalf("linked repo read %+v, want the YouTrack card", got)
	}
	if len(srv.Requests()) == 0 {
		t.Fatal("the YouTrack fake saw no request for the linked repo")
	}
	if c := issueSideCalls(recA.Calls()); len(c) != 0 {
		t.Fatalf("the linked repo's forge saw issue-side calls: %v", c)
	}
	ytBefore := len(srv.Requests())
	trP, err := svc.IssueTrackerForRepo(ctx, userB.ID, repoP)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := trP.GetIssue(ctx, projectID, issueN); err != nil || got.Title != "forge card" {
		t.Fatalf("plain GetIssue = %+v, %v", got, err)
	}
	if len(srv.Requests()) != ytBefore {
		t.Fatal("the plain repo's read reached YouTrack")
	}
	if c := issueSideCalls(recB.Calls()); len(c) != 1 {
		t.Fatalf("the plain repo's forge issue-side calls = %v, want its one GetIssue", c)
	}
	// Binding is by repo row and owner: the other owner cannot resolve either repo.
	for name, call := range map[string]func() error{
		"linked, wrong owner": func() error { _, err := svc.IssueTrackerForRepo(ctx, userB.ID, repoL); return err },
		"plain, wrong owner":  func() error { _, err := svc.IssueTrackerForRepo(ctx, userA.ID, repoP); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: resolved", name)
		}
	}

	// 2. A poller tick per repo. The plain repo's tick makes issue-side forge calls and
	// never reaches YouTrack; the linked repo's tick makes no issue-side forge call.
	tick := func(repo uuid.UUID) {
		e := poller.New(svc, pairRepoStore{q: q, repos: map[uuid.UUID]bool{repo: true}}, time.Minute, 3)
		e.PollOnce(ctx)
	}
	ytBefore, aBefore, bBefore := len(srv.Requests()), len(recA.Calls()), len(recB.Calls())
	tick(repoP)
	if len(srv.Requests()) != ytBefore {
		t.Fatal("the plain repo's poller tick reached YouTrack")
	}
	if len(recA.Calls()) != aBefore {
		t.Fatal("the plain repo's poller tick reached the linked repo's forge")
	}
	if c := issueSideCalls(recB.Calls()[bBefore:]); len(c) == 0 {
		t.Fatal("the plain repo's poller tick made no issue-side forge call")
	}
	bBefore = len(recB.Calls())
	tick(repoL)
	if len(recB.Calls()) != bBefore {
		t.Fatal("the linked repo's poller tick reached the plain repo's forge")
	}
	if c := issueSideCalls(recA.Calls()[aBefore:]); len(c) != 0 {
		t.Fatalf("the linked repo's poller tick made issue-side forge calls: %v", c)
	}

	// 3. A handler issue read per repo.
	read := func(user store.User, repo uuid.UUID) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", repo.String())
		rctx.URLParams.Add("iid", strconv.FormatInt(issueN, 10))
		r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
		rr := httptest.NewRecorder()
		h.GetIssueDetail(rr, r)
		return rr
	}
	ytBefore, aBefore, bBefore = len(srv.Requests()), len(recA.Calls()), len(recB.Calls())
	if rr := read(userB, repoP); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "forge card") {
		t.Fatalf("plain handler read = %d %s", rr.Code, rr.Body.String())
	}
	if len(srv.Requests()) != ytBefore || len(recA.Calls()) != aBefore || len(recB.Calls()) != bBefore+1 {
		t.Fatalf("the plain handler read crossed over (yt %d->%d, A %d->%d, B %d->%d)", ytBefore, len(srv.Requests()), aBefore, len(recA.Calls()), bBefore, len(recB.Calls()))
	}
	ytBefore, bBefore = len(srv.Requests()), len(recB.Calls())
	rr := read(userA, repoL)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "a youtrack card") {
		t.Fatalf("linked handler read = %d %s", rr.Code, rr.Body.String())
	}
	if len(srv.Requests()) == ytBefore {
		t.Fatal("the linked handler read did not reach YouTrack")
	}
	if len(recB.Calls()) != bBefore {
		t.Fatal("the linked handler read reached the plain repo's forge")
	}
	if c := issueSideCalls(recA.Calls()[aBefore:]); len(c) != 0 {
		t.Fatalf("the linked handler read made issue-side forge calls: %v", c)
	}
}

// The same collision with ONE owner: two forge connections of the same user, forge
// project id 77 on both, one repo linked. The binding is by repo row, so the owner
// being the same does not blur the two.
func TestSameOwnerLinkedAndUnlinkedRepoWithTheSameForgeProjectIDLiveDB(t *testing.T) {
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

	recA := forgetest.NewRecorder(&collisionForge{})
	recB := forgetest.NewRecorder(&collisionForge{})
	urlA := "https://" + uuid.NewString() + ".forge-a.test"
	urlB := "https://" + uuid.NewString() + ".forge-b.test"
	svc := forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, func(_ forge.Type, u, _ string, _ time.Duration) (forge.Forge, error) {
		if u == urlA {
			return recA, nil
		}
		return recB, nil
	})
	itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	svc.SetIssueTrackerDrivers(itSvc)
	h.svc = svc
	sealed, err := svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}
	user := store.User{ID: uuid.New(), Email: fmt.Sprintf("col-same-%s@e2e", uuid.NewString()[:8])}
	connA, connB, repoL, repoP := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user.ID, user.Email)
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot-a', 1, $4), ($5, $2, 'gitlab', $6, 'bot-b', 2, $4)`, connA, user.ID, urlA, sealed, connB, urlB)
	yt, err := itSvc.Connect(ctx, user.ID, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	const projectID = 77
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, $3, $4, 'https://forge-a.test/g/l', 'main', true, $5, $6, $7, $8, $9, now())`,
		repoL, connA, projectID, "g/linked-"+repoL.String(), yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge-b.test/g/p', 'main', true)`, repoP, connB, projectID, "g/plain-"+repoP.String())
	issueN := srv.SeedIssue("a youtrack card", "yt body", youtracktest.EligibilityTagID)
	srv.ResetRequests()

	read := func(repo uuid.UUID) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", repo.String())
		rctx.URLParams.Add("iid", strconv.FormatInt(issueN, 10))
		r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
		rr := httptest.NewRecorder()
		h.GetIssueDetail(rr, r)
		return rr
	}
	if rr := read(repoP); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "forge card") {
		t.Fatalf("plain read = %d %s", rr.Code, rr.Body.String())
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("the plain repo's read reached YouTrack (%d requests)", n)
	}
	if c := issueSideCalls(recB.Calls()); len(c) != 1 || len(issueSideCalls(recA.Calls())) != 0 {
		t.Fatalf("plain read: forge B %v, forge A %v", c, issueSideCalls(recA.Calls()))
	}
	if rr := read(repoL); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "a youtrack card") {
		t.Fatalf("linked read = %d %s", rr.Code, rr.Body.String())
	}
	if len(srv.Requests()) == 0 {
		t.Fatal("the linked repo's read did not reach YouTrack")
	}
	if len(issueSideCalls(recA.Calls())) != 0 || len(issueSideCalls(recB.Calls())) != 1 {
		t.Fatalf("the linked read touched a forge: A %v, B %v", recA.Calls(), recB.Calls())
	}
}
