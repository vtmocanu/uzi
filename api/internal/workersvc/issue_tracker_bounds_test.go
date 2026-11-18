package workersvc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 acceptance (S4): issue tracker titles, descriptions and comments reach a
// run through the same issueinput bounds and fencing as forge issue text. These tests
// drive the real YouTrack driver against the YouTrack fake and compare with the forge
// equivalent.

// youTrackDriver builds the real driver against the fake, trusting its certificate.
func youTrackDriver(t *testing.T, srv *youtracktest.Server) issuetracker.Driver {
	t.Helper()
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })
	d, err := issuetracker.New(issuetracker.KindYouTrack, issuetracker.Options{
		BaseURL:        srv.URL,
		Token:          srv.Token,
		Project:        issuetracker.ProjectRef{ID: youtracktest.ProjectID, ShortName: youtracktest.ProjectShortName},
		ProjectID:      77,
		EligibilityTag: issuetracker.TagRef{ID: youtracktest.EligibilityTagID, Name: youtracktest.EligibilityTagName},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// An oversized YouTrack description is classified exactly like an oversized forge issue
// response: the run is refused as too large and none is created.
func TestOversizedIssueDescriptionIsRefusedLikeTheForgeEquivalent(t *testing.T) {
	srv := youtracktest.NewServer(t)
	n := srv.SeedIssue("big", strings.Repeat("a", 3<<20), youtracktest.EligibilityTagID)
	yt := youTrackDriver(t, srv)

	trackers := map[string]forge.IssueTracker{
		"youtrack": yt,
		"forge":    &tooLargeIssueTracker{},
	}
	for name, tr := range trackers {
		t.Run(name, func(t *testing.T) {
			row := linkedRepoRow()
			row.ForgeProjectID = 77 // the driver refuses any other project id
			fs := &fakeStore{repoRow: row, issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}
			svc := New(fs, newBox(t), testParams())
			svc.SetForges(m2Builder{&m2Forge{}})
			svc.SetIssueSources(&fakeSources{tr: tr})
			iid := n
			if name == "forge" {
				iid = 4
			}
			_, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), iid, "caller body", nil, nil, false, nil, nil, nil, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), ErrDescriptionTooLarge.Error()) {
				t.Fatalf("err = %v, want ErrDescriptionTooLarge", err)
			}
			if fs.createRunParams != nil {
				t.Fatal("a run was created from an oversized issue")
			}
		})
	}
}

type tooLargeIssueTracker struct{ forgetest.BaseFake }

func (*tooLargeIssueTracker) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, &forge.ResponseTooLargeError{Provenance: forge.ResponseIssueSuccess}
}

// An oversized YouTrack comment never reaches a run unbounded: the captured thread obeys
// the same byte bound as a forge thread, and a comment of a YouTrack author (never an
// assessed forge account) is withheld behind the fixed placeholder.
func TestOversizedIssueCommentIsBoundedLikeTheForgeEquivalent(t *testing.T) {
	const bound = 32768
	big := strings.Repeat("c", 3<<20)

	srv := youtracktest.NewServer(t)
	n := srv.SeedIssue("card", "body", youtracktest.EligibilityTagID)
	srv.SeedComment(n, "someone", big)
	yt := youTrackDriver(t, srv)
	got, err := issueinput.Fetch(context.Background(), &m2Forge{disposition: forge.AuthorEligible}, yt, 77, n, 1)
	if err != nil {
		t.Fatalf("YouTrack capture: %v", err)
	}
	total := 0
	for _, c := range got.Thread.Comments {
		total += len(c.Body)
		if strings.Contains(c.Body, "cccc") {
			t.Fatal("an unassessed YouTrack comment body reached the capture")
		}
	}
	if total > bound || !got.Unknown {
		t.Fatalf("YouTrack thread bytes=%d unknown=%v, want <= %d and permission_unknown", total, got.Unknown, bound)
	}

	// The forge equivalent: an assessed author's oversized comment is cut to the same bound.
	f := &m2Forge{disposition: forge.AuthorEligible, comments: []forge.IssueComment{{AuthorForgeUserID: 5, Body: big}}}
	want, err := issueinput.Fetch(context.Background(), f, f, 7, 4, 1)
	if err != nil {
		t.Fatalf("forge capture: %v", err)
	}
	ftotal := 0
	for _, c := range want.Thread.Comments {
		ftotal += len(c.Body)
	}
	if ftotal != bound || !want.Thread.Truncated {
		t.Fatalf("forge thread bytes=%d truncated=%v, want exactly %d and truncated", ftotal, want.Thread.Truncated, bound)
	}
}

// An oversized YouTrack comment by the connection's own (mapped) account reaches the run
// through the linked capture and is cut to exactly the forge bound, marked truncated, like
// the forge equivalent above.
func TestOversizedMappedIssueCommentIsTruncatedLikeTheForgeEquivalent(t *testing.T) {
	const bound = 32768
	srv := youtracktest.NewServer(t)
	n := srv.SeedIssue("card", "body", youtracktest.EligibilityTagID)
	srv.SetReporter(n, youtracktest.BotLogin)
	srv.SeedComment(n, youtracktest.BotLogin, strings.Repeat("c", 3<<20))

	row := linkedRepoRow()
	row.ForgeProjectID = 77 // the driver refuses any other project id
	row.EligibilityTagName.String = youtracktest.EligibilityTagName
	fs := &fakeStore{repoRow: row, issueByID: store.Issue{Labels: []byte(`["` + youtracktest.EligibilityTagName + `"]`)}, createRunResult: store.Run{ID: uuid.New()}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(m2Builder{&m2Forge{disposition: forge.AuthorEligible}})
	svc.SetIssueSources(&fakeSources{tr: linkedYouTrack{Driver: youTrackDriver(t, srv)}})
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), n, "", nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var th issueinput.Thread
	if err := json.Unmarshal(fs.createRunParams.IssueComments, &th); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, c := range th.Comments {
		total += len(c.Body)
	}
	if total != bound || !th.Truncated {
		t.Fatalf("YouTrack thread bytes=%d truncated=%v, want exactly %d and truncated", total, th.Truncated, bound)
	}
	if len(th.Comments) == 0 || th.Comments[0].Reason != "" || !strings.Contains(th.Comments[0].Body, "cccc") {
		t.Fatalf("the mapped account's comment did not reach the run: %+v", th.Comments)
	}
}
