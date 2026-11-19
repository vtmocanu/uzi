package forgesvc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// ErrIssueWriteNotOffered is returned by the write guard for the one write to a linked
// external issue tracker that is not offered: an UpdateIssueLabels that removes the
// eligibility name (the eligibility tag is never stripped). No request is sent.
// MoveToColumn, SetIssueState, CreateIssue, CreateIssueNote, UpdateIssueDescription,
// EnsureLabels and the other label writes pass through (PRD #4173 M5).
//
// It is the issuesource sentinel itself, so a package that cannot import forgesvc
// (workersvc) can wrap and match the same value.
var ErrIssueWriteNotOffered = issuesource.ErrIssueWriteNotOffered

// linkedIssueError marks an *forge.IssueNotFoundError returned by a linked
// repo's issue reads and writes. A forge not-found is never marked, so IssueWriteSkippable
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
	return LinkedIssueGone(err)
}

// LinkedIssueGone reports whether err says a linked repo's issue is no longer in
// the linked project (D10), as opposed to the write not being offered.
func LinkedIssueGone(err error) bool {
	var marked *linkedIssueError
	return errors.As(err, &marked)
}

// ColumnMoveRefused reports whether err is the issue tracker driver refusing a
// card move or State write before any request: the column has no State mapped,
// the mapping is unset, or the target State is not allowed (PRD #4173 M5). It is
// deliberately not part of IssueWriteSkippable: a refused move may succeed once
// the owner saves the mapping, so a caller keeps its retry marker for it.
func ColumnMoveRefused(err error) bool {
	return errors.Is(err, issuetracker.ErrColumnNotMapped) ||
		errors.Is(err, issuetracker.ErrStateMappingUnset) ||
		errors.Is(err, issuetracker.ErrStateNotAllowed)
}

// LinkedIssueResolved reports whether err is the issue tracker driver refusing a
// card move because the issue is resolved in the issue tracker
// (issuetracker.ErrIssueResolved). Any column move (MoveToColumn) other than an
// explicit reopen of a closed card is refused this way: an automatic move, or a board
// drag to a column of a card the cache shows open. A drag to Closed is a close, not a
// column move, and is not refused. It is deliberately not part of IssueWriteSkippable
// or ColumnMoveRefused: retrying cannot help, and the person's resolution wins until
// the next sync brings it into the cache.
func LinkedIssueResolved(err error) bool {
	return errors.Is(err, issuetracker.ErrIssueResolved)
}

// issueWriteGuard wraps a linked repo's issue tracker driver. Reads pass
// through, as do all writes (PRD #4173 M5), each marked when the issue moved away; the
// only write refused before any request is a label write that removes the eligibility
// name.
type issueWriteGuard struct {
	inner forge.IssueTracker
	// eligibility is the link's stored eligibility tag name; "" when none is known
	// (UnavailableIssueTracker), where the inner driver returns its cause instead.
	eligibility string
}

var (
	_ forge.IssueTracker           = (*issueWriteGuard)(nil)
	_ issueinput.AuthorMapper      = (*issueWriteGuard)(nil)
	_ issueinput.IssueTextMapper   = (*issueWriteGuard)(nil)
	_ issueinput.LinkedIssueSide   = (*issueWriteGuard)(nil)
	_ issuesource.IssueStateLister = (*issueWriteGuard)(nil)
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

// ListIssuesWithState forwards the listing with State values when the inner
// driver offers it. A driver that cannot (unavailableIssueTracker) is an error,
// never an empty result, because the linked full sync evicts every cached issue
// the listing does not return.
func (g *issueWriteGuard) ListIssuesWithState(ctx context.Context, projectID int64, opts forge.ListIssuesOptions) ([]issuesource.StatedIssue, error) {
	l, ok := g.inner.(issuesource.IssueStateLister)
	if !ok {
		return nil, fmt.Errorf("list issues with state: the issue tracker driver cannot list State values: %w", errNoStateListing)
	}
	return l.ListIssuesWithState(ctx, projectID, opts)
}

// errNoStateListing marks an inner driver that has no ListIssuesWithState.
var errNoStateListing = errors.New("listing with issue tracker States is unavailable")

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

// EnsureLabels passes through (PRD #4173 M5). The driver never creates the eligibility
// tag; the handler's explicit label-ensure endpoint refuses a linked repo before this.
func (g *issueWriteGuard) EnsureLabels(ctx context.Context, projectID int64, labels []forge.Label) error {
	return markNotFound(g.inner.EnsureLabels(ctx, projectID, labels))
}

// CreateIssue passes through (PRD #4173 M5); the caller has already rewritten the label
// set with issuesource.LinkedLabels.
func (g *issueWriteGuard) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (forge.Issue, error) {
	iss, err := g.inner.CreateIssue(ctx, projectID, title, description, labels)
	return iss, markNotFound(err)
}

// UpdateIssueLabels passes through unless the removal set contains the eligibility name:
// that is refused before any request, so the eligibility tag is never stripped.
func (g *issueWriteGuard) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if g.eligibility != "" && slices.Contains(remove, g.eligibility) {
		return g.refuse("update issue labels")
	}
	return markNotFound(g.inner.UpdateIssueLabels(ctx, projectID, issueIID, add, remove))
}

