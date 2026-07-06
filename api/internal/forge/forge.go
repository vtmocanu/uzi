// Package forge is uzi's forge-generic integration layer. Callers depend only
// on the Forge interface and the neutral domain types below; a driver (GitLab
// today, Forgejo later) is selected by New and never imported directly. This is
// the seam that lets a second forge land without touching handlers, schema, or
// UI flows.
package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrTokenIntrospectionUnsupported is returned by TokenInfo when the forge does
// not expose the token-introspection endpoint (GitLab < 15.5, which 404s
// GET /personal_access_tokens/self). It is a distinct sentinel — not a redacted
// generic error — so the privilege checker can downgrade "cannot verify scopes"
// to a warning instead of a hard block. It carries no secret material.
var ErrTokenIntrospectionUnsupported = errors.New("forge: token introspection not supported by this forge version")

// ErrNoPipeline is returned by LatestPipeline / LatestMRPipeline when the ref (or
// MR) has no pipeline at all: a project with no CI configured, or a ref/MR that
// has never triggered one. It is a distinct sentinel — not a redacted generic
// error — so the pipeline sync (PRD #6) can cache "no CI" for the ref instead of
// treating it as a failure. It carries no secret material.
var ErrNoPipeline = errors.New("forge: no pipeline for ref")

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
	// IsAdmin reports whether the bot user is an instance administrator. GitLab
	// includes is_admin on GET /user only when the caller is an admin, so an
	// absent field decodes to false — exactly the non-admin (compliant) case. An
	// instance-admin PAT is effectively god-mode, so the privilege checker treats
	// true as a violation.
	IsAdmin bool
}

// TokenInfo is PAT introspection returned by TokenInfo: the scopes it carries,
// whether it is active, and when it expires. GitLab: GET
// /personal_access_tokens/self.
type TokenInfo struct {
	// Scopes are the token's granted scopes (e.g. ["api"]).
	Scopes []string
	// Active is true when the token is usable (not revoked, not expired).
	Active bool
	// ExpiresAt is the token's expiry; the zero time means it never expires.
	ExpiresAt time.Time
}

