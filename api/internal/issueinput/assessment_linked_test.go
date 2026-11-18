package issueinput_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

// side is an issue side for the D11 tests. The embedded nil interface is never
// called: Assessment only asks the side what it is.
type side struct{ forge.IssueTracker }

// linkedSide is a linked repo's issue side that cannot map authors.
type linkedSide struct{ side }

func (linkedSide) LinkedIssueSource() {}

// mappingSide is a linked repo's issue side that maps authors.
type mappingSide struct {
	linkedSide
	maps  map[string]bool
	err   error
	calls []string
}

func (m *mappingSide) MapsAuthor(_ context.Context, login string) (bool, error) {
	m.calls = append(m.calls, login)
	return m.maps[login], m.err
}

var (
	_ issueinput.AuthorMapper    = (*mappingSide)(nil)
	_ issueinput.LinkedIssueSide = linkedSide{}
	_ issueinput.LinkedIssueSide = (*mappingSide)(nil)
	_ forge.IssueTracker         = (*mappingSide)(nil)
)

// D11: a linked repo's author is eligible only when the issue side maps the login;
// the forge's repository membership lookup is never consulted, whatever the account id.
func TestLinkedAssessmentDecidesByMappingAndNeverCallsTheForgeLookup(t *testing.T) {
	boom := errors.New("users/me down")
	for name, tc := range map[string]struct {
		side   forge.IssueTracker
		issue  forge.Issue
		want   forge.AuthorEligibility
		wantOK bool
		mapped int // expected MapsAuthor calls
	}{
		"mapped login":            {side: &mappingSide{maps: map[string]bool{"uzi-bot": true}}, issue: forge.Issue{Author: "uzi-bot"}, want: forge.AuthorEligible, wantOK: true, mapped: 1},
		"mapped login with an id": {side: &mappingSide{maps: map[string]bool{"uzi-bot": true}}, issue: forge.Issue{Author: "uzi-bot", AuthorForgeUserID: 9}, want: forge.AuthorEligible, wantOK: true, mapped: 1},
		"unmapped login":          {side: &mappingSide{maps: map[string]bool{"uzi-bot": true}}, issue: forge.Issue{Author: "someone"}, want: forge.AuthorUnknown, mapped: 1},
		"mapper error":            {side: &mappingSide{maps: map[string]bool{"x": true}, err: boom}, issue: forge.Issue{Author: "x"}, want: forge.AuthorUnknown, mapped: 1},
		"empty login":             {side: &mappingSide{maps: map[string]bool{"": true}}, issue: forge.Issue{AuthorForgeUserID: 9}, want: forge.AuthorUnknown},
		"side that cannot map":    {side: linkedSide{}, issue: forge.Issue{Author: "uzi-bot", AuthorForgeUserID: 9}, want: forge.AuthorUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			l := &lookup{disposition: forge.AuthorEligible}
			a := issueinput.NewAssessment(context.Background(), l, 7, issueinput.WithIssueSide(tc.side))
			defer a.Close()
			d, err := a.Author(tc.issue)
			if d != tc.want || (err == nil) != tc.wantOK {
				t.Fatalf("Author = %v, %v; want %v (ok=%v)", d, err, tc.want, tc.wantOK)
			}
			if l.calls != 0 {
				t.Fatalf("the forge lookup ran %d times for a linked repo", l.calls)
			}
			if m, ok := tc.side.(*mappingSide); ok && len(m.calls) != tc.mapped {
				t.Fatalf("MapsAuthor calls = %v, want %d", m.calls, tc.mapped)
			}
		})
	}
}

// The same login is mapped once per assessment, and two different accounts ids with one
// login are one author.
func TestLinkedAssessmentMemoizesByLogin(t *testing.T) {
	m := &mappingSide{maps: map[string]bool{"uzi-bot": true}}
	a := issueinput.NewAssessment(context.Background(), &lookup{}, 7, issueinput.WithIssueSide(m))
	defer a.Close()
	for _, id := range []int64{0, 4, 5} {
		if d, err := a.Author(forge.Issue{Author: "uzi-bot", AuthorForgeUserID: id}); d != forge.AuthorEligible || err != nil {
			t.Fatalf("id %d: %v %v", id, d, err)
		}
	}
	if len(m.calls) != 1 {
		t.Fatalf("MapsAuthor calls = %v, want one", m.calls)
	}
}

// An unlinked repo's issue side (a forge driver, no marker) keeps the forge lookup.
func TestUnlinkedIssueSideKeepsTheForgeLookup(t *testing.T) {
	for name, opts := range map[string][]issueinput.Option{
		"no side":       nil,
		"unlinked side": {issueinput.WithIssueSide(side{})},
	} {
		t.Run(name, func(t *testing.T) {
			l := &lookup{disposition: forge.AuthorEligible}
			a := issueinput.NewAssessment(context.Background(), l, 7, opts...)
			defer a.Close()
			if d, err := a.Author(forge.Issue{Author: "u", AuthorForgeUserID: 9}); d != forge.AuthorEligible || err != nil || l.calls != 1 {
				t.Fatalf("Author = %v, %v, lookups=%d", d, err, l.calls)
			}
		})
	}
}

// A thread projected by a linked assessment withholds an unmapped author's comment and
// keeps the connection account's own.
func TestProjectThreadOnALinkedSideWithholdsUnmappedAuthors(t *testing.T) {
	m := &mappingSide{maps: map[string]bool{"uzi-bot": true}}
	a := issueinput.NewAssessment(context.Background(), &lookup{disposition: forge.AuthorEligible}, 7, issueinput.WithIssueSide(m))
	defer a.Close()
	th := issueinput.ProjectThread(a, []forge.IssueComment{
		{AuthorUsername: "someone", Body: "SECRET_RAW"},
		{AuthorUsername: "uzi-bot", Body: "kept"},
	}, 1)
	if len(th.Comments) != 2 || th.Comments[0].Body != issueinput.Placeholder || th.Comments[0].Reason != issueinput.Unknown ||
		th.Comments[1].Body != "kept" || th.Comments[1].Reason != "" || !th.Unknown {
		t.Fatalf("thread = %+v", th)
	}
}
