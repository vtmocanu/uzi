package forge

import (
	"context"
	"time"
)

// Label is a forge label. Color is optional (used when creating column labels).
type Label struct {
	Name  string
	Color string
}

// Issue is a forge issue as uzi caches it. Description is carried so the caller
// can compute the PRD-link check at fetch time; it is never persisted.
type Issue struct {
	// IsPullRequest distinguishes PRs returned by shared issue endpoints; internal only.
	IsPullRequest     bool `json:"-"`
	IID               int64
	Title             string
	State             string // "opened" | "closed"
	Labels            []string
	Description       string
	Author            string // username, may be empty
	AuthorForgeUserID int64  // stable author identity; zero means unresolved
	WebURL            string
	UpdatedAt         time.Time
	Assignees         []int64 // forge user ids of the issue's assignees (may be empty, never nil after mapping)
}

// LabelEvent is one resource label event on an issue: a record that a user added
// or removed a single label at a point in time. Autopilot reads these to attribute
// the autopilot label to the human who added it (PRD #19 Decision 3). Username is
// the actor; it may be empty for a system-generated event.
type LabelEvent struct {
	ID        int64
	Action    string // "add" | "remove"
	LabelName string
	Username  string
	CreatedAt time.Time
}

// IssueNote is a comment created on an issue, returned by CreateIssueNote.
type IssueNote struct {
	ID   int64
	Body string
}

// IssueComment is one human-authored comment on an issue, returned by
// ListIssueComments oldest-first. It deliberately carries NO System/type field:
// the GitLab driver drops system notes (D2), and the Forgejo/GitHub drivers read
// human-only comment endpoints (their timeline/event notes live behind separate
// endpoints uzi never calls), so the neutral type only ever represents a human
// comment. Bodies are attacker-influenceable free text (same untrusted class as
// Issue.Description).
type IssueComment struct {
	AuthorForgeUserID int64  // stable forge user id of the comment author
	AuthorUsername    string // author login, may be empty
	Body              string // comment body (untrusted)
	CreatedAt         time.Time
}

// IssueState is the neutral issue-state vocabulary, matching the values the
// drivers already normalize their RESPONSES to and the values the issue cache
// stores in issues.state.
//
// The neutral spelling is GitLab's, not Forgejo's, for the historical reason that
// GitLab was the only driver when the cache's state='opened' filter was written.
// Forgejo says "open"; forgejoIssueState maps that INBOUND, and
// forgejoIssueStateParam maps it OUTBOUND — that function's comment is where the
// asymmetry is explained, and it is the one that matters, since an untranslated
// neutral value is not rejected on the Forgejo lane, it is silently ignored.
//
// (This used to point at StateAll's note for the outbound explanation. StateAll's
// note says nothing about it — it documents the zero value.)
type IssueState string

const (
	// StateAll is the default and reproduces pre-M6 behaviour exactly: every
	// caller that leaves ListIssuesOptions.State zero gets "all".
	StateAll IssueState = ""
	// StateOpened restricts to open issues.
	StateOpened IssueState = "opened"
	// StateClosed restricts to closed issues.
	StateClosed IssueState = "closed"
)

// ListIssuesOptions filters ListIssues. Labels is ANDed; an empty UpdatedAfter
// means "no lower bound" (full fetch).
type ListIssuesOptions struct {
	// Labels each issue must carry (AND semantics). Typically []string{"PRD"}.
	Labels []string
	// UpdatedAfter, when non-nil, restricts to issues updated at/after it. The
	// bound is inclusive at second granularity on GitLab; callers dedupe the
	// boundary by upsert.
	UpdatedAfter *time.Time
	// State restricts by issue state. The ZERO VALUE IS StateAll, which is what
	// every pre-M6 caller relied on: the Closed column and de-label/close eviction
	// both need to see closed issues, and the drivers used to hardcode it.
	//
	// Added by PRD #102 Decision 10 for M6's additive non-PRD fetch, which wants
	// open-only — without it that fetch would pull the entire closed history on
	// every reconcile purely to discard it.
	State IssueState
	// Limit optionally caps the number of issues returned. Limit == 0 means NO CAP:
	// the driver paginates internally and returns the COMPLETE set, preserving the
	// interface's "return complete result sets" contract for every current caller,
	// all of which leave this zero. A positive Limit tells the driver to stop once
	// that many issues have been collected (truncating a page as needed), so the
	// worker forge read (PRD #158) can bound a list without the driver having to walk
	// the whole project. It is a soft ceiling on count only — it does not change
	// filtering, ordering, or the shape of any returned issue.
	Limit int
}