// BranchProtection reports a branch's protection state, returned by
// DefaultBranchProtection.
type BranchProtection struct {
	// Protected is false when the branch has no protection rule at all (the
	// driver maps a 404 from the protected-branches endpoint to this).
	Protected bool
	// DevelopersCanPush is true when the branch's push access levels admit the
	// Developer role (access level 30) or lower-but-nonzero. It is load-bearing:
	// a protected default branch that Developers may still push to does not
	// protect main, so a Developer-role bot could push directly.
	DevelopersCanPush bool
	// BotCanPush is true when a push access level names the bot user directly (a
	// per-user allow-to-push grant), which lets the bot push to the protected
	// branch even at Developer role — a false negative the role/DevelopersCanPush
	// checks alone would miss. Group-level push grants to the bot are NOT detected
	// (they need an extra membership call); that gap is documented for manual audit.
	BotCanPush bool
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

// IsKnownMRState reports whether s is one of the MR states this integration
// recognizes. The MR-close watcher records and acts on known states only; an
// unrecognized or empty value is ignored so a transient forge glitch cannot
// poison the watcher's edge baseline (reviewer hardening, PRD #24 Decision Log).
func IsKnownMRState(s string) bool {
	switch s {
	case MRStateOpened, MRStateClosed, MRStateMerged, MRStateLocked:
		return true
	default:
		return false
	}
}

// Pipeline is the latest pipeline uzi caches for a watched ref (PRD #6). Status
// is the raw GitLab pipeline status — one of created|waiting_for_resource|
// preparing|pending|running|success|failed|canceled|skipped|manual|scheduled —
// which the web layer collapses to a handful of badge tones; the cache stores it
// verbatim so uzi never invents a status the forge did not report. CreatedAt /
// UpdatedAt are zero when the forge omitted them.
type Pipeline struct {
	ID        int64
	Ref       string
	SHA       string
	Status    string
	WebURL    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Job is one job of a pipeline (PRD #6). The CI-fix path snapshots a failed
// pipeline's jobs (name/stage/status/url) plus each one's log tail so the run
// stays self-contained. Status is the raw GitLab job status.
type Job struct {
	ID     int64
	Name   string
	Stage  string
	Status string
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
	// UserExists reports whether a user with the given username exists on the
	// forge. It backs the best-effort verification of a user's self-declared
	// human_username (PRD #19 M3): a false — or an error — downgrades a save to
	// "saved with a warning", never a hard failure. A blank username is not a
	// forge lookup; it returns (false, nil).
	UserExists(ctx context.Context, username string) (bool, error)
	// ListIssueLabelEvents returns the issue's resource label events (who added or
	// removed which label, when), oldest first, paginated internally. Autopilot
	// uses it to attribute the autopilot label to the human who added it (PRD #19
	// Decision 3). No caller until the autopilot-trigger milestone.
	ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error)
	// CreateIssueNote posts a comment on an issue and returns it. Autopilot uses
	// it to surface an outcome — no eligible user, a run failure, or the success MR
	// link — back on the forge so a forge-only user is never left waiting (PRD #19
	// Decision 6). No caller until the autopilot trigger/execution milestones.
	CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error)
	// GetMergeRequest returns one merge request by its project-scoped IID. The
	// MR-close watcher (PRD #24) polls this for cards parked in Human Review to
	// detect an opened→closed (reviewer rejected the MR) edge.
	GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error)
	// TokenInfo returns introspection data for the PAT the client authenticates
	// with: its scopes, whether it is active, and its expiry. GitLab: GET
	// /personal_access_tokens/self. Returns ErrTokenIntrospectionUnsupported when
	// the forge version lacks the endpoint (so the caller warns, not blocks).
	TokenInfo(ctx context.Context) (TokenInfo, error)
	// ProjectRole returns the bot's effective (direct or inherited) access level
	// on a project, and whether the bot is a member at all. member is false with
	// a nil error when the bot has no effective membership (a 404 from the
	// members/all lookup). GitLab: GET /projects/:id/members/all/:user_id.
	ProjectRole(ctx context.Context, projectID, forgeUserID int64) (role int, member bool, err error)
	// DefaultBranchProtection reports whether the given branch is protected,
	// whether Developer-level push is allowed on it, and whether the bot user has
	// a direct per-user push grant on it. GitLab: GET
	// /projects/:id/protected_branches/:name (a 404 means unprotected). botUserID
	// is the bot's forge user id, used to flag a per-user allow-to-push entry.
	DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error)
	// LatestPipeline returns the newest branch pipeline for a ref, or
	// ErrNoPipeline when the ref has none (no CI configured, or it never ran).
	// GitLab: GET /projects/:id/pipelines?ref=<ref>&per_page=1 (default
	// order_by=id desc, so the first row is newest).
	LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error)
	// LatestMRPipeline returns the newest pipeline attached to a merge request.
	// This is what catches detached MR pipelines (refs/merge-requests/:iid/head)
	// and merged-results pipelines, which never appear under the source-branch
	// ref — so run-branch status and fix verification key on the run's MR, not its
	// branch ref. Returns ErrNoPipeline when the MR has no pipeline. GitLab: GET
	// /projects/:id/merge_requests/:iid/pipelines (newest first).
	LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error)
	// ListPipelineJobs returns the pipeline's jobs with status/stage/name,
	// paginated internally. GitLab: GET /projects/:id/pipelines/:pipeline_id/jobs.
	// Called only at fix-trigger time (to snapshot failed jobs), never on the poll
	// tick.
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error)
	// JobLogTail returns at most maxBytes from the END of a job's trace (a
	// failure's cause concludes its log). GitLab: GET /projects/:id/jobs/:job_id/
	// trace — the endpoint has no range/tail parameter, so this is a full download
	// truncated client-side; acceptable because it runs only at fix-trigger time,
	// never on the poll tick. The returned tail passes through the PAT redactor.
	JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error)
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
