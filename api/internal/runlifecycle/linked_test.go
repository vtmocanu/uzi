package runlifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
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
	// A linked card sits in its cached column, never in a label (PRD #4173 M5).
	issue := issueWith(repoID, 4, "opened")
	issue.IssueTrackerColumn = txt("Later")
	fs := &fakeStore{
		moveCtx: linkedMoveCtx(repoID, userID, status),
		issue:   issue,
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

// A person resolved the issue in the issue tracker and the cache still shows it open: the
// driver refuses the write and the marker is cleared, like the cached closed-issue guard.
func TestLinkedMoveIssueResolvedInTheIssueTrackerClearsMarker(t *testing.T) {
	fs, sm, _, l, runID, _, _ := linkedFixture("queued", fmt.Errorf("move: %w", issuetracker.ErrIssueResolved))

	l.notifyOnce(context.Background(), runID, "queued")

	if len(sm.moves) != 1 {
		t.Fatalf("expected one move attempt, got %v", sm.moves)
	}
	if len(fs.cleared) != 1 || fs.cleared[0] != runID {
		t.Fatalf("a resolved issue clears the marker once, got %v", fs.cleared)
	}
	if len(fs.recorded) != 0 {
		t.Fatal("board_column must not be recorded for a refused move")
	}
}

func TestLinkedMoveOtherErrorStillRetries(t *testing.T) {
	fs, _, _, l, runID, _, _ := linkedFixture("queued", errors.New("youtrack unreachable"))

	l.notifyOnce(context.Background(), runID, "queued")

	if len(fs.cleared) != 0 {
		t.Fatal("a transient resolver/tracker error leaves the marker")
	}
}

// probeTracker answers the lifecycle's uncached-issue probe (PRD #4173 M5): GetIssue
// returns issue and err, and every call is counted.
type probeTracker struct {
	forge.IssueTracker
	issue forge.Issue
	err   error
	reads int
}

func (p *probeTracker) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	p.reads++
	return p.issue, p.err
}

// A linked run whose issue is not cached asks the issue tracker (PRD #4173 M5). The write
// guard marks a not-found read as a moved-away issue, so the not-found case is built with
// the guard over a cause that is a forge not-found.
func TestLinkedMoveUncachedIssueProbe(t *testing.T) {
	missing := &forge.IssueNotFoundError{Err: errors.New("404")}
	tests := []struct {
		name      string
		tracker   func() forge.IssueTracker
		wantClear bool
		noTagName bool // the link carries no eligibility tag name
	}{
		{"moved away (not found) clears the marker", func() forge.IssueTracker { return forgesvc.UnavailableIssueTracker(missing) }, true, false},
		{"found without the eligibility tag clears the marker", func() forge.IssueTracker {
			return &probeTracker{issue: forge.Issue{IID: 4, Labels: []string{"other"}}}
		}, true, false},
		{"found and eligible keeps the marker", func() forge.IssueTracker {
			return &probeTracker{issue: forge.Issue{IID: 4, Labels: []string{"other", "uzi-ready"}}}
		}, false, false},
		{"found with no eligibility tag name keeps the marker", func() forge.IssueTracker {
			return &probeTracker{issue: forge.Issue{IID: 4, Labels: []string{"other"}}}
		}, false, true},
		{"a read error keeps the marker", func() forge.IssueTracker {
			return &probeTracker{err: errors.New("youtrack unreachable")}
		}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs, sm, src, l, runID, _, _ := linkedFixture("queued", nil)
			fs.issueErr = pgx.ErrNoRows // not in the cache
			fs.moveCtx.EligibilityTagName = txt("uzi-ready")
			if tc.noTagName {
				fs.moveCtx.EligibilityTagName = pgtype.Text{}
			}
			src.tr = tc.tracker()

			l.notifyOnce(context.Background(), runID, "queued")

			if len(sm.moves) != 0 {
				t.Fatalf("an uncached issue is never moved: %v", sm.moves)
			}
			if got := len(fs.cleared) == 1; got != tc.wantClear {
				t.Fatalf("cleared = %v, want clear=%v", fs.cleared, tc.wantClear)
			}
		})
	}
}

