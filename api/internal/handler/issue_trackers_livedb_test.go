package handler

// Live-DB tests of the issue tracker routes (PRD #4173 M4) through the real router: owner
// scoping, the allowlist, the link and its refusals, and the connection delete and reconnect
// rules. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/issuetrackertest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const itTestHost = "https://issues.example.com"

var (
	itBot   = issuetracker.Account{ID: "1-1", Login: "bot"}
	itOther = issuetracker.Account{ID: "1-2", Login: "maintainer"}
)

// itDriver answers for the account its token names: a token ending in "-other" is the
// second account.
type itDriver struct {
	issuetrackertest.BaseFake
	token string
}

func (d *itDriver) VerifyToken(context.Context) (issuetracker.Account, error) {
	if strings.HasSuffix(d.token, "-other") {
		return itOther, nil
	}
	return itBot, nil
}

func (*itDriver) ListProjects(context.Context) ([]issuetracker.Project, error) {
	return []issuetracker.Project{{ID: "0-1", ShortName: "UZI", Name: "Uzi\u202e"}}, nil
}

func (*itDriver) ListTags(context.Context) ([]issuetracker.Tag, error) {
	all := issuetracker.Group{ID: "675-0", Name: "All Users", AllUsers: true}
	return []issuetracker.Tag{
		{ID: "5-1", Name: "uzi-ready", Owner: itBot, Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{all}}},
		{ID: "5-2", Name: "private", Owner: itBot},
		// Hostile names: a bidi override and a newline must not reach the browser.
		{ID: "5-3", Name: "evil\u202ename\n", Owner: issuetracker.Account{ID: "1-2", Login: "mal\u202elory"},
			Use: issuetracker.SharingSettings{Groups: []issuetracker.Group{{ID: "675-9", Name: "gr\noup\u200b"}}, Users: []issuetracker.Account{{ID: "1-3", Login: "us\u202eer"}}}},
	}, nil
}

type itFixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	q         *store.Queries
	h         *Handler
	router    http.Handler
	user      uuid.UUID
	jwt       string
	forgeConn uuid.UUID
}

func newITFixture(t *testing.T, allowed bool) *itFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	q := store.New(pool)
	allow := func(u string) bool { return allowed && u == itTestHost }
	h.SetIssueTrackerService(issuetrackersvc.New(q, h.box, allow,
		func(_ issuetracker.Kind, o issuetracker.Options) (issuetracker.Driver, error) {
			return &itDriver{token: o.Token}, nil
		}).UseTransactions(pool, func(tx pgx.Tx) issuetrackersvc.Querier { return q.WithTx(tx) }))
	f := &itFixture{t: t, pool: pool, q: q, h: h, router: router, user: cliSeedUser(t, pool, false)}
	f.jwt = cliMintJWT(t, pool, f.user)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, f.user) })
	f.forgeConn = uuid.New()
	cliMustExec(t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	                      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, f.forgeConn, f.user, "https://"+f.forgeConn.String()+".e2e", []byte{0x1})
	return f
}

