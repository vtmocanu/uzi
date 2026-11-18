package issuesource

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

type stubSources struct {
	tr    forge.IssueTracker
	err   error
	calls int
}

func (s *stubSources) IssueTrackerForRepo(context.Context, uuid.UUID, uuid.UUID) (forge.IssueTracker, error) {
	s.calls++
	return s.tr, s.err
}

func TestForUnlinkedReturnsTheCallersDriverWithoutAskingSources(t *testing.T) {
	own, src := &forgetest.BaseFake{}, &stubSources{tr: &forgetest.BaseFake{}}
	got, err := For(context.Background(), src, false, uuid.New(), uuid.New(), own)
	if err != nil || got != forge.IssueTracker(own) || src.calls != 0 {
		t.Fatalf("got=%v err=%v calls=%d", got, err, src.calls)
	}
}

func TestForLinkedUsesSourcesAndFailsClosedWithout(t *testing.T) {
	own, resolved := &forgetest.BaseFake{}, &forgetest.BaseFake{}
	src := &stubSources{tr: resolved}
	got, err := For(context.Background(), src, true, uuid.New(), uuid.New(), own)
	if err != nil || got != forge.IssueTracker(resolved) {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got, err := For(context.Background(), nil, true, uuid.New(), uuid.New(), own); got != nil || !errors.Is(err, ErrNotWired) {
		t.Fatalf("no sources: got=%v err=%v, want ErrNotWired and no forge fallback", got, err)
	}
	boom := errors.New("cannot resolve")
	if _, err := For(context.Background(), &stubSources{err: boom}, true, uuid.New(), uuid.New(), own); !errors.Is(err, boom) {
		t.Fatalf("resolver error lost: %v", err)
	}
}
