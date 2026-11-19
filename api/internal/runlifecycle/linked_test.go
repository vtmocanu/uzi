package runlifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 (DL-5, D10): a linked repo's issue side comes from the resolver, never
// the forge, and a write the resolver skips clears the move marker instead of retrying.

// resolvedTracker marks the issue side the resolver returned.
type resolvedTracker struct{ forge.IssueTracker }

type fakeSources struct {
	calls  []struct{ user, repo uuid.UUID }
	tr     forge.IssueTracker
	err    error
	called int
}

func (f *fakeSources) IssueTrackerForRepo(_ context.Context, userID, repoID uuid.UUID) (forge.IssueTracker, error) {
	f.called++
	f.calls = append(f.calls, struct{ user, repo uuid.UUID }{userID, repoID})
	return f.tr, f.err
}

// seenMover records which issue tracker AutoMove was handed.
type seenMover struct {
	*fakeMover
	seen []forge.IssueTracker
}

func (m *seenMover) AutoMove(ctx context.Context, f forge.IssueTracker, p int64, issue store.Issue, cols []store.BoardColumn, target string) (store.Issue, error) {
	m.seen = append(m.seen, f)
	return m.fakeMover.AutoMove(ctx, f, p, issue, cols, target)
}

func linkedMoveCtx(repoID, userID uuid.UUID, status string) store.GetRunMoveContextRow {
	mc := moveCtx(repoID, 4, status, txt("Later"), nullText())
	mc.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	mc.UserID = userID
	return mc
}

func linkedFixture(status string, autoMoveErr error) (*fakeStore, *seenMover, *fakeSources, *Lifecycle, uuid.UUID, uuid.UUID, uuid.UUID) {
	runID, repoID, userID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: linkedMoveCtx(repoID, userID, status),
		issue:   issueWith(repoID, 4, "opened", "Later"),
		columns: defaultCols(),
	}
	sm := &seenMover{fakeMover: &fakeMover{autoMoveErr: autoMoveErr}}
	src := &fakeSources{tr: &resolvedTracker{}}
	l := New(fs, sm, testOrigin)
	l.now = newTestLifecycle(fs, sm.fakeMover).now
	l.SetIssueSources(src)
	return fs, sm, src, l, runID, repoID, userID
}

func TestLinkedMoveUsesResolverNotForge(t *testing.T) {
	fs, sm, src, l, runID, repoID, userID := linkedFixture("queued", nil)

	l.notifyOnce(context.Background(), runID, "queued")

	if len(sm.seen) != 1 || sm.seen[0] != src.tr {
		t.Fatalf("AutoMove must be handed the resolver's tracker, got %v", sm.seen)
	}
	if len(src.calls) != 1 || src.calls[0].user != userID || src.calls[0].repo != repoID {
		t.Fatalf("resolver must be asked for the run's owner and repo row, got %+v", src.calls)
	}
	if len(fs.recorded) != 1 {
		t.Fatalf("a successful move records board_column, got %v", fs.recorded)
	}
}

func TestLinkedMoveWithoutSourcesFailsClosed(t *testing.T) {
	fs, sm, _, l, runID, _, _ := linkedFixture("queued", nil)
	l.issueSources = nil

	l.notifyOnce(context.Background(), runID, "queued")

	if len(sm.moves) != 0 {
		t.Fatalf("a linked repo with no resolver must never reach AutoMove (forge fallback), got %v", sm.moves)
	}
	if len(fs.cleared) != 0 {
		t.Fatal("an unresolvable issue side is an outage: the marker is left for the reconcile loop")
	}
}