// freshRepo is a repo as the membership listing creates it.
func (f *itFixture) freshRepo() uuid.UUID {
	f.t.Helper()
	r, err := f.q.UpsertRepo(context.Background(), store.UpsertRepoParams{
		ConnectionID: f.forgeConn, ForgeProjectID: int64(uuid.New().ID()), PathWithNamespace: "g/r", WebUrl: "https://forge.e2e/g/r",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return r.ID
}

func (f *itFixture) do(jwt, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return cookieReq(f.t, f.router, method, path, jwt, body)
}

func (f *itFixture) connect(jwt, token string) (int, string, uuid.UUID) {
	f.t.Helper()
	rec := f.do(jwt, http.MethodPost, "/api/issue-trackers/connections",
		fmt.Sprintf(`{"kind":"youtrack","base_url":%q,"token":%q}`, itTestHost, token))
	var out struct {
		Connection struct {
			ID uuid.UUID `json:"id"`
		} `json:"connection"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, rec.Body.String(), out.Connection.ID
}

func itToken(suffix string) string { return "perm-it-route-token-0123456789abcdef-" + suffix }

func linkBody(conn uuid.UUID, tag string) string {
	return fmt.Sprintf(`{"connection_id":%q,"project_id":"0-1","tag_id":%q}`, conn, tag)
}

func TestIssueTrackerConnectionRoutesLiveDB(t *testing.T) {
	f := newITFixture(t, true)
	token := itToken("a")
	code, body, conn := f.connect(f.jwt, token)
	if code != http.StatusCreated {
		t.Fatalf("connect = %d\n%s", code, body)
	}
	// The response never carries the token or any ciphertext field.
	if strings.Contains(body, token) || strings.Contains(strings.ToLower(body), "ciphertext") || strings.Contains(body, `"token"`) {
		t.Fatalf("connect response leaks secret material: %s", body)
	}
	rec := f.do(f.jwt, http.MethodGet, "/api/issue-trackers/connections", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), conn.String()) || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(f.jwt, http.MethodGet, "/api/issue-trackers/connections/"+conn.String()+"/projects", "")
	projectsBody := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"short_name":"UZI"`) {
		t.Fatalf("projects = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(f.jwt, http.MethodGet, "/api/issue-trackers/connections/"+conn.String()+"/tags", "")
	var tags struct {
		Tags []struct {
			ID              string `json:"id"`
			AccountCanApply bool   `json:"account_can_apply"`
			OthersCanApply  bool   `json:"others_can_apply"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tags); err != nil || rec.Code != http.StatusOK || len(tags.Tags) != 3 {
		t.Fatalf("tags = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	for _, tg := range tags.Tags {
		if tg.ID == "5-3" {
			continue
		}
		want := tg.ID == "5-1" // the shared tag; the private one has no one else
		if !tg.AccountCanApply || tg.OthersCanApply != want {
			t.Errorf("tag %s: verdicts = %v/%v", tg.ID, tg.AccountCanApply, tg.OthersCanApply)
		}
	}
	// Names from the tracker reach the browser without control, bidi or zero-width characters.
	for _, resp := range []string{projectsBody, rec.Body.String()} {
		for _, bad := range []string{`\u202e`, `\u200b`, `\n`} {
			if strings.Contains(resp, bad) {
				t.Errorf("response still carries %s: %s", bad, resp)
			}
		}
	}
	if !strings.Contains(rec.Body.String(), `"name":"evilname"`) || !strings.Contains(rec.Body.String(), `"login":"mallory"`) || !strings.Contains(projectsBody, `"name":"Uzi"}`) {
		t.Errorf("sanitised names missing: %s | %s", rec.Body.String(), projectsBody)
	}
	if rec = f.do(f.jwt, http.MethodDelete, "/api/issue-trackers/connections/"+conn.String(), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
}

func TestIssueTrackerConnectionRefusedWithAnEmptyAllowlistLiveDB(t *testing.T) {
	f := newITFixture(t, false)
	code, body, _ := f.connect(f.jwt, itToken("a"))
	if code != http.StatusBadRequest || !strings.Contains(body, "allowed") {
		t.Fatalf("connect = %d %s, want 400 naming the allowlist", code, body)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_tracker_connections WHERE user_id = $1`, f.user).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stored %d connections (%v)", n, err)
	}
}

func TestIssueTrackerRoutesAreOwnerScopedLiveDB(t *testing.T) {
	f := newITFixture(t, true)
	_, _, conn := f.connect(f.jwt, itToken("a"))
	repo := f.freshRepo()
	stranger := cliSeedUser(t, f.pool, false)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, stranger) })
	sjwt := cliMintJWT(t, f.pool, stranger)

	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/issue-trackers/connections/" + conn.String() + "/projects", ""},
		{http.MethodGet, "/api/issue-trackers/connections/" + conn.String() + "/tags", ""},
		{http.MethodDelete, "/api/issue-trackers/connections/" + conn.String(), ""},
		// The owner's repo is not the stranger's: the link route answers 404 for it too.
		{http.MethodPut, "/api/repos/" + repo.String() + "/issue-tracker-link", linkBody(conn, "5-1")},
	} {
		if rec := f.do(sjwt, rt.method, rt.path, rt.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as a stranger = %d, want 404\n%s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	// The owner's repo with the stranger's connection id.
	_, _, theirConn := f.connect(sjwt, itToken("b"))
	if rec := f.do(f.jwt, http.MethodPut, "/api/repos/"+repo.String()+"/issue-tracker-link", linkBody(theirConn, "5-1")); rec.Code != http.StatusNotFound {
		t.Errorf("link with a foreign connection = %d, want 404\n%s", rec.Code, rec.Body.String())
	}
	// And the repo is still the owner's, unlinked.
	var linked bool
	if err := f.pool.QueryRow(context.Background(), `SELECT issue_tracker_connection_id IS NOT NULL FROM repos WHERE id = $1`, repo).Scan(&linked); err != nil || linked {
		t.Fatalf("linked = %v (%v)", linked, err)
	}
}

func TestIssueTrackerLinkRouteLiveDB(t *testing.T) {
	f := newITFixture(t, true)
	_, _, conn := f.connect(f.jwt, itToken("a"))
	path := func(repo uuid.UUID) string { return "/api/repos/" + repo.String() + "/issue-tracker-link" }

	repo := f.freshRepo()
	rec := f.do(f.jwt, http.MethodPut, path(repo), linkBody(conn, "5-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("link = %d\n%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Link struct {
			RepoID, ConnectionID uuid.UUID
			ProjectShortName     string `json:"project_short_name"`
			TagName              string `json:"tag_name"`
		} `json:"link"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Link.ProjectShortName != "UZI" || out.Link.TagName != "uzi-ready" {
		t.Fatalf("link body = %s", rec.Body.String())
	}

	if rec = f.do(f.jwt, http.MethodPut, path(repo), linkBody(conn, "5-1")); rec.Code != http.StatusConflict {
		t.Errorf("relink = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	enabled := f.freshRepo()
	if _, err := f.q.SetRepoEnabledForUser(context.Background(), store.SetRepoEnabledForUserParams{ID: enabled, Enabled: true, UserID: f.user}); err != nil {
		t.Fatal(err)
	}
	if rec = f.do(f.jwt, http.MethodPut, path(enabled), linkBody(conn, "5-1")); rec.Code != http.StatusConflict {
		t.Errorf("link of an enabled repo = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	// A tag nobody else can apply is refused with the reason, as a 409.
	if rec = f.do(f.jwt, http.MethodPut, path(f.freshRepo()), linkBody(conn, "5-2")); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "named users") {
		t.Errorf("link with a private tag = %d, want 409 saying what to change\n%s", rec.Code, rec.Body.String())
	}
	if rec = f.do(f.jwt, http.MethodPut, path(f.freshRepo()), linkBody(conn, "5-404")); rec.Code != http.StatusBadRequest {
		t.Errorf("link with an unknown tag = %d, want 400\n%s", rec.Code, rec.Body.String())
	}
	if rec = f.do(f.jwt, http.MethodPut, path(f.freshRepo()), `{"connection_id":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("link with a malformed connection id = %d, want 400", rec.Code)
	}

	// With a repo linked: the connection cannot be deleted, and cannot change account.
	if rec = f.do(f.jwt, http.MethodDelete, "/api/issue-trackers/connections/"+conn.String(), ""); rec.Code != http.StatusConflict {
		t.Errorf("delete of a linked connection = %d, want 409\n%s", rec.Code, rec.Body.String())
	}
	if code, body, _ := f.connect(f.jwt, itToken("other")); code != http.StatusConflict || !strings.Contains(body, "different account") {
		t.Errorf("reconnect with another account = %d, want 409\n%s", code, body)
	}
	if code, body, id := f.connect(f.jwt, itToken("rotated")); code != http.StatusCreated || id != conn {
		t.Errorf("same-account reconnect = %d %s (id %s, want %s)", code, body, id, conn)
	}
}
