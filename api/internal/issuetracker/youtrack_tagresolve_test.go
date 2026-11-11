package issuetracker

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// These tests pin PRD #4173 D14: the eligibility label name is the stored
// eligibility tag by id, whoever owns it, and any other name is only ever a tag
// the connection's own account owns. The fake seeds an eligibility tag owned by
// another user and shared with All Users, and a tag with the same name owned by
// the account.

func labels(names ...string) []forge.Label {
	out := make([]forge.Label, 0, len(names))
	for _, n := range names {
		out = append(out, forge.Label{Name: n})
	}
	return out
}

func ids(tags []youtracktest.TagInfo) []string {
	var out []string
	for _, t := range tags {
		out = append(out, t.ID)
	}
	return out
}

func TestEnsureLabelsNeverCreatesTheEligibilityTag(t *testing.T) {
	y, srv := newFakeDriver(t)
	ctx := context.Background()
	for range 2 {
		if err := y.EnsureLabels(ctx, testProjectID, labels(youtracktest.EligibilityTagName, "fresh")); err != nil {
			t.Fatal(err)
		}
	}
	got := srv.TagsNamed(youtracktest.EligibilityTagName)
	if want := []string{youtracktest.EligibilityTagID, youtracktest.SameNameTagID}; !sameSet(ids(got), want) {
		t.Fatalf("tags named %q = %v, want the seeded %v and no new one", youtracktest.EligibilityTagName, ids(got), want)
	}
	fresh := srv.TagsNamed("fresh")
	if len(fresh) != 1 {
		t.Fatalf("tags named fresh = %+v, want one", fresh)
	}
	elig := srv.TagByID(youtracktest.EligibilityTagID)
	if fresh[0].OwnerLogin != youtracktest.BotLogin {
		t.Errorf("fresh is owned by %q, want the account %q", fresh[0].OwnerLogin, youtracktest.BotLogin)
	}
	if !reflect.DeepEqual(fresh[0].Read, elig.Read) || !reflect.DeepEqual(fresh[0].Use, elig.Use) {
		t.Errorf("fresh sharing = read %+v use %+v, want the eligibility tag's read %+v use %+v", fresh[0].Read, fresh[0].Use, elig.Read, elig.Use)
	}
	if len(elig.Use.Groups) != 1 || elig.Use.Groups[0] != youtracktest.AllUsersGroupID {
		t.Fatalf("fixture error: the seeded eligibility tag is not shared with All Users: %+v", elig)
	}
}

func TestEnsureLabelsDoesNotAdoptSomeoneElsesTagOfTheSameName(t *testing.T) {
	y, srv := newFakeDriver(t)
	theirs := srv.SeedTag("theirs", youtracktest.OtherLogin,
		youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}}, youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}})
	if err := y.EnsureLabels(context.Background(), testProjectID, labels("theirs")); err != nil {
		t.Fatal(err)
	}
	got := srv.TagsNamed("theirs")
	if len(got) != 2 || got[0].ID != theirs || got[1].OwnerLogin != youtracktest.BotLogin {
		t.Errorf("tags named theirs = %+v, want theirs plus one the account owns", got)
	}
}

