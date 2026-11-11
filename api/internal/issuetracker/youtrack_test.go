package issuetracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// testProjectID is the forge project id the test drivers are bound to; the
// tests pass it as the projectID argument.
const testProjectID = 1

// fakeEligibility is the eligibility tag the fake seeds: owned by another user
// and shared with All Users.
var fakeEligibility = TagRef{ID: youtracktest.EligibilityTagID, Name: youtracktest.EligibilityTagName}

// fakeOptions are the Options that bind a driver to the fake's main project
// and eligibility tag with the state mapping its State bundle supports.
func fakeOptions(srv *youtracktest.Server) Options {
	return Options{
		BaseURL:        srv.URL,
		Token:          srv.Token,
		Project:        ProjectRef{ID: youtracktest.ProjectID, ShortName: youtracktest.ProjectShortName},
		ProjectID:      testProjectID,
		EligibilityTag: fakeEligibility,
		OpenState:      "Open",
		ClosedState:    "Fixed",
	}
}

// newFakeDriver starts the fake and builds a driver over it.
func newFakeDriver(t *testing.T) (*youTrack, *youtracktest.Server) {
	t.Helper()
	srv := youtracktest.NewServer(t)
	y, err := newYouTrackOver(fakeOptions(srv), srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return y, srv
}

// credential assembles a token-shaped value at runtime, with characters that
// each escaping form rewrites.
func credential() string {
	return strings.Join([]string{"perm", ":", "ab c/d\"e", "-", "Zq9xLm2VnB7wKp3T"}, "")
}

// youtracktestBot is the login the fake's bearer token authenticates as.
const youtracktestBot = youtracktest.BotLogin

// newFakeDriverWithField is newFakeDriver with the State field renamed and the
// driver told the new name.
func newFakeDriverWithField(t *testing.T, field string) (*youtracktest.Server, *youTrack) {
	t.Helper()
	srv := youtracktest.NewServer(t)
	srv.SetStateField(field)
	opts := fakeOptions(srv)
	opts.StateField = field
	y, err := newYouTrackOver(opts, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return srv, y
}

func TestConstructorValidation(t *testing.T) {
	good := Options{
		BaseURL: "https://yt.example.com", Token: credential(), Project: ProjectRef{ID: "0-1", ShortName: "UZI"},
		ProjectID: 7, EligibilityTag: TagRef{ID: "5-3", Name: "uzi"},
	}
	mutate := func(f func(*Options)) Options { o := good; f(&o); return o }
	if _, err := New(KindYouTrack, good); err != nil {
		t.Fatalf("good options: %v", err)
	}
	if _, err := New(KindYouTrack, mutate(func(o *Options) { o.Project, o.ProjectID, o.EligibilityTag = ProjectRef{}, 0, TagRef{} })); err != nil {
		t.Fatalf("unbound options: %v", err)
	}
	bad := map[string]Options{
		"http":        mutate(func(o *Options) { o.BaseURL = "http://yt.example.com" }),
		"path":        mutate(func(o *Options) { o.BaseURL = "https://yt.example.com/youtrack" }),
		"userinfo":    mutate(func(o *Options) { o.BaseURL = "https://u:p@yt.example.com" }),
		"query":       mutate(func(o *Options) { o.BaseURL = "https://yt.example.com?x=1" }),
		"fragment":    mutate(func(o *Options) { o.BaseURL = "https://yt.example.com#x" }),
		"no host":     mutate(func(o *Options) { o.BaseURL = "https://" }),
		"zero width":  mutate(func(o *Options) { o.BaseURL = "https://yt.exam\u200bple.com" }),
		"bidi":        mutate(func(o *Options) { o.BaseURL = "https://yt.example.com\u202e" }),
		"hangul fill": mutate(func(o *Options) { o.BaseURL = "https://yt.exam\u3164ple.com" }),
		"unicode idn": mutate(func(o *Options) { o.BaseURL = "https://yt.exampl\u00e9.com" }),
		"empty":       mutate(func(o *Options) { o.BaseURL = "" }),
		"short token": mutate(func(o *Options) { o.Token = "short" }),
		"bad short":   mutate(func(o *Options) { o.Project.ShortName = "1X" }),
		"bad id":      mutate(func(o *Options) { o.Project.ID = "abc" }),
		"half bound":  mutate(func(o *Options) { o.Project.ID = "" }),
		// A binding is all or nothing (PRD #4173 D10, D14).
		"no project id":       mutate(func(o *Options) { o.ProjectID = 0 }),
		"negative project id": mutate(func(o *Options) { o.ProjectID = -3 }),
		"no eligibility":      mutate(func(o *Options) { o.EligibilityTag = TagRef{} }),
		"project id only":     mutate(func(o *Options) { o.Project, o.EligibilityTag = ProjectRef{}, TagRef{} }),
		"eligibility only":    mutate(func(o *Options) { o.Project, o.ProjectID = ProjectRef{}, 0 }),
		"bad tag id":          mutate(func(o *Options) { o.EligibilityTag.ID = "../x" }),
		"bad tag name":        mutate(func(o *Options) { o.EligibilityTag.Name = "a}b" }),
		"empty tag name":      mutate(func(o *Options) { o.EligibilityTag.Name = "" }),
	}
	for name, o := range bad {
		if _, err := New(KindYouTrack, o); err == nil {
			t.Errorf("%s: constructor accepted %+v", name, o)
		}
	}
	if _, err := New(KindYouTrack, mutate(func(o *Options) { o.BaseURL = "https://yt.example.com/" })); err != nil {
		t.Errorf("trailing slash rejected: %v", err)
	}
}

// requestLog counts the requests a raw handler receives, by method.
type requestLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *requestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, r.Method+" "+r.URL.EscapedPath())
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// writes returns the logged requests that are not GETs.
func (l *requestLog) writes() []string {
	var out []string
	for _, c := range l.all() {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// loggedDriver builds a bound driver over a raw handler and logs every request.
func loggedDriver(t *testing.T, h http.HandlerFunc) (*youTrack, *requestLog) {
	t.Helper()
	log := &requestLog{}
	y, _ := tlsDriver(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		h(w, r)
	}))
	return y, log
}

// rawIssue renders an issue as the API would, in project projectID.
func rawIssue(id, readable string, number int64, projectID, tags string) string {
	return fmt.Sprintf(`{"id":%q,"idReadable":%q,"numberInProject":%d,"summary":"s","created":1,"updated":2000,`+
		`"project":{"id":%q,"shortName":"UZI"},"tags":[%s]}`, id, readable, number, projectID, tags)
}

// rawMe is the users/me answer of the account that raw handlers' tags belong to.
const (
	rawMeID      = "1-1"
	rawMe        = `{"id":"` + rawMeID + `","login":"u"}`
	rawNoSharing = `{"readSharingSettings":{"permittedGroups":[],"permittedUsers":[]},"tagSharingSettings":{"permittedGroups":[],"permittedUsers":[]}}`
)

func writeRaw(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// everyMethod lists the eleven forge.IssueTracker methods as closures over y,
// each called with the project id the test drivers are bound to.
func everyMethod(y *youTrack) map[string]func() error { return everyMethodWith(y, testProjectID) }

// everyMethodWith is everyMethod with a chosen projectID argument.
func everyMethodWith(y *youTrack, pid int64) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"ListLabels":             func() error { _, err := y.ListLabels(ctx, pid); return err },
		"EnsureLabels":           func() error { return y.EnsureLabels(ctx, pid, []forge.Label{{Name: "a"}}) },
		"ListIssues":             func() error { _, err := y.ListIssues(ctx, pid, forge.ListIssuesOptions{}); return err },
		"GetIssue":               func() error { _, err := y.GetIssue(ctx, pid, 1); return err },
		"CreateIssue":            func() error { _, err := y.CreateIssue(ctx, pid, "t", "d", []string{"a"}); return err },
		"UpdateIssueLabels":      func() error { return y.UpdateIssueLabels(ctx, pid, 1, []string{"a"}, []string{"b"}) },
		"UpdateIssueDescription": func() error { return y.UpdateIssueDescription(ctx, pid, 1, "d") },
		"SetIssueState":          func() error { return y.SetIssueState(ctx, pid, 1, forge.StateClosed) },
		"ListIssueLabelEvents":   func() error { _, err := y.ListIssueLabelEvents(ctx, pid, 1); return err },
		"ListIssueComments":      func() error { _, err := y.ListIssueComments(ctx, pid, 1); return err },
		"CreateIssueNote":        func() error { _, err := y.CreateIssueNote(ctx, pid, 1, "n"); return err },
	}
}

