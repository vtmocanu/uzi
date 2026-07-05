// Package forge is uzi's forge-generic integration layer. Callers depend only
// on the Forge interface and the neutral domain types below; a driver (GitLab
// today, Forgejo later) is selected by New and never imported directly. This is
// the seam that lets a second forge land without touching handlers, schema, or
// UI flows.
package forge

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Type identifies a forge driver. It maps 1:1 to the forge_connections.forge_type
// column, which is CHECK-constrained to the same set.
type Type string

const (
	// TypeGitLab is the GitLab REST driver.
	TypeGitLab Type = "gitlab"
)

// BotIdentity is the connected bot account, returned by VerifyToken.
type BotIdentity struct {
	// ForgeUserID is the bot's stable numeric user id on the forge.
	ForgeUserID int64
	// Username is the bot's login (e.g. "uzi-bot-vmocanu").
	Username string
}

// Project is a repo the bot has membership on.
type Project struct {
	// ForgeProjectID is the stable numeric project id (not the path, which can
	// be renamed).
	ForgeProjectID    int64
	PathWithNamespace string
	WebURL            string
	DefaultBranch     string
}

// Label is a forge label. Color is optional (used when creating column labels).
type Label struct {
	Name  string
	Color string
}

// Issue is a forge issue as uzi caches it. Description is carried so the caller
// can compute the PRD-link check at fetch time; it is never persisted.
type Issue struct {
	IID         int64
	Title       string
	State       string // "opened" | "closed"
	Labels      []string
	Description string
	Author      string // username, may be empty
	WebURL      string
	UpdatedAt   time.Time
}

// MR states as GitLab reports them on the single-MR GET. The MR-close watcher
// (PRD #24) only acts on the opened↔closed edges; merged and locked are recorded
// but never move a card (a merge closes the issue via `Closes #N`, which the
// existing issue-close sync owns; locked is transient during merge processing).
const (
	MRStateOpened = "opened"
	MRStateClosed = "closed"
	MRStateMerged = "merged"
	MRStateLocked = "locked"
)

// MergeRequest is a forge merge request as the MR-close watcher observes it
// (PRD #24). Only the fields the watcher needs are carried; add more as callers
// require. State is one of the MRState* constants.
type MergeRequest struct {
	IID    int64
	State  string
	WebURL string
}

// ListIssuesOptions filters ListIssues. State is always queried as "all" by the
// driver (the Closed column requires it), so it is not exposed here. Labels is
// ANDed; an empty UpdatedAfter means "no lower bound" (full fetch).
type ListIssuesOptions struct {
	// Labels each issue must carry (AND semantics). Typically []string{"PRD"}.
	Labels []string
	// UpdatedAfter, when non-nil, restricts to issues updated at/after it. The
	// bound is inclusive at second granularity on GitLab; callers dedupe the
	// boundary by upsert.
	UpdatedAfter *time.Time
}

// Forge is the driver contract. Every method takes a context and surfaces
// redacted errors (the PAT never appears in an error string). Implementations
// paginate internally and return complete result sets.
type Forge interface {
	// VerifyToken confirms the PAT works and returns the bot identity.
	VerifyToken(ctx context.Context) (BotIdentity, error)
	// ListProjects returns every project the bot has at least Developer access
	// to (membership=true).
	ListProjects(ctx context.Context) ([]Project, error)
	// ListLabels returns all labels defined on a project.
	ListLabels(ctx context.Context, projectID int64) ([]Label, error)
	// EnsureLabels creates any of the given labels that do not already exist on
	// the project (by name). Existing labels are left untouched.
	EnsureLabels(ctx context.Context, projectID int64, labels []Label) error
	// ListIssues returns issues matching opts, always across state=all.
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
	// GetMergeRequest returns one merge request by its project-scoped IID. The
	// MR-close watcher (PRD #24) polls this for cards parked in Human Review to
	// detect an opened→closed (reviewer rejected the MR) edge.
	GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error)
}

// New constructs a driver for the given forge type. baseURL must already be
// allowlist-validated by the caller (the SSRF guard lives in config, not here).
// timeout bounds every HTTP call the driver makes.
func New(t Type, baseURL, token string, timeout time.Duration) (Forge, error) {
	switch t {
	case TypeGitLab:
		return newGitLab(baseURL, token, timeout)
	default:
		return nil, fmt.Errorf("forge: unsupported forge type %q", t)
	}
}

// timeoutClient builds an *http.Client with a hard per-call timeout. Every
// driver routes its transport through one of these, closing multica's
// untimeouted-DefaultClient wart.
func timeoutClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{Timeout: timeout}
}
