package handler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
)

// linkedWorkerRun is one worker-held run on a repo linked to the YouTrack fake, plus a
// second run on an unlinked repo of the same owner, both on one recorded forge.
type linkedWorkerRun struct {
	router   http.Handler
	srv      *youtracktest.Server
	rec      *forgetest.Recorder
	token    string
	linked   uuid.UUID // run on the linked repo
	unlinked uuid.UUID // run on the unlinked repo
	issueN   int64
}

func (w linkedWorkerRun) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/worker/runs/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+w.token)
	rr := httptest.NewRecorder()
	w.router.ServeHTTP(rr, req)
	return rr
}

func newLinkedWorkerRun(t *testing.T, wireDrivers bool) linkedWorkerRun {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	q := h.q
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
	forgeURL := "https://" + uuid.NewString() + ".forge.test"
	svc := forgesvc.NewWithForgeBuilder(q, h.box, 5*time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return rec, nil })
	itSvc := issuetrackersvc.New(q, h.box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	if wireDrivers {
		svc.SetIssueTrackerDrivers(itSvc)
	}
	h.svc = svc

	sealed, err := svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}
	owner := cliSeedUser(t, pool, false)
	conn := uuid.New()
	cliMustExec(t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, owner, forgeURL, sealed)
	yt, err := itSvc.Connect(ctx, owner, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	const projectID = 77
	repoL, repoP := uuid.New(), uuid.New()
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, $3, $4, 'https://forge.test/g/l', 'main', true, $5, $6, $7, $8, $9, now())`,
		repoL, conn, projectID, "g/linked-"+repoL.String(), yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.test/g/p', 'main', true)`, repoP, conn, projectID+1, "g/plain-"+repoP.String())

	issueN := srv.SeedIssue("a youtrack card", "yt body", youtracktest.EligibilityTagID)
	srv.SeedComment(issueN, "someone", "a youtrack comment")

	token := "linked-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	worker := uuid.New()
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'linked-worker',$3,'online')", worker, owner, sum[:])
	seedRun := func(repo uuid.UUID, iid int64) uuid.UUID {
		id := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, worker_id, claim_generation)
		 VALUES ($1, $2, $3, $4, 'Do X', 'desc', 'running', 'issue', $5, 1)`, id, owner, repo, iid, worker)
		return id
	}
	srv.ResetRequests()
	return linkedWorkerRun{router: router, srv: srv, rec: rec, token: token, linked: seedRun(repoL, issueN), unlinked: seedRun(repoP, 8), issueN: issueN}
}

func (w linkedWorkerRun) forgeIssueCalls() []string { return issueSideCalls(w.rec.Calls()) }

func (w linkedWorkerRun) eligibilityCalls() int {
	n := 0
	for _, c := range w.rec.Calls() {
		if strings.HasPrefix(c, "RepositoryAuthorEligibility(") {
			n++
		}
	}
	return n
}

// PRD #4173 M4 (DL-5, S2, S5): a worker's issue routes for a run on a linked repo reach
// the issue tracker through the API only; the merge-request routes keep the forge; an
// unlinked run is unchanged. The issue author on a linked repo is never looked up as a
// forge account.
func TestWorkerForgeIssueRoutesForLinkedRunReachYouTrackOnlyLiveDB(t *testing.T) {
	w := newLinkedWorkerRun(t, true)
	other := w.srv.SeedIssue("another card", "other body", youtracktest.EligibilityTagID)
	w.srv.SeedComment(other, "someone", "another comment")

	for name, path := range map[string]string{
		"own issue":    fmt.Sprintf("%s/forge/issues/%d", w.linked, w.issueN),
		"other issue":  fmt.Sprintf("%s/forge/issues/%d", w.linked, other),
		"list":         fmt.Sprintf("%s/forge/issues", w.linked),
		"label events": fmt.Sprintf("%s/forge/issues/%d/label-events", w.linked, w.issueN),
	} {
		before := len(w.srv.Requests())
		rr := w.get(t, path)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", name, rr.Code, rr.Body.String())
		}
		if len(w.srv.Requests()) == before {
			t.Errorf("%s: the request did not reach YouTrack", name)
		}
		if c := w.forgeIssueCalls(); len(c) != 0 {
			t.Fatalf("%s: a linked run's issue read reached the forge: %v", name, c)
		}
	}
	// YouTrack authors carry forge account id 0, which the assessment never looks up even
	// on an unlinked repo, so this count cannot fail on its own; the check that a linked
	// repo never consults the lookup for a non-zero id is
	// issueinput.TestLinkedAssessmentDecidesByMappingAndNeverCallsTheForgeLookup and the
	// stamped-id double in workersvc/linked_author_mapping_test.go.
	if n := w.eligibilityCalls(); n != 0 {
		t.Fatalf("the forge was asked about a YouTrack issue author %d times; a linked repo's author is never a forge account", n)
	}

	// The merge-request route keeps the forge driver (S2) and never touches YouTrack.
	before := len(w.srv.Requests())
	if rr := w.get(t, w.linked.String()+"/forge/merge-requests/3"); rr.Code != http.StatusOK {
		t.Fatalf("merge request route: %d %s", rr.Code, rr.Body.String())
	}
	if len(w.srv.Requests()) != before {
		t.Fatal("the merge-request route reached YouTrack")
	}
	if !strings.Contains(strings.Join(w.rec.Calls(), "\n"), "GetMergeRequest(") {
		t.Fatalf("the merge-request route did not reach the forge: %v", w.rec.Calls())
	}

	// An unlinked run is byte-identical to before: its issue read goes to the forge only.
	before = len(w.srv.Requests())
	if rr := w.get(t, w.unlinked.String()+"/forge/issues/9"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"iid":9`) {
		t.Fatalf("unlinked issue read: %d %s", rr.Code, rr.Body.String())
	}
	if len(w.srv.Requests()) != before {
		t.Fatal("an unlinked run's issue read reached YouTrack")
	}
	if c := w.forgeIssueCalls(); len(c) == 0 {
		t.Fatal("an unlinked run's issue read did not reach the forge")
	}
}