// MoveToColumn passes through to the issue tracker driver (PRD #4173 M5). The
// driver refuses an unmapped column or a State it may not write before any
// request; a moved-away issue is marked so IssueWriteSkippable stays true (D10).
func (g *issueWriteGuard) MoveToColumn(ctx context.Context, projectID, issueIID int64, move forge.ColumnMove) error {
	return markNotFound(g.inner.MoveToColumn(ctx, projectID, issueIID, move))
}

// UpdateIssueDescription passes through (PRD #4173 M5); a moved-away issue is marked.
func (g *issueWriteGuard) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	return markNotFound(g.inner.UpdateIssueDescription(ctx, projectID, issueIID, description))
}

// SetIssueState passes through to the issue tracker driver, which writes the
// stored closed or open State (PRD #4173 M5); a moved-away issue is marked.
func (g *issueWriteGuard) SetIssueState(ctx context.Context, projectID, issueIID int64, state forge.IssueState) error {
	return markNotFound(g.inner.SetIssueState(ctx, projectID, issueIID, state))
}

// CreateIssueNote passes through (PRD #4173 M5): the note lands as a comment on the
// linked issue; a moved-away issue is marked.
func (g *issueWriteGuard) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (forge.IssueNote, error) {
	n, err := g.inner.CreateIssueNote(ctx, projectID, issueIID, body)
	return n, markNotFound(err)
}

// UnavailableIssueTracker is the issue side handed to a linked repo whose resolver
// failed (for example a connection removed from the allowlist): every read returns
// cause, which is NOT skippable so no marker is settled over an outage, as does a
// move, State, create, note, description or label write (it has no eligibility name,
// so the guard refuses nothing). It lets the poller's issue-side steps fail or skip
// per candidate while the code-host steps carry on, instead of dereferencing a nil
// tracker.
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
func (u unavailableIssueTracker) ListIssuesWithState(context.Context, int64, forge.ListIssuesOptions) ([]issuesource.StatedIssue, error) {
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
func (u unavailableIssueTracker) MoveToColumn(context.Context, int64, int64, forge.ColumnMove) error {
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

// isUnavailableTracker reports whether tr is the UnavailableIssueTracker placeholder, a
// linked issue side whose every read, move and State write fails with its cause.
func isUnavailableTracker(tr forge.IssueTracker) bool {
	g, ok := tr.(*issueWriteGuard)
	if !ok {
		return false
	}
	_, unavailable := g.inner.(unavailableIssueTracker)
	return unavailable
}

// isTransientUnavailableTracker reports whether tr is the UnavailableIssueTracker built
// from a failed repo-source database read (errRepoSourceRead), the only unavailable cause
// classified as transient. Other transient failures, such as the connection-row read inside
// DriverFor, are not classified: they are recorded without a move.
func isTransientUnavailableTracker(tr forge.IssueTracker) bool {
	g, ok := tr.(*issueWriteGuard)
	if !ok {
		return false
	}
	u, unavailable := g.inner.(unavailableIssueTracker)
	return unavailable && errors.Is(u.cause, errRepoSourceRead)
}