func TestCreateIssueAttachesTheEligibilityTagNotTheSameNamedOne(t *testing.T) {
	y, srv := newFakeDriver(t)
	issue, err := y.CreateIssue(context.Background(), testProjectID, "t", "d", []string{youtracktest.EligibilityTagName, "other"})
	if err != nil {
		t.Fatal(err)
	}
	got := srv.IssueTagIDs(issue.IID)
	if len(got) != 2 || got[0] != youtracktest.EligibilityTagID || got[1] == youtracktest.SameNameTagID {
		t.Errorf("created issue carries %v, want the eligibility tag then the account's own %q", got, "other")
	}
	if !sameSet(issue.Labels, []string{youtracktest.EligibilityTagName, "other"}) {
		t.Errorf("labels = %v", issue.Labels)
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func TestListIssuesMatchesByResolvedTagID(t *testing.T) {
	y, srv := newFakeDriver(t)
	ctx := context.Background()
	eligible := srv.SeedIssue("eligible", "", youtracktest.EligibilityTagID)
	lookalike := srv.SeedIssue("lookalike", "", youtracktest.SameNameTagID)
	theirs := srv.SeedTag("shared-name", youtracktest.OtherLogin,
		youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}}, youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}})
	onTheirs := srv.SeedIssue("on their tag", "", theirs)

	iids := func(labels ...string) []int64 {
		t.Helper()
		got, err := y.ListIssues(ctx, testProjectID, forge.ListIssuesOptions{Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, i := range got {
			out = append(out, i.IID)
		}
		return out
	}
	if got := iids(youtracktest.EligibilityTagName); !slices.Equal(got, []int64{eligible}) {
		t.Errorf("eligibility name finds %v, want only %d (not the lookalike %d)", got, eligible, lookalike)
	}
	// The lookalike's labels never show the eligibility name.
	if got, err := y.GetIssue(ctx, testProjectID, lookalike); err != nil || len(got.Labels) != 0 {
		t.Errorf("lookalike = %+v, %v; want no label", got.Labels, err)
	}
	all, err := y.ListIssues(ctx, testProjectID, forge.ListIssuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range all {
		if i.IID == lookalike && len(i.Labels) != 0 {
			t.Errorf("ListIssues reports labels %v for the lookalike", i.Labels)
		}
	}
	// A name the account owns no tag of finds nothing, though someone else's
	// tag has the name, and no tag is created by the read.
	if got := iids("shared-name"); len(got) != 0 {
		t.Errorf("a name only someone else owns finds %v (their issue is %d)", got, onTheirs)
	}
	if n := len(srv.TagsNamed("shared-name")); n != 1 {
		t.Errorf("ListIssues created a tag: %d tags named shared-name", n)
	}
	// Once the account owns a tag of that name, only issues with that tag match.
	if err := y.EnsureLabels(ctx, testProjectID, labels("shared-name")); err != nil {
		t.Fatal(err)
	}
	if got := iids("shared-name"); len(got) != 0 {
		t.Errorf("after EnsureLabels finds %v, want none: no issue carries the account's tag", got)
	}
	named := srv.TagsNamed("shared-name")
	if len(named) != 2 {
		t.Fatalf("tags named shared-name = %+v, want their tag and one the account owns", named)
	}
	mine := named[1].ID
	own := srv.SeedIssue("on my tag", "", mine)
	if got := iids("shared-name"); !slices.Equal(got, []int64{own}) {
		t.Errorf("finds %v, want only %d", got, own)
	}
}

func TestUpdateIssueLabelsOfTheEligibilityNameTouchesOnlyTheEligibilityTag(t *testing.T) {
	y, srv := newFakeDriver(t)
	ctx := context.Background()
	n := srv.SeedIssue("t", "", youtracktest.SameNameTagID)
	if err := y.UpdateIssueLabels(ctx, testProjectID, n, []string{youtracktest.EligibilityTagName}, nil); err != nil {
		t.Fatal(err)
	}
	if got := srv.IssueTagIDs(n); !sameSet(got, []string{youtracktest.SameNameTagID, youtracktest.EligibilityTagID}) {
		t.Errorf("after add: %v, want the account's tag and the eligibility tag", got)
	}
	if err := y.UpdateIssueLabels(ctx, testProjectID, n, nil, []string{youtracktest.EligibilityTagName}); err != nil {
		t.Fatal(err)
	}
	if got := srv.IssueTagIDs(n); !slices.Equal(got, []string{youtracktest.SameNameTagID}) {
		t.Errorf("after remove: %v, want only the account's tag", got)
	}
	// With no eligibility tag on the issue, a remove removes nothing, though the
	// issue carries a tag of that name.
	srv.ResetRequests()
	if err := y.UpdateIssueLabels(ctx, testProjectID, n, nil, []string{youtracktest.EligibilityTagName}); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("removing an absent eligibility tag wrote: %v", r)
		}
	}
}