// IssueTracker is the issue-side half of the driver contract: issues, labels,
// comments and issue state. It is split out of Forge (PRD #4173) so a later phase
// can let a non-forge tracker implement it without the code-host methods.
type IssueTracker interface {
	// ListLabels returns all labels defined on a project.
	ListLabels(ctx context.Context, projectID int64) ([]Label, error)
	// EnsureLabels creates any of the given labels that do not already exist on
	// the project (by name). Existing labels are left untouched.
	EnsureLabels(ctx context.Context, projectID int64, labels []Label) error
	// ListIssues returns issues matching opts. State defaults to StateAll (the
	// zero value), which is what every pre-M6 caller relies on; each driver
	// translates it into its own request vocabulary. opts.Limit optionally caps the
	// number of issues returned — zero (the default) means no cap and the complete
	// set is returned as ever, a positive value stops pagination once that many are
	// collected.
	ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error)
	// GetIssue returns one issue by its project-scoped IID. Its Description is
	// populated (the run-create path snapshots it), unlike the cached list rows.
	GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error)
	// CreateIssue opens a new issue with the given title, description, and labels
	// and returns it. uzi never fabricates a local-only card — the forge stays the
	// source of truth and the board picks the issue up on the next sync.
	CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error)
	// UpdateIssueLabels adds and removes labels on an issue in one call. On
	// GitLab this is atomic (a single add_labels/remove_labels update);
	// single-column enforcement relies on that atomicity.
	UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error
	// UpdateIssueDescription replaces an issue's description wholesale (PRD #72 M5).
	// It is a read-modify-write from the CALLER's point of view — the caller reads
	// via GetIssue, rewrites, and writes back — because no forge here offers a patch
	// primitive. A driver may additionally need its own internal read to satisfy its
	// forge's API shape; that is the driver's business, not the interface's.
	UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error
	// SetIssueState closes or reopens an issue, forge-first. state must be
	// StateOpened or StateClosed (the two live states); other values are a caller
	// bug. Each driver translates the neutral state into its own mutate vocabulary
	// (GitLab's state_event close/reopen, GitHub's state+state_reason, Forgejo's
	// state), sending state alone so nothing else on the issue is clobbered.
	SetIssueState(ctx context.Context, projectID, issueIID int64, state IssueState) error
	// ListIssueLabelEvents returns the issue's resource label events (who added or
	// removed which label, when), oldest first, paginated internally. Autopilot
	// uses it to attribute the autopilot label to the human who added it (PRD #19
	// Decision 3). No caller until the autopilot-trigger milestone.
	ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error)
	// ListIssueComments returns the issue's HUMAN comments, oldest first,
	// paginated internally and bounded by the shared forge sanity ceilings
	// (maxForgeItems/maxForgePages) — no per-call limit (PRD #381 D4; the size
	// bound is an assembly concern, not the driver's). Each driver filters out
	// forge system/timeline notes (D2) and normalizes ordering to oldest-first
	// (D8). Bodies are untrusted free text. AuthorForgeUserID lets the caller drop
	// uzi's own bot-authored comments (D1). No caller until PRD #381's plumbing.
	ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]IssueComment, error)
	// CreateIssueNote posts a comment on an issue and returns it. Autopilot uses
	// it to surface an outcome — no eligible user, a run failure, or the success MR
	// link — back on the forge so a forge-only user is never left waiting (PRD #19
	// Decision 6). No caller until the autopilot trigger/execution milestones.
	CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error)
}
