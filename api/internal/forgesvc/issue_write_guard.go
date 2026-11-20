package forgesvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
)

// ErrIssueWriteNotOffered is returned by the write guard for every write to a
// linked external issue tracker: YouTrack writes arrive in PRD #4173 M5, so until
// then no request is sent (S1: uzi stays read-only against the tracker).
//
// It is the issuesource sentinel itself, so a package that cannot import forgesvc
// (workersvc) can wrap and match the same value.
var ErrIssueWriteNotOffered = issuesource.ErrIssueWriteNotOffered

// linkedIssueError marks an *forge.IssueNotFoundError returned by a linked
// repo's issue reads. A forge not-found is never marked, so IssueWriteSkippable
// can tell "the linked issue moved or is gone" (D10: skip, never retry forever)
// from an ordinary forge not-found that existing callers handle on their own.
type linkedIssueError struct{ err error }

func (e *linkedIssueError) Error() string { return e.err.Error() }
func (e *linkedIssueError) Unwrap() error { return e.err }

// IssueWriteSkippable reports whether a caller doing issue-side bookkeeping
// should treat err as a permanent skip: the write is not offered for a linked
// repo, or the linked issue no longer exists where the repo's binding looks.
// A plain forge not-found is not skippable.
func IssueWriteSkippable(err error) bool {
	if errors.Is(err, ErrIssueWriteNotOffered) {
		return true
	}
	var marked *linkedIssueError
	return errors.As(err, &marked)
}

// issueWriteGuard wraps a linked repo's issue tracker driver. Reads pass
// through; every write is refused before any request.
type issueWriteGuard struct {
	inner forge.IssueTracker
}

var (
	_ forge.IssueTracker         = (*issueWriteGuard)(nil)
	_ issueinput.AuthorMapper    = (*issueWriteGuard)(nil)
	_ issueinput.IssueTextMapper = (*issueWriteGuard)(nil)
	_ issueinput.LinkedIssueSide = (*issueWriteGuard)(nil)
)

func markNotFound(err error) error {
	var missing *forge.IssueNotFoundError
	if errors.As(err, &missing) {
		return &linkedIssueError{err: err}
	}
	return err
}

func (g *issueWriteGuard) ListLabels(ctx context.Context, projectID int64) ([]forge.Label, error) {
	return g.inner.ListLabels(ctx, projectID)
}

func (g *issueWriteGuard) ListIssues(ctx context.Context, projectID int64, opts forge.ListIssuesOptions) ([]forge.Issue, error) {
	return g.inner.ListIssues(ctx, projectID, opts)
}

func (g *issueWriteGuard) GetIssue(ctx context.Context, projectID, issueIID int64) (forge.Issue, error) {
	iss, err := g.inner.GetIssue(ctx, projectID, issueIID)
	return iss, markNotFound(err)
}

func (g *issueWriteGuard) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]forge.LabelEvent, error) {
	ev, err := g.inner.ListIssueLabelEvents(ctx, projectID, issueIID)
	return ev, markNotFound(err)
}

func (g *issueWriteGuard) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]forge.IssueComment, error) {
	c, err := g.inner.ListIssueComments(ctx, projectID, issueIID)
	return c, markNotFound(err)
}

// MapsAuthor forwards D11 author mapping to the inner driver when it can map; a
// driver that cannot (UnavailableIssueTracker, a stub) maps nobody. It is a
// read, so the guard does not refuse it.
func (g *issueWriteGuard) MapsAuthor(ctx context.Context, login string) (bool, error) {
	if m, ok := g.inner.(issueinput.AuthorMapper); ok {
		return m.MapsAuthor(ctx, login)
	}
	return false, nil
}

// IssueTextMapped forwards D11 for the issue text to the inner driver when it
// can say; a driver that cannot maps nothing. A read, so it is not refused.
func (g *issueWriteGuard) IssueTextMapped(ctx context.Context, projectID, issueIID int64) (bool, error) {
	if m, ok := g.inner.(issueinput.IssueTextMapper); ok {
		return m.IssueTextMapped(ctx, projectID, issueIID)
	}
	return false, nil
}

func (g *issueWriteGuard) refuse(op string) error {
	return fmt.Errorf("%s: %w", op, ErrIssueWriteNotOffered)
}

func (g *issueWriteGuard) EnsureLabels(context.Context, int64, []forge.Label) error {
	return g.refuse("ensure labels")
}

func (g *issueWriteGuard) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, g.refuse("create issue")
}

func (g *issueWriteGuard) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return g.refuse("update issue labels")
}

func (g *issueWriteGuard) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return g.refuse("update issue description")
}

func (g *issueWriteGuard) SetIssueState(context.Context, int64, int64, forge.IssueState) error {
	return g.refuse("set issue state")
}

func (g *issueWriteGuard) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, g.refuse("create issue note")
}

// UnavailableIssueTracker is the issue side handed to a linked repo whose resolver
// failed (for example a connection removed from the allowlist): every read returns
// cause, which is NOT skippable so no marker is settled over an outage, and every
// write is refused by the write guard. It lets the poller's issue-side steps fail
// or skip per candidate while the code-host steps carry on, instead of dereferencing
// a nil tracker.
func UnavailableIssueTracker(cause error) forge.IssueTracker {
	return &issueWriteGuard{inner: unavailableIssueTracker{cause: cause}}
}

type unavailableIssueTracker struct{ cause error }

func (u unavailableIssueTracker) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return nil, u.cause
}
func (u unavailableIssueTracker) EnsureLabels(context.Context, int64, []forge.Label) error {
	return u.cause
}
func (u unavailableIssueTracker) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, u.cause
}
func (u unavailableIssueTracker) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, u.cause
}
func (u unavailableIssueTracker) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, u.cause
}
func (u unavailableIssueTracker) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return u.cause
}
func (u unavailableIssueTracker) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return u.cause
}
func (u unavailableIssueTracker) SetIssueState(context.Context, int64, int64, forge.IssueState) error {
	return u.cause
}
func (u unavailableIssueTracker) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, u.cause
}
func (u unavailableIssueTracker) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, u.cause
}
func (u unavailableIssueTracker) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, u.cause
}

// LinkedIssueSource marks the guard (and so UnavailableIssueTracker) as a linked
// repo's issue side. A wrapper placed around a resolver's result must forward it,
// or SyncIssues will treat the wrapped tracker as a forge driver.
func (*issueWriteGuard) LinkedIssueSource() {}

// isLinkedTracker reports whether tr is a linked repo's issue side, the only thing
// SyncIssues dispatches on: the choice comes from the resolver's own output, never
// from a separate caller flag that could disagree with it.
func isLinkedTracker(tr forge.IssueTracker) bool {
	_, ok := tr.(interface{ LinkedIssueSource() })
	return ok
}