func TestUpdateIssueLabelsRemoveNeverTouchesSomeoneElsesTag(t *testing.T) {
	y, srv := newFakeDriver(t)
	theirs := srv.SeedTag("theirs", youtracktest.OtherLogin,
		youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}}, youtracktest.Sharing{Groups: []string{youtracktest.AllUsersGroupID}})
	n := srv.SeedIssue("t", "", theirs)
	srv.ResetRequests()
	if err := y.UpdateIssueLabels(context.Background(), testProjectID, n, nil, []string{"theirs"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.IssueTagIDs(n); !slices.Equal(got, []string{theirs}) {
		t.Errorf("tags = %v, want their tag left on the issue", got)
	}
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("a remove with no tag of the account's wrote: %v", r)
		}
	}
	if len(srv.TagsNamed("theirs")) != 1 {
		t.Error("a remove created a tag")
	}
}

func TestLabelEventsDropATagThatOnlySharesTheEligibilityName(t *testing.T) {
	y, srv := newFakeDriver(t)
	n := srv.SeedIssue("t", "")
	srv.SeedTagActivity(n, "alice", []string{youtracktest.SameNameTagID, youtracktest.EligibilityTagID}, []string{youtracktest.SameNameTagID})
	got, err := y.ListIssueLabelEvents(context.Background(), testProjectID, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Action != "add" || got[0].LabelName != youtracktest.EligibilityTagName || got[0].ID != 1 {
		t.Errorf("events = %+v, want one add of the eligibility tag", got)
	}
}

// TestAccountIsReadOnceAndKept: the token's account comes from users/me the
// first time ownership matters, once for the driver's lifetime, even when
// calls run in parallel; a failed lookup is not kept.
func TestAccountIsReadOnceAndKept(t *testing.T) {
	var meCalls atomic.Int64
	var failMe atomic.Bool
	y, _ := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/users/me":
			meCalls.Add(1)
			if failMe.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeRaw(w, rawMe)
		case r.URL.Path == "/api/tags" && r.Method == http.MethodGet:
			writeRaw(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/api/tags/"):
			writeRaw(w, rawNoSharing)
		case r.URL.Path == "/api/tags":
			writeRaw(w, `{"id":"5-7","name":"x"}`)
		}
	})
	ctx := context.Background()
	failMe.Store(true)
	if err := y.EnsureLabels(ctx, testProjectID, labels("a")); err == nil {
		t.Fatal("EnsureLabels succeeded with users/me failing")
	}
	failMe.Store(false)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := y.EnsureLabels(ctx, testProjectID, labels("tag-"+strconv.Itoa(i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := y.EnsureLabels(ctx, testProjectID, labels("b")); err != nil {
		t.Fatal(err)
	}
	// One failed read, one that was kept.
	if got := meCalls.Load(); got != 2 {
		t.Errorf("users/me was read %d times, want 2 (the failure, then once for good)", got)
	}
}

func TestEnsureLabelsStopsWhenTheEligibilityTagCannotBeRead(t *testing.T) {
	y, log := loggedDriver(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/users/me":
			writeRaw(w, rawMe)
		case r.URL.Path == "/api/tags" && r.Method == http.MethodGet:
			writeRaw(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if err := y.EnsureLabels(context.Background(), testProjectID, labels("a")); err == nil {
		t.Fatal("EnsureLabels created a tag without the eligibility tag's sharing settings")
	}
	for _, w := range log.writes() {
		t.Errorf("sent %v", w)
	}
}

func TestEligibilityNameAloneMakesNoRequest(t *testing.T) {
	y, log := loggedDriver(t, func(w http.ResponseWriter, _ *http.Request) { writeRaw(w, "[]") })
	if err := y.EnsureLabels(context.Background(), testProjectID, labels(youtracktest.EligibilityTagName)); err != nil {
		t.Fatal(err)
	}
	if _, err := y.ListIssues(context.Background(), testProjectID, forge.ListIssuesOptions{Labels: []string{youtracktest.EligibilityTagName}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range log.all() {
		if strings.Contains(c, "/tags") || strings.Contains(c, "users/me") {
			t.Errorf("sent %v for the eligibility name alone", c)
		}
	}
}
