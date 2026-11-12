package issuetracker

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// Regression tests for the maintainer's review of the M3 pull request
// (PRD #4173 Decision Log, "M3 review").

var sharedWithAll = youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}}

func listedIIDs(t *testing.T, y *youTrack, labels ...string) []int64 {
	t.Helper()
	got, err := y.ListIssues(context.Background(), testProjectID, forge.ListIssuesOptions{Labels: labels})
	if err != nil {
		t.Fatalf("ListIssues(%v): %v", labels, err)
	}
	out := []int64{}
	for _, i := range got {
		out = append(out, i.IID)
	}
	return out
}

// issueQueries returns the decoded query parameter of every issue listing the
// fake received.
func issueQueries(t *testing.T, srv *youtracktest.Server) []string {
	t.Helper()
	var out []string
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet || r.Path != "/api/issues" {
			continue
		}
		v, err := url.ParseQuery(r.RawQuery)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v.Get("query"))
	}
	return out
}

// TestListIssuesFollowsARenamedEligibilityTag: intake names the stored
// eligibility name, but the tag is followed by id. After a rename in YouTrack
// the tagged issue is still listed, under the stored name; an issue carrying
// only the account's tag of the old name, or only someone else's tag of the new
// name, is not.
func TestListIssuesFollowsARenamedEligibilityTag(t *testing.T) {
	y, srv := newFakeDriver(t)
	eligible := srv.SeedIssue("eligible", "", youtracktest.EligibilityTagID)
	oldName := srv.SeedIssue("carries the account's tag of the old name", "", youtracktest.SameNameTagID)
	srv.RenameTag(youtracktest.EligibilityTagID, "renamed")
	newName := srv.SeedIssue("carries someone else's tag of the new name", "",
		srv.SeedTag("renamed", youtracktest.OtherLogin, sharedWithAll, sharedWithAll))

	srv.ResetRequests()
	if got := listedIIDs(t, y, youtracktest.EligibilityTagName); !slices.Equal(got, []int64{eligible}) {
		t.Fatalf("stored eligibility name lists %v after the rename, want only %d (not %d or %d)", got, eligible, oldName, newName)
	}
	queries := issueQueries(t, srv)
	if len(queries) == 0 || !strings.Contains(queries[0], "tag: {renamed}") || strings.Contains(queries[0], youtracktest.EligibilityTagName) {
		t.Errorf("queries = %q, want the live name and not the stored one", queries)
	}
	got, err := y.GetIssue(context.Background(), testProjectID, eligible)
	if err != nil || !slices.Equal(got.Labels, []string{youtracktest.EligibilityTagName}) {
		t.Errorf("labels = %v, %v; want the stored eligibility name", got.Labels, err)
	}
}

// TestListIssuesWithALiveEligibilityNameTheQueryCannotCarry: a live name with a
// brace would break the query's quoting, so no tag term is sent and the id
// filter alone selects the eligible issue. The fake answers 400 to a query it
// cannot parse, so a brace in the query fails this test.
func TestListIssuesWithALiveEligibilityNameTheQueryCannotCarry(t *testing.T) {
	y, srv := newFakeDriver(t)
	eligible := srv.SeedIssue("eligible", "", youtracktest.EligibilityTagID)
	untagged := srv.SeedIssue("untagged", "")
	srv.RenameTag(youtracktest.EligibilityTagID, "go {now}")

	srv.ResetRequests()
	if got := listedIIDs(t, y, youtracktest.EligibilityTagName); !slices.Equal(got, []int64{eligible}) {
		t.Fatalf("lists %v, want only %d (not the untagged %d)", got, eligible, untagged)
	}
	for _, q := range issueQueries(t, srv) {
		if strings.Contains(q, "tag:") {
			t.Errorf("query %q carries a tag term", q)
		}
	}
}

