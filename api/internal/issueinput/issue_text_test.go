package issueinput_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

// textSide is a linked issue side that maps authors and the issue text.
type textSide struct {
	mappingSide
	text      bool
	textErr   error
	textCalls int
}

func (s *textSide) IssueTextMapped(context.Context, int64, int64) (bool, error) {
	s.textCalls++
	return s.text, s.textErr
}

var _ issueinput.IssueTextMapper = (*textSide)(nil)

func TestIssueTextIsEligibleOnlyWhenReporterAndTextMap(t *testing.T) {
	boom := errors.New("activities down")
	for name, tc := range map[string]struct {
		side      *textSide
		issue     forge.Issue
		want      forge.AuthorEligibility
		wantErr   bool
		textCalls int
	}{
		"reporter and text map": {&textSide{mappingSide: mappingSide{maps: map[string]bool{"bot": true}}, text: true}, forge.Issue{IID: 3, Author: "bot"}, forge.AuthorEligible, false, 1},
		"text edited by other":  {&textSide{mappingSide: mappingSide{maps: map[string]bool{"bot": true}}}, forge.Issue{IID: 3, Author: "bot"}, forge.AuthorUnknown, true, 1},
		"text read fails":       {&textSide{mappingSide: mappingSide{maps: map[string]bool{"bot": true}}, textErr: boom, text: true}, forge.Issue{IID: 3, Author: "bot"}, forge.AuthorUnknown, true, 1},
		"reporter unmapped":     {&textSide{mappingSide: mappingSide{maps: map[string]bool{"bot": true}}, text: true}, forge.Issue{IID: 3, Author: "other"}, forge.AuthorUnknown, true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			a := issueinput.NewAssessment(context.Background(), &lookup{}, 7, issueinput.WithIssueSide(tc.side))
			defer a.Close()
			d, err := a.IssueText(tc.issue)
			if d != tc.want || (err != nil) != tc.wantErr || tc.side.textCalls != tc.textCalls {
				t.Fatalf("IssueText = %v, %v (text calls %d); want %v, err=%v, calls %d", d, err, tc.side.textCalls, tc.want, tc.wantErr, tc.textCalls)
			}
		})
	}
	t.Run("a side without the text interface", func(t *testing.T) {
		a := issueinput.NewAssessment(context.Background(), &lookup{}, 7,
			issueinput.WithIssueSide(&mappingSide{maps: map[string]bool{"bot": true}}))
		defer a.Close()
		if d, err := a.IssueText(forge.Issue{IID: 3, Author: "bot"}); d != forge.AuthorUnknown || err == nil {
			t.Fatalf("IssueText = %v, %v; want unknown", d, err)
		}
	})
}

func TestIssueTextOnAnUnlinkedSideIsAuthor(t *testing.T) {
	l := &lookup{disposition: forge.AuthorEligible}
	a := issueinput.NewAssessment(context.Background(), l, 7, issueinput.WithIssueSide(side{}))
	defer a.Close()
	d, err := a.IssueText(forge.Issue{IID: 3, Author: "u", AuthorForgeUserID: 9})
	if d != forge.AuthorEligible || err != nil || l.calls != 1 {
		t.Fatalf("IssueText = %v, %v, lookups=%d", d, err, l.calls)
	}
}

// The text verdict is never cached under the login: an issue whose text is not
// mapped does not turn the reporter's comments unmapped, and a comment by the
// reporter does not stand in for the issue text.
func TestIssueTextVerdictIsNotCachedUnderTheLogin(t *testing.T) {
	maps := map[string]bool{"bot": true}
	t.Run("text first", func(t *testing.T) {
		s := &textSide{mappingSide: mappingSide{maps: maps}}
		a := issueinput.NewAssessment(context.Background(), &lookup{}, 7, issueinput.WithIssueSide(s))
		defer a.Close()
		if d, _ := a.IssueText(forge.Issue{IID: 3, Author: "bot"}); d != forge.AuthorUnknown {
			t.Fatalf("IssueText = %v, want unknown", d)
		}
		if d, err := a.Author(forge.Issue{Author: "bot"}); d != forge.AuthorEligible || err != nil {
			t.Fatalf("Author after an unmapped text = %v, %v; want eligible", d, err)
		}
	})
	t.Run("author first", func(t *testing.T) {
		s := &textSide{mappingSide: mappingSide{maps: maps}}
		a := issueinput.NewAssessment(context.Background(), &lookup{}, 7, issueinput.WithIssueSide(s))
		defer a.Close()
		if d, err := a.Author(forge.Issue{Author: "bot"}); d != forge.AuthorEligible || err != nil {
			t.Fatalf("Author = %v, %v", d, err)
		}
		if d, _ := a.IssueText(forge.Issue{IID: 3, Author: "bot"}); d != forge.AuthorUnknown {
			t.Fatalf("IssueText after a mapped author = %v, want unknown", d)
		}
	})
}

// orderedSide records the order of the reads Fetch makes of a linked side.
type orderedSide struct {
	forgetest.BaseFake
	order []string
}

func (o *orderedSide) LinkedIssueSource() {}
func (o *orderedSide) MapsAuthor(context.Context, string) (bool, error) {
	o.order = append(o.order, "MapsAuthor")
	return true, nil
}
func (o *orderedSide) IssueTextMapped(context.Context, int64, int64) (bool, error) {
	o.order = append(o.order, "IssueTextMapped")
	return true, nil
}
func (o *orderedSide) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	o.order = append(o.order, "GetIssue")
	return forge.Issue{IID: 11, Title: "t", Author: "bot"}, nil
}
func (o *orderedSide) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	o.order = append(o.order, "ListIssueComments")
	return nil, nil
}

func TestFetchReadsTheTextAfterTheIssueAndBeforeTheComments(t *testing.T) {
	o := &orderedSide{}
	c, err := issueinput.Fetch(context.Background(), o, o, 7, 11, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"GetIssue", "MapsAuthor", "IssueTextMapped", "ListIssueComments"}; !slices.Equal(o.order, want) {
		t.Fatalf("order = %v, want %v", o.order, want)
	}
	if c.Unknown || c.Withheld {
		t.Fatalf("capture = %+v, want an eligible issue", c)
	}
}

func TestFetchOfAnIssueWithUnmappedTextIsPermissionUnknown(t *testing.T) {
	s := &fetchTextSide{}
	c, err := issueinput.Fetch(context.Background(), s, s, 7, 11, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Unknown || c.Reason != issueinput.Unknown {
		t.Fatalf("capture = %+v, want permission_unknown", c)
	}
}

type fetchTextSide struct{ orderedSide }

func (*fetchTextSide) IssueTextMapped(context.Context, int64, int64) (bool, error) { return false, nil }