func TestLinkedMoveSkippableWriteClearsMarker(t *testing.T) {
	for name, err := range map[string]error{
		"not offered": fmt.Errorf("update issue labels: %w", forgesvc.ErrIssueWriteNotOffered),
	} {
		t.Run(name, func(t *testing.T) {
			fs, sm, _, l, runID, _, _ := linkedFixture("queued", err)

			l.notifyOnce(context.Background(), runID, "queued")

			if len(sm.moves) != 1 {
				t.Fatalf("expected one move attempt, got %v", sm.moves)
			}
			if len(fs.cleared) != 1 || fs.cleared[0] != runID {
				t.Fatalf("a skippable write clears the marker once (no retry to the give-up window), got %v", fs.cleared)
			}
			if len(fs.recorded) != 0 {
				t.Fatal("board_column must not be recorded for a skipped move")
			}
		})
	}
}

func TestLinkedMoveOtherErrorStillRetries(t *testing.T) {
	fs, _, _, l, runID, _, _ := linkedFixture("queued", errors.New("youtrack unreachable"))

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fs.cleared) != 0 {
		t.Fatal("a transient resolver/tracker error leaves the marker")
	}
}

func TestLinkedMoveIssueNoLongerCachedClearsMarker(t *testing.T) {
	fs, sm, _, l, runID, _, _ := linkedFixture("queued", nil)
	fs.issueErr = pgx.ErrNoRows // the moved issue was evicted from the cache

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fs.cleared) != 1 || len(sm.moves) != 0 {
		t.Fatalf("an evicted linked issue is a permanent skip: cleared=%v moves=%v", fs.cleared, sm.moves)
	}
}

func TestUnlinkedNoLongerCachedKeepsMarker(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()), issueErr: pgx.ErrNoRows, columns: defaultCols()}
	fm := &fakeMover{}
	l := newTestLifecycle(fs, fm)
	src := &fakeSources{}
	l.SetIssueSources(src)

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fs.cleared) != 0 {
		t.Fatal("the unlinked path is unchanged: no cached card leaves the marker")
	}
	if src.called != 0 {
		t.Fatal("an unlinked repo must never consult the resolver")
	}
}

func TestUnlinkedMoveNeverConsultsResolver(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	fs := &fakeStore{
		moveCtx: moveCtx(repoID, 4, "queued", txt("Later"), nullText()),
		issue:   issueWith(repoID, 4, "opened", "Later"),
		columns: defaultCols(),
	}
	fm := &fakeMover{}
	sm := &seenMover{fakeMover: fm}
	l := New(fs, sm, testOrigin)
	src := &fakeSources{}
	l.SetIssueSources(src)

	l.notifyOnce(context.Background(), runID, "queued")

	if src.called != 0 || len(sm.seen) != 1 || sm.seen[0] != forge.IssueTracker(fm.forge) {
		t.Fatalf("an unlinked move uses the mover's forge: resolver calls=%d seen=%v", src.called, sm.seen)
	}
}

func TestLinkedTerminalCommentUsesResolverAndLogsSkip(t *testing.T) {
	fs, _, src, l, runID, _, _ := linkedFixture("failed", nil)
	fs.moveCtx.AutoApprove = true
	note := &fakeForge{noteErr: fmt.Errorf("create issue note: %w", forgesvc.ErrIssueWriteNotOffered)}
	src.tr = note

	l.notifyOnce(context.Background(), runID, "failed")

	if len(note.notes) != 1 {
		t.Fatalf("the comment goes to the resolver's tracker, got %d notes", len(note.notes))
	}
	if fs.claimCalls != 1 {
		t.Fatalf("record-then-comment is unchanged, claims=%d", fs.claimCalls)
	}
}

func TestLinkedTerminalCommentWithoutSourcesPostsNothing(t *testing.T) {
	fs, sm, _, l, runID, _, _ := linkedFixture("failed", nil)
	fs.moveCtx.AutoApprove = true
	l.issueSources = nil

	l.notifyOnce(context.Background(), runID, "failed")

	if sm.forge != nil && len(sm.forge.notes) != 0 {
		t.Fatal("a linked repo with no resolver must not comment through the forge")
	}
}