// TestListIssuesFailsClosedWhenTheEligibilityTagCannotBeRead: a tag that is
// gone, or that the token can no longer see, is an error and no issue is asked
// for. Listing without the eligibility label does not read the tag.
func TestListIssuesFailsClosedWhenTheEligibilityTagCannotBeRead(t *testing.T) {
	for name, tagID := range map[string]func(*youtracktest.Server) string{
		"gone": func(*youtracktest.Server) string { return "5-7777" },
		"not visible": func(s *youtracktest.Server) string {
			return s.SeedTag("hidden", youtracktest.OtherLogin, youtracktest.Sharing{}, youtracktest.Sharing{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := youtracktest.NewServer(t)
			opts := fakeOptions(srv)
			opts.EligibilityTag = TagRef{ID: tagID(srv), Name: "hidden"}
			y, err := newYouTrackOver(opts, srv.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			srv.SeedIssue("any", "")
			ctx := context.Background()
			got, err := y.ListIssues(ctx, testProjectID, forge.ListIssuesOptions{Labels: []string{"hidden"}})
			if err == nil || got != nil {
				t.Fatalf("ListIssues = %+v, %v; want an error and no issue", got, err)
			}
			if q := issueQueries(t, srv); len(q) != 0 {
				t.Errorf("listed issues though the eligibility tag could not be read: %q", q)
			}
			if got, err := y.ListIssues(ctx, testProjectID, forge.ListIssuesOptions{}); err != nil || len(got) != 1 {
				t.Errorf("unfiltered ListIssues = %+v, %v; want the one issue", got, err)
			}
		})
	}
}

// TestCreatedTagKeepsTheEligibilityOwnersAccess: the eligibility tag's owner
// can see and apply it by owning it, which its sharing settings do not say. A
// tag the driver creates names that owner in both settings, once.
func TestCreatedTagKeepsTheEligibilityOwnersAccess(t *testing.T) {
	ctx := context.Background()
	created := func(t *testing.T, srv *youtracktest.Server, eligibility TagRef) youtracktest.TagInfo {
		t.Helper()
		opts := fakeOptions(srv)
		opts.EligibilityTag = eligibility
		y, err := newYouTrackOver(opts, srv.Client().Transport)
		if err != nil {
			t.Fatal(err)
		}
		if err := y.EnsureLabels(ctx, testProjectID, labels("fresh")); err != nil {
			t.Fatal(err)
		}
		tags := srv.TagsNamed("fresh")
		if len(tags) != 1 {
			t.Fatalf("tags named fresh = %+v, want one", tags)
		}
		return tags[0]
	}
	t.Run("owner is someone else", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		got := created(t, srv, fakeEligibility)
		want := []string{youtracktest.OtherUserID}
		if !slices.Equal(got.Read.Users, want) || !slices.Equal(got.Use.Users, want) {
			t.Errorf("created tag users: read %v use %v, want the eligibility owner %v in both", got.Read.Users, got.Use.Users, want)
		}
		if !slices.Equal(got.Read.Groups, []string{youtracktest.AllUsersGroupID}) || !slices.Equal(got.Use.Groups, []string{youtracktest.AllUsersGroupID}) {
			t.Errorf("created tag groups: read %v use %v, want the eligibility tag's", got.Read.Groups, got.Use.Groups)
		}
	})
	t.Run("owner already named is not repeated", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		named := youtracktest.Sharing{Users: []string{youtracktest.BotID, youtracktest.OtherUserID}}
		id := srv.SeedTag("go", youtracktest.OtherLogin, named, youtracktest.Sharing{Users: []string{youtracktest.BotID}})
		got := created(t, srv, TagRef{ID: id, Name: "go"})
		if want := []string{youtracktest.BotID, youtracktest.OtherUserID}; !slices.Equal(got.Read.Users, want) || !slices.Equal(got.Use.Users, want) {
			t.Errorf("created tag users: read %v use %v, want %v in both", got.Read.Users, got.Use.Users, want)
		}
	})
	t.Run("owner is the connection's account", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		id := srv.SeedOwnTag("mine")
		got := created(t, srv, TagRef{ID: id, Name: "mine"})
		if len(got.Read.Users) != 0 || len(got.Use.Users) != 0 {
			t.Errorf("created tag users: read %v use %v, want none", got.Read.Users, got.Use.Users)
		}
	})
}

// TestCreatedTagRefusesAMalformedEligibilityOwnerID: the owner id goes into a
// request body, so it is checked like every other id; nothing is written.
func TestCreatedTagRefusesAMalformedEligibilityOwnerID(t *testing.T) {
	y, log := loggedDriver(t, withRawMe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tags" && r.Method == http.MethodGet:
			writeRaw(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/api/tags/"):
			writeRaw(w, `{"owner":{"id":"../x"},"readSharingSettings":{"permittedGroups":[],"permittedUsers":[]},"tagSharingSettings":{"permittedGroups":[],"permittedUsers":[]}}`)
		default:
			writeRaw(w, `{"id":"5-7","name":"x"}`)
		}
	}))
	if err := y.EnsureLabels(context.Background(), testProjectID, labels("a")); err == nil {
		t.Fatal("EnsureLabels accepted a malformed eligibility owner id")
	}
	for _, w := range log.writes() {
		t.Errorf("sent %v", w)
	}
}