// A linked run whose resolver is not wired fails closed: a generic 502, no forge issue
// call and no YouTrack request.
func TestWorkerForgeIssueRoutesForLinkedRunFailClosedWithoutResolverLiveDB(t *testing.T) {
	w := newLinkedWorkerRun(t, false)
	for _, path := range []string{
		fmt.Sprintf("%s/forge/issues/%d", w.linked, w.issueN),
		fmt.Sprintf("%s/forge/issues", w.linked),
		fmt.Sprintf("%s/forge/issues/%d/label-events", w.linked, w.issueN),
	} {
		rr := w.get(t, path)
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("%s: status %d, want 502 (body %s)", path, rr.Code, rr.Body.String())
		}
	}
	if c := w.forgeIssueCalls(); len(c) != 0 {
		t.Fatalf("a linked run with no resolver fell back to the forge: %v", c)
	}
	if n := len(w.srv.Requests()); n != 0 {
		t.Fatalf("YouTrack saw %d requests", n)
	}
}

// PRD #4173 D11 wiring: the worker's get_issue route agrees with the capture. Through the
// real write guard over the YouTrack fake, an issue reported by an unmapped login and a
// comment by one are withheld behind the placeholder, while the connection account's own
// issue and comment are kept; the forge is never asked who the author is.
func TestWorkerForgeGetIssueForLinkedRunMapsAuthorsByTheConnectionAccountLiveDB(t *testing.T) {
	w := newLinkedWorkerRun(t, true)
	mine := w.srv.SeedIssue("my card", "MINE_BODY", youtracktest.EligibilityTagID)
	w.srv.SeedComment(mine, "someone", "UNMAPPED_COMMENT")
	w.srv.SeedComment(mine, youtracktest.BotLogin, "OWN_COMMENT")
	theirs := w.srv.SeedIssue("their card", "THEIR_BODY", youtracktest.EligibilityTagID)
	w.srv.SetReporter(theirs, youtracktest.OtherLogin)
	// Reported by the connection's account, but its description was then edited by another
	// user: not the account's text (D11), so it comes back behind the placeholder.
	edited := w.srv.SeedIssue("edited card", "EDITED_ORIGINAL", youtracktest.EligibilityTagID)
	injected := "INJECTED_BODY"
	w.srv.SeedTextEdit(edited, youtracktest.OtherLogin, nil, &injected)

	rr := w.get(t, fmt.Sprintf("%s/forge/issues/%d", w.linked, mine))
	if rr.Code != http.StatusOK {
		t.Fatalf("own account's issue: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "MINE_BODY") || !strings.Contains(body, "OWN_COMMENT") {
		t.Fatalf("the connection account's own issue and comment must be kept: %s", body)
	}
	if strings.Contains(body, "UNMAPPED_COMMENT") || !strings.Contains(body, "Issue content withheld") {
		t.Fatalf("an unmapped comment must be withheld behind the placeholder: %s", body)
	}

	rr = w.get(t, fmt.Sprintf("%s/forge/issues/%d", w.linked, theirs))
	if rr.Code != http.StatusOK {
		t.Fatalf("unmapped author's issue: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "THEIR_BODY") || !strings.Contains(rr.Body.String(), "Issue content withheld") {
		t.Fatalf("an unmapped author's issue must be withheld: %s", rr.Body.String())
	}
	rr = w.get(t, fmt.Sprintf("%s/forge/issues/%d", w.linked, edited))
	if rr.Code != http.StatusOK {
		t.Fatalf("edited issue: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "INJECTED_BODY") || strings.Contains(rr.Body.String(), "EDITED_ORIGINAL") ||
		!strings.Contains(rr.Body.String(), "Issue content withheld") {
		t.Fatalf("an issue whose text another user edited must be withheld: %s", rr.Body.String())
	}
	if n := w.eligibilityCalls(); n != 0 {
		t.Fatalf("the forge was asked about a YouTrack author %d times", n)
	}
}