// A refused move leaves move_pending_since so reconciliation can retry it after the
// owner saves the mapping (PRD #4173 M5); a skippable write still clears it.
func TestLinkedMoveRefusedColumnKeepsMarker(t *testing.T) {
	for name, err := range map[string]error{
		"column not mapped": fmt.Errorf("move: %w", issuetracker.ErrColumnNotMapped),
		"mapping unset":     fmt.Errorf("move: %w", issuetracker.ErrStateMappingUnset),
		"state not allowed": fmt.Errorf("move: %w", issuetracker.ErrStateNotAllowed),
		"state machine":     fmt.Errorf("move: %w", issuetracker.ErrStateMachineField),
		"state unreadable":  fmt.Errorf("move: %w", issuetracker.ErrStateFieldUnreadable),
	} {
		t.Run(name, func(t *testing.T) {
			fs, sm, _, l, runID, _, _ := linkedFixture("queued", err)

			l.notifyOnce(context.Background(), runID, "queued")

			if len(sm.moves) != 1 {
				t.Fatalf("expected one move attempt, got %v", sm.moves)
			}
			if len(fs.cleared) != 0 || len(fs.recorded) != 0 {
				t.Fatalf("a refused move keeps the marker and records nothing: cleared=%v recorded=%v", fs.cleared, fs.recorded)
			}
		})
	}
}

// The manual-drag guard compares the cached column of a linked card (PRD #4173 M5): a
// card the lifecycle did not leave in the baseline column is a manual drag.
func TestLinkedMoveManualDragIsReadFromTheCachedColumn(t *testing.T) {
	fs, sm, _, l, runID, _, _ := linkedFixture("queued", nil)
	fs.issue.IssueTrackerColumn = txt("Upcoming")

	l.notifyOnce(context.Background(), runID, "queued")

	if len(sm.moves) != 0 || len(fs.cleared) != 1 {
		t.Fatalf("a card in another column is a manual drag: moves=%v cleared=%v", sm.moves, fs.cleared)
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

// Converted from TestLinkedTerminalCommentUsesResolverAndLogsSkip (PRD #4173 M5 U6): the
// note is no longer refused with ErrIssueWriteNotOffered, it lands on the resolver's
// tracker for the run's issue.
func TestLinkedTerminalCommentLandsOnTheResolversTracker(t *testing.T) {
	fs, _, src, l, runID, _, _ := linkedFixture("failed", nil)
	fs.moveCtx.AutoApprove = true
	note := &fakeForge{}
	src.tr = note

	l.notifyOnce(context.Background(), runID, "failed")

	if len(note.notes) != 1 || note.notes[0].issueIID != fs.moveCtx.IssueIid.Int64 || !strings.Contains(note.notes[0].body, "Autopilot") {
		t.Fatalf("the comment goes to the resolver's tracker for the run's issue, got %+v", note.notes)
	}
	if fs.claimCalls != 1 {
		t.Fatalf("record-then-comment is unchanged, claims=%d", fs.claimCalls)
	}
}

// A failed linked comment (the issue tracker answers an error) is logged and the
// lifecycle carries on: one attempt, no retry.
func TestLinkedTerminalCommentFailureIsLogged(t *testing.T) {
	fs, _, src, l, runID, _, _ := linkedFixture("failed", nil)
	fs.moveCtx.AutoApprove = true
	note := &fakeForge{noteErr: errors.New("youtrack answered 500")}
	src.tr = note
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l.notifyOnce(context.Background(), runID, "failed")

	if len(note.notes) != 1 || fs.claimCalls != 1 {
		t.Fatalf("notes=%d claims=%d", len(note.notes), fs.claimCalls)
	}
	if !strings.Contains(logs.String(), "post autopilot terminal comment") || !strings.Contains(logs.String(), "500") {
		t.Fatalf("the failure was not logged: %s", logs.String())
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