// TestReadsFollowTheWriteSideTagRules: a label a read reports is one a
// ListIssues filter matches and UpdateIssueLabels can remove. Someone else's
// visible tag is none of those, so reads leave it out.
func TestReadsFollowTheWriteSideTagRules(t *testing.T) {
	y, srv := newFakeDriver(t)
	ctx := context.Background()
	theirs := srv.SeedTag("theirs", youtracktest.OtherLogin, sharedWithAll, sharedWithAll)
	mine := srv.SeedOwnTag("mine")
	n := srv.SeedIssue("t", "", theirs, mine, youtracktest.EligibilityTagID, youtracktest.SameNameTagID)
	srv.SeedTagActivity(n, "alice", []string{theirs, mine, youtracktest.EligibilityTagID}, []string{theirs})

	want := []string{"mine", youtracktest.EligibilityTagName}
	got, err := y.GetIssue(ctx, testProjectID, n)
	if err != nil || !slices.Equal(got.Labels, want) {
		t.Fatalf("GetIssue labels = %v, %v; want %v (someone else's tag left out)", got.Labels, err, want)
	}
	listed, err := y.ListIssues(ctx, testProjectID, forge.ListIssuesOptions{})
	if err != nil || len(listed) != 1 || !slices.Equal(listed[0].Labels, want) {
		t.Fatalf("ListIssues = %+v, %v; want one issue with labels %v", listed, err, want)
	}
	events, err := y.ListIssueLabelEvents(ctx, testProjectID, n)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range events {
		names = append(names, e.Action+" "+e.LabelName)
	}
	if wantEvents := []string{"add mine", "add " + youtracktest.EligibilityTagName}; !slices.Equal(names, wantEvents) {
		t.Errorf("label events = %v, want %v", names, wantEvents)
	}

	// The label a read does not report is also not filterable.
	if got := listedIIDs(t, y, "theirs"); len(got) != 0 {
		t.Errorf("filter by someone else's tag name lists %v", got)
	}
	// Every reported label filters to the issue and can be removed.
	for _, l := range got.Labels {
		if got := listedIIDs(t, y, l); !slices.Equal(got, []int64{n}) {
			t.Errorf("filter by reported label %q lists %v, want [%d]", l, got, n)
		}
		if err := y.UpdateIssueLabels(ctx, testProjectID, n, nil, []string{l}); err != nil {
			t.Fatalf("remove %q: %v", l, err)
		}
	}
	if got := srv.IssueTagIDs(n); !slices.Equal(got, []string{theirs, youtracktest.SameNameTagID}) {
		t.Errorf("after removing every reported label the issue carries %v, want only the unreported %v", got, []string{theirs, youtracktest.SameNameTagID})
	}
	after, err := y.GetIssue(ctx, testProjectID, n)
	if err != nil || len(after.Labels) != 0 {
		t.Errorf("labels after the removes = %v, %v; want none", after.Labels, err)
	}
}

// TestContainsToken covers every form the redactor knows, and the miss.
func TestContainsToken(t *testing.T) {
	c := credential()
	for _, form := range tokenForms() {
		if !ContainsToken("login-"+form+"-x", c) {
			t.Errorf("ContainsToken missed the form %q", form)
		}
		if lower := lowerPercent(form); !ContainsToken(lower, c) {
			t.Errorf("ContainsToken missed the lower-case escape %q", lower)
		}
	}
	if tail := c[strings.LastIndexByte(c, '-')+1:]; ContainsToken("uzi-bot", c) || ContainsToken(tail[:8], c) || ContainsToken("x", "") {
		t.Error("ContainsToken matched a value that holds no form of the token")
	}
}

// TestBaseURLPathRefusalNamesTheContextPath pins the message an admin sees for
// a YouTrack served under a context path.
func TestBaseURLPathRefusalNamesTheContextPath(t *testing.T) {
	_, err := strictBaseURL("https://yt.example.com/youtrack")
	if err == nil || !strings.Contains(err.Error(), "must not carry a path") || !strings.Contains(err.Error(), "context path is not supported yet") {
		t.Errorf("err = %v", err)
	}
}