// TestUnboundDriverRefusesEveryMethod builds a driver with no project and
// checks that every IssueTracker method refuses before sending a request.
func TestUnboundDriverRefusesEveryMethod(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeRaw(w, "[]")
	}))
	t.Cleanup(srv.Close)
	y, err := newYouTrackOver(Options{BaseURL: srv.URL, Token: credential(), OpenState: "Open", ClosedState: "Fixed"}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	calls := everyMethod(y)
	if len(calls) != 11 {
		t.Fatalf("table covers %d methods, want 11", len(calls))
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrProjectNotBound) {
			t.Errorf("%s = %v, want ErrProjectNotBound", name, err)
		}
	}
	if got := log.all(); len(got) != 0 {
		t.Errorf("unbound driver sent %v", got)
	}
	// ListProjects needs no binding.
	if _, err := y.ListProjects(context.Background()); err != nil {
		t.Errorf("ListProjects on an unbound driver: %v", err)
	}
}

// TestBaseURLTrailingSlashIsNormalised checks that a base URL with a trailing
// slash never yields a double slash in a request path.
func TestBaseURLTrailingSlashIsNormalised(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeRaw(w, `{"id":"1-1","login":"u"}`)
	}))
	t.Cleanup(srv.Close)
	y, err := newYouTrackOver(Options{BaseURL: srv.URL + "/", Token: credential()}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(y.baseURL, "/") {
		t.Errorf("baseURL = %q, want no trailing slash", y.baseURL)
	}
	if _, err := y.VerifyToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := log.all(); len(got) != 1 || got[0] != "GET /api/users/me" {
		t.Errorf("requests = %v, want one GET /api/users/me", got)
	}
}

// TestPinnedLimits pins the literal limits.
func TestPinnedLimits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want int64
	}{
		{"listPageBytes", listPageBytes, 8 << 20},
		{"objectBytes", objectBytes, 2 << 20},
		{"errorBodyBytes", errorBodyBytes, 4 << 10},
		{"pageSize", pageSize, 100},
		{"maxItems", int64(maxItems), 100000},
		{"maxPages", int64(maxPages), 2000},
		{"maxTotalBytes", int64(maxTotalBytes), 64 << 20},
		{"maxNestedItems", maxNestedItems, 1000},
		{"maxLabelNameLen", maxLabelNameLen, 200},
		{"minTokenLen", minTokenLen, 16},
		{"maxTokenLen", maxTokenLen, 4096},
		{"maxMessageBytes", maxMessageBytes, 512},
		{"minSegmentLen", minSegmentLen, 8},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}
