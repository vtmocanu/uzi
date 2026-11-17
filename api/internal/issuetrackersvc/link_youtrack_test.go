package issuetrackersvc

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// ytHarness runs the Service over the real YouTrack driver against the
// youtracktest fake, with the fake's certificate trusted for the test only.
type ytHarness struct {
	svc        *Service
	q          *fakeQuerier
	srv        *youtracktest.Server
	user, conn uuid.UUID
}

func newYTHarness(t *testing.T) *ytHarness {
	t.Helper()
	srv := youtracktest.NewServer(t)
	// The driver builds its transport from a clone of http.DefaultTransport and
	// takes no TLS option by design, so trust the fake there and restore after.
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })

	key := make([]byte, secretbox.KeySize)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeQuerier{}
	svc := New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	user := uuid.New()
	c, err := svc.Connect(context.Background(), user, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	return &ytHarness{svc: svc, q: q, srv: srv, user: user, conn: c.ID}
}

func (h *ytHarness) link(repo uuid.UUID, tag string) error {
	_, err := h.svc.LinkRepo(context.Background(), h.user, repo, h.conn, youtracktest.ProjectID, tag)
	return err
}

// A successful link check only reads: every request is a GET and no issue
// gains or loses a tag (PRD #4173 M4: the link writes nothing to the tracker).
func TestLinkRepoOverYouTrackSendsOnlyReads(t *testing.T) {
	h := newYTHarness(t)
	issue := h.srv.SeedIssue("a card", "body", youtracktest.EligibilityTagID)
	before := h.srv.IssueTagIDs(issue)
	tagsBefore := len(h.srv.TagsNamed(youtracktest.EligibilityTagName))
	repo := h.q.addRepo(h.user, true)

	h.srv.ResetRequests()
	l, err := h.svc.LinkRepo(context.Background(), h.user, repo, h.conn, youtracktest.ProjectID, youtracktest.EligibilityTagID)
	if err != nil {
		t.Fatal(err)
	}
	if l.ProjectShort != youtracktest.ProjectShortName || l.TagName != youtracktest.EligibilityTagName {
		t.Fatalf("link = %+v", l)
	}
	reqs := h.srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("the link check made no request to the tracker")
	}
	for _, r := range reqs {
		if r.Method != http.MethodGet {
			t.Errorf("request %s %s is not a read", r.Method, r.Path)
		}
	}
	if got := h.srv.IssueTagIDs(issue); !slices.Equal(got, before) {
		t.Errorf("issue tags changed from %v to %v", before, got)
	}
	if n := len(h.srv.TagsNamed(youtracktest.EligibilityTagName)); n != tagsBefore {
		t.Errorf("tags named %q went from %d to %d", youtracktest.EligibilityTagName, tagsBefore, n)
	}
}

func TestLinkRepoOverYouTrackRefusesByTheTagsRealSharing(t *testing.T) {
	h := newYTHarness(t)
	solo := h.srv.SeedTag("solo", youtracktest.OtherLogin,
		youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}},
		youtracktest.Sharing{Groups: []string{youtracktest.SoloGroupID}})
	hidden := h.srv.SeedTag("hidden", youtracktest.OtherLogin, youtracktest.Sharing{}, youtracktest.Sharing{})
	cases := []struct {
		name string
		tag  string
		kind Kind
		msg  string
	}{
		{"bot owns it, shared with nobody", youtracktest.SameNameTagID, KindConflict, "named users"},
		{"other owns it, use group lacks the bot", solo, KindConflict, "cannot apply"},
		{"the bot cannot see it", hidden, KindBadRequest, "not found"},
		{"no such tag", "5-123456", KindBadRequest, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := h.q.addRepo(h.user, true)
			e := wantKind(t, h.link(repo, tc.tag), tc.kind)
			if !strings.Contains(e.Message, tc.msg) {
				t.Errorf("message %q lacks %q", e.Message, tc.msg)
			}
			if h.q.repo(repo).linkedConn != uuid.Nil {
				t.Error("the repo was linked")
			}
		})
	}
	_ = wantKind(t, errOf(h.svc.LinkRepo(context.Background(), h.user, h.q.addRepo(h.user, true), h.conn, "0-999", youtracktest.EligibilityTagID)), KindBadRequest)
}
