package issueinput_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

type lookup struct {
	calls       int
	disposition forge.AuthorEligibility
	err         error
	fn          func(context.Context) (forge.AuthorEligibility, error)
}

func (l *lookup) RepositoryAuthorEligibility(ctx context.Context, _, _ int64) (forge.AuthorEligibility, error) {
	l.calls++
	if l.fn != nil {
		return l.fn(ctx)
	}
	return l.disposition, l.err
}

func TestAssessmentBudgetAndMemoization(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(fmt.Sprint(unresolved), func(t *testing.T) {
			l := &lookup{disposition: forge.AuthorEligible}
			a := issueinput.NewAssessment(context.Background(), l, 7)
			defer a.Close()
			for i := 1; i <= 200; i++ {
				issue := forge.Issue{AuthorForgeUserID: int64(i)}
				if unresolved {
					issue.AuthorForgeUserID = 0
					issue.Author = fmt.Sprintf("missing-%d", i)
				}
				for j := 0; j < 2; j++ {
					d, err := a.Author(issue)
					if unresolved {
						if d != forge.AuthorUnknown || err == nil {
							t.Fatalf("unresolved authorized: %v %v", d, err)
						}
					} else if d != forge.AuthorEligible || err != nil {
						t.Fatalf("author %d: %v %v", i, d, err)
					}
				}
			}
			d, err := a.Author(forge.Issue{AuthorForgeUserID: 201})
			if d != forge.AuthorUnknown || !errors.Is(err, issueinput.ErrAuthorBudget) {
				t.Fatalf("201: %v %v", d, err)
			}
			want := 200
			if unresolved {
				want = 0
			}
			if l.calls != want {
				t.Fatalf("lookups=%d want %d", l.calls, want)
			}
		})
	}
}

func TestAssessmentLookupFailuresConsumeBudget(t *testing.T) {
	for _, positiveFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("positive-first-%t", positiveFirst), func(t *testing.T) {
			failure := errors.New("lookup forbidden")
			l := &lookup{disposition: forge.AuthorEligible, err: failure}
			a := issueinput.NewAssessment(context.Background(), l, 7)
			defer a.Close()
			if positiveFirst {
				l.err = nil
			}
			for id := int64(1); id <= 200; id++ {
				want := forge.AuthorUnknown
				wantErr := failure
				if positiveFirst && id == 1 {
					want, wantErr = forge.AuthorEligible, nil
				}
				for repeat := 0; repeat < 2; repeat++ {
					got, err := a.Author(forge.Issue{AuthorForgeUserID: id})
					if got != want || !errors.Is(err, wantErr) {
						t.Fatalf("author %d: got=%v err=%v want=%v err=%v", id, got, err, want, wantErr)
					}
				}
				l.err = failure
			}
			got, err := a.Author(forge.Issue{AuthorForgeUserID: 201})
			if got != forge.AuthorUnknown || !errors.Is(err, issueinput.ErrAuthorBudget) || l.calls != 200 {
				t.Fatalf("201st author: got=%v err=%v lookups=%d", got, err, l.calls)
			}
			if positiveFirst {
				got, err = a.Author(forge.Issue{AuthorForgeUserID: 1})
				if got != forge.AuthorEligible || err != nil || l.calls != 200 {
					t.Fatalf("completed positive cache: got=%v err=%v lookups=%d", got, err, l.calls)
				}
			}
		})
	}
}

func TestAssessmentErrorsAndFreshOperations(t *testing.T) {
	for _, d := range []forge.AuthorEligibility{forge.AuthorEligible, forge.AuthorNotEligible, forge.AuthorUnknown, 99} {
		l := &lookup{disposition: d, err: errors.New("incomplete evidence")}
		a := issueinput.NewAssessment(context.Background(), l, 7)
		issue := forge.Issue{AuthorForgeUserID: 42, Author: "old"}
		for i := 0; i < 2; i++ {
			got, err := a.Author(issue)
			if got != forge.AuthorUnknown || err == nil {
				t.Fatalf("error authorized: %v %v", got, err)
			}
			issue.Author = "renamed"
		}
		if l.calls != 1 {
			t.Fatal("rename did not memoize stable ID")
		}
		a.Close()
		l.err = nil
		l.disposition = forge.AuthorEligible
		b := issueinput.NewAssessment(context.Background(), l, 7)
		got, err := b.Author(issue)
		b.Close()
		if got != forge.AuthorEligible || err != nil || l.calls != 2 {
			t.Fatalf("fresh operation: %v %v calls=%d", got, err, l.calls)
		}
	}
	a := issueinput.NewAssessment(context.Background(), &lookup{}, 7)
	defer a.Close()
	if d, err := a.Author(forge.Issue{AuthorForgeUserID: 1}); d != forge.AuthorUnknown || err == nil {
		t.Fatal("zero-value answer authorized")
	}
}

func TestAssessmentDeadlineAndCancellation(t *testing.T) {
	start := time.Now()
	a := issueinput.NewAssessment(context.Background(), &lookup{}, 7)
	deadline, ok := a.Context().Deadline()
	if !ok || deadline.Before(start.Add(29*time.Second)) || deadline.After(start.Add(31*time.Second)) {
		t.Fatalf("deadline: %v", deadline)
	}
	a.Close()
	if d, err := a.Author(forge.Issue{AuthorForgeUserID: 1}); d != forge.AuthorUnknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v %v", d, err)
	}
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	l := &lookup{fn: func(ctx context.Context) (forge.AuthorEligibility, error) {
		<-ctx.Done()
		return forge.AuthorEligible, nil
	}}
	b := issueinput.NewAssessment(parent, l, 7)
	defer b.Close()
	if d, err := b.Author(forge.Issue{AuthorForgeUserID: 2}); d != forge.AuthorUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline answer: %v %v", d, err)
	}
}

func TestBaseFakeAuthorDefaultIsLoud(t *testing.T) {
	d, err := (&forgetest.BaseFake{}).RepositoryAuthorEligibility(context.Background(), 7, 42)
	if d != forge.AuthorUnknown || !errors.Is(err, forgetest.ErrNotStubbed) {
		t.Fatalf("default: %v %v", d, err)
	}
}

func TestIndependentCompletedDecisionSurvivesLaterFailure(t *testing.T) {
	l := &lookup{disposition: forge.AuthorEligible}
	a := issueinput.NewAssessment(context.Background(), l, 7)
	defer a.Close()
	first := forge.Issue{AuthorForgeUserID: 1}
	if d, err := a.Author(first); d != forge.AuthorEligible || err != nil {
		t.Fatal(d, err)
	}
	l.err = errors.New("other author forbidden")
	if d, err := a.Author(forge.Issue{AuthorForgeUserID: 2}); d != forge.AuthorUnknown || err == nil {
		t.Fatal(d, err)
	}
	if d, err := a.Author(first); d != forge.AuthorEligible || err != nil || l.calls != 2 {
		t.Fatal(d, err, l.calls)
	}
}
