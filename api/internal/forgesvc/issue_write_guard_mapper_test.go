package forgesvc

import (
	"context"
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// The guard is what the resolver hands every consumer of a linked repo's issue side, so
// it must satisfy both halves of what issueinput asks of it.
var (
	_ issueinput.AuthorMapper    = (*issueWriteGuard)(nil)
	_ issueinput.IssueTextMapper = (*issueWriteGuard)(nil)
	_ issueinput.LinkedIssueSide = (*issueWriteGuard)(nil)
)

func youTrackBehindTheGuard(t *testing.T, srv *youtracktest.Server, token string) forge.IssueTracker {
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
		Token:          token,
		Project:        issuetracker.ProjectRef{ID: youtracktest.ProjectID, ShortName: youtracktest.ProjectShortName},
		ProjectID:      77,
		EligibilityTag: issuetracker.TagRef{ID: youtracktest.EligibilityTagID, Name: youtracktest.EligibilityTagName},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &issueWriteGuard{inner: d}
}

// PRD #4173 D11: over the real YouTrack driver the guard maps the connection account's
// login, and only that; a failing account lookup maps nobody and says so.
func TestGuardOverYouTrackMapsTheConnectionAccountLogin(t *testing.T) {
	srv := youtracktest.NewServer(t)
	ctx := context.Background()

	m := youTrackBehindTheGuard(t, srv, srv.Token).(issueinput.AuthorMapper)
	if ok, err := m.MapsAuthor(ctx, youtracktest.BotLogin); !ok || err != nil {
		t.Fatalf("the account's own login: %v, %v", ok, err)
	}
	for _, login := range []string{youtracktest.OtherLogin, ""} {
		if ok, err := m.MapsAuthor(ctx, login); ok || err != nil {
			t.Fatalf("login %q: %v, %v; want false, nil", login, ok, err)
		}
	}

	broken := youTrackBehindTheGuard(t, srv, "a-wrong-token-of-sufficient-length-xyz").(issueinput.AuthorMapper)
	if ok, err := broken.MapsAuthor(ctx, youtracktest.BotLogin); ok || err == nil {
		t.Fatalf("a failed users/me must map nobody with an error, got %v, %v", ok, err)
	}
}

// A guard around an inner side that cannot map (UnavailableIssueTracker, a stub) maps
// nobody, with no error: not mapped is permission_unknown downstream, never a crash.
func TestGuardOverAnInnerThatCannotMapMapsNobody(t *testing.T) {
	for name, tr := range map[string]forge.IssueTracker{
		"unavailable": UnavailableIssueTracker(context.DeadlineExceeded),
		"stub":        &issueWriteGuard{inner: &fakeForge{}},
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := tr.(issueinput.AuthorMapper)
			if !ok {
				t.Fatal("the guard must always implement AuthorMapper")
			}
			if mapped, err := m.MapsAuthor(context.Background(), "anyone"); mapped || err != nil {
				t.Fatalf("MapsAuthor = %v, %v; want false, nil", mapped, err)
			}
		})
	}
}

// D11 for the issue text forwards through the guard to the real driver: the
// account's own text maps, text edited by another user does not.
func TestGuardOverYouTrackForwardsIssueTextMapping(t *testing.T) {
	srv := youtracktest.NewServer(t)
	ctx := context.Background()
	own := srv.SeedIssue("t", "d")
	edited := srv.SeedIssue("t", "d")
	changed := "changed"
	srv.SeedTextEdit(edited, youtracktest.OtherLogin, nil, &changed)
	m := youTrackBehindTheGuard(t, srv, srv.Token).(issueinput.IssueTextMapper)
	if ok, err := m.IssueTextMapped(ctx, 77, own); !ok || err != nil {
		t.Fatalf("untouched text: %v, %v; want true, nil", ok, err)
	}
	if ok, err := m.IssueTextMapped(ctx, 77, edited); ok || err != nil {
		t.Fatalf("text edited by another user: %v, %v; want false, nil", ok, err)
	}
}

func TestGuardOverAnInnerWithoutTextMappingMapsNothing(t *testing.T) {
	for name, tr := range map[string]forge.IssueTracker{
		"unavailable": UnavailableIssueTracker(context.DeadlineExceeded),
		"stub":        &issueWriteGuard{inner: &fakeForge{}},
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := tr.(issueinput.IssueTextMapper)
			if !ok {
				t.Fatal("the guard must always implement IssueTextMapper")
			}
			if mapped, err := m.IssueTextMapped(context.Background(), 1, 1); mapped || err != nil {
				t.Fatalf("IssueTextMapped = %v, %v; want false, nil", mapped, err)
			}
		})
	}
}
