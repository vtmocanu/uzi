package issueinput

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

type deadlineLookup struct {
	calls int
	wait  bool
}

func (l *deadlineLookup) RepositoryAuthorEligibility(ctx context.Context, project, id int64) (forge.AuthorEligibility, error) {
	l.calls++
	if project != 7 || id != int64(l.calls) {
		return forge.AuthorUnknown, errors.New("unexpected repository or stable author ID")
	}
	if l.wait {
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return forge.AuthorUnknown, errors.New("child did not expire")
		}
	}
	// Model a driver that completes with a positive answer after its child expires.
	return forge.AuthorEligible, nil
}

func TestAssessmentChildDeadlineExpiresWhileParentLive(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	l := &deadlineLookup{}
	a := NewAssessment(parent, l, 7)
	defer a.Close()
	first := forge.Issue{AuthorForgeUserID: 1}
	if got, err := a.Author(first); got != forge.AuthorEligible || err != nil {
		t.Fatalf("completed positive: got=%v err=%v", got, err)
	}

	// Replace only this test's child context; production keeps its 30-second cap.
	child, cancelChild := context.WithTimeout(parent, 50*time.Millisecond)
	defer cancelChild()
	a.ctx = forge.BeginAuthorAssessment(child)
	l.wait = true
	got, err := a.Author(forge.Issue{AuthorForgeUserID: 2})
	if got != forge.AuthorUnknown || !errors.Is(err, context.DeadlineExceeded) || l.calls != 2 {
		t.Fatalf("expired child's late positive: got=%v err=%v lookups=%d", got, err, l.calls)
	}
	if parent.Err() != nil {
		t.Fatalf("parent must remain live: %v", parent.Err())
	}
	if !errors.Is(a.Context().Err(), context.DeadlineExceeded) {
		t.Fatalf("assessment child must have expired: %v", a.Context().Err())
	}
	got, err = a.Author(first)
	if got != forge.AuthorEligible || err != nil || l.calls != 2 {
		t.Fatalf("completed cache after child expiry: got=%v err=%v lookups=%d", got, err, l.calls)
	}
	got, err = a.Author(forge.Issue{AuthorForgeUserID: 3})
	if got != forge.AuthorUnknown || !errors.Is(err, context.DeadlineExceeded) || l.calls != 2 {
		t.Fatalf("new author after child expiry: got=%v err=%v lookups=%d", got, err, l.calls)
	}
}
