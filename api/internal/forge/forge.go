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

// ErrForgeVersionUnsupported is wrapped by the error VerifyToken returns when the
// forge server is older than the driver's minimum supported version (Forgejo <
// 16.0.0, D4 — the CI-fix loop's job-logs route first ships in 16.0.0). The
// wrapping error names the reported and required versions so it stays actionable
// when surfaced verbatim at connect. A re-run of VerifyToken on the periodic
// privilege sweep re-fires the gate automatically; today a downgrade surfaces
// there as the sweep's generic StatusError report (fail-safe — flagged, not
// silently OK). This sentinel exists so a privcheck consumer can errors.Is it and
// give a downgrade its own distinct finding — that consumer is not wired yet. It
// carries no secret material. The version gate is a FEATURE gate, never a security
// control (D4 L2): GET /version is public, unauthenticated and self-reported, so
// nothing security-relevant may hang on it. GitLab has no version floor and never
// returns this; it lives here because the sentinel is part of the driver contract,
// not one driver's implementation.
var ErrForgeVersionUnsupported = errors.New("forge: server version is older than this driver's minimum")

// Type identifies a forge driver. It maps 1:1 to the forge_connections.forge_type
// column, which is CHECK-constrained to the same set.
type Type string

const (
	// TypeGitLab is the GitLab REST driver.
	TypeGitLab Type = "gitlab"
	// TypeForgejo is the Forgejo REST driver.
	TypeForgejo Type = "forgejo"
)

// Role is a bot's effective permission level on a project, in forge-neutral
// terms. It deliberately carries no number: GitLab's access levels (30 =
// Developer) are a GitLab implementation detail, and Forgejo has no numeric
// levels at all — its API returns exactly this vocabulary
// (none|read|write|admin|owner). A driver that mapped "write" onto 30 to satisfy
// a GitLab-shaped contract would be leaking one forge's model through the seam
// this package exists to be.
type Role string

const (
	// RoleNone is no access.
	RoleNone Role = "none"
	// RoleRead can read but not write.
	RoleRead Role = "read"
	// RoleWrite can push branches and open merge requests. This is the exact role
	// uzi's bot must hold: enough to do the work, not enough to be trusted with
	// main.
	RoleWrite Role = "write"
	// RoleAdmin administers the project (settings, protection rules).
	RoleAdmin Role = "admin"
	// RoleOwner owns the project.
	RoleOwner Role = "owner"
)

// roleRank orders the roles least- to most-privileged. Unlike a forge's access
// level it is not a wire value and never leaves this package — AtLeast is the
// only way to read it, so no caller can reintroduce an "access level 30"
// comparison against a neutral type.
var roleRank = map[Role]int{RoleNone: 0, RoleRead: 1, RoleWrite: 2, RoleAdmin: 3, RoleOwner: 4}

// AtLeast reports whether r is min or more privileged. An unrecognized role
// ranks as RoleNone, so an unknown value from a forge never reads as privileged.
func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] }

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
//
// Protected, WriteRoleCanPush, and WriteRoleCanMerge are evaluated on EVERY path,
// including when Protected is false. That is a deliberate invariant, not an
// incidental one: an unprotected branch is the case where a write-role bot CAN
// push and CAN merge, so a driver that returned the zero value there would report
// the most dangerous branch in the repo as "cannot push, cannot merge". A
// consumer that reads those three without checking Protected first would invert
// its guardrail on exactly the worst case — so drivers report what is true of the
// branch, never what was convenient to skip (the GitLab 404 arm hardcodes these
// two CanPush/CanMerge fields true; the Forgejo unprotected early-return returns
// them true for the supported write-bot config — its user_can_* are bot-scoped, so
// a non-write bot, already a ProjectRole finding, reads false there, but the
// Protected-first guard fires "not protected" regardless). See
// TestDefaultBranchProtectionUnprotectedIsNotSafe and its Forgejo twin, which fail
// if either driver reverts to the zero value there.
//
// BotCanPush and BotCanMerge are DIFFERENT and the invariant above does NOT extend
// to them: they are per-user-grant flags — "a push/merge access level names the
// bot user directly" — meaningful ONLY when Protected is true. On an unprotected
// branch no such grant exists, so they are legitimately false there. That makes
// them a trap read on their own: a consumer writing `if BotCanPush || BotCanMerge`
// without a Protected-first guard reads false,false on unprotected main and
// concludes safe — the same R12 inversion. The GitLab driver populates them from a
// protected branch's access levels; the Forgejo driver never sets them (its branch
// endpoint folds the bot's own rights into user_can_push/user_can_merge, which map
// onto WriteRoleCan*), so on Forgejo they are always false. Consumers must gate on
// Protected first — M3's evaluateRepo does.
type BranchProtection struct {
	// Protected is false when the branch has no protection rule at all (the
	// driver maps a 404 from the protected-branches endpoint to this). It is a
	// finding in its own right, not a precondition for reading the fields below.
	Protected bool
	// WriteRoleCanPush is true when any principal at the write role may push to
	// the branch — either because a push access level admits it, or because the
	// branch is unprotected and so admits every writer. It is load-bearing: a
	// default branch a write-role bot may push to does not protect main.
	WriteRoleCanPush bool
	// WriteRoleCanMerge is true when any principal at the write role may merge
	// into the branch. uzi's "an agent can only ever open a merge request"
	// guarantee needs this as much as it needs WriteRoleCanPush: a bot that can
	// merge its own MR does not need push to land code on main.
	WriteRoleCanMerge bool
	// BotCanPush is true when a push access level names the bot user directly (a
	// per-user allow-to-push grant), which lets the bot push to the protected
	// branch even at write role — a false negative the role/WriteRoleCanPush
	// checks alone would miss. Group-level push grants to the bot are NOT detected
	// (they need an extra membership call); that gap is documented for manual audit.
	// GitLab only: the Forgejo driver never sets this (see the type doc). Read it
	// behind a Protected-first guard, never on its own.
	BotCanPush bool
	// BotCanMerge is the merge counterpart of BotCanPush: a merge access level
	// naming the bot user directly. Same group-level gap; likewise GitLab-only and
	// meaningful only when Protected is true.
	BotCanMerge bool
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

// IssueState is the neutral issue-state vocabulary, matching the values the
// drivers already normalize their RESPONSES to and the values the issue cache
// stores in issues.state.
//
// The neutral spelling is GitLab's, not Forgejo's, for the historical reason that
// GitLab was the only driver when the cache's state='opened' filter was written.
// Forgejo says "open"; forgejoIssueState maps that inbound. See StateAll's note on
// why the OUTBOUND direction needs its own translation.
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
	// UpdateIssueDescription replaces an issue's description wholesale (PRD #72 M5).
	// It is a read-modify-write from the CALLER's point of view — the caller reads
	// via GetIssue, rewrites, and writes back — because no forge here offers a patch
	// primitive. A driver may additionally need its own internal read to satisfy its
	// forge's API shape; that is the driver's business, not the interface's.
	UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error
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
	// ProjectRole returns the bot's effective (direct or inherited) role on a
	// project, and whether the bot is a member at all. member is false with a nil
	// error when the bot has no effective membership (a 404 from the members/all
	// lookup). GitLab: GET /projects/:id/members/all/:user_id, whose numeric
	// access level the driver maps onto Role.
	ProjectRole(ctx context.Context, projectID, forgeUserID int64) (role Role, member bool, err error)
	// DefaultBranchProtection reports whether the given branch is protected, and
	// whether the write role or the bot specifically may push to or merge into it.
	// GitLab: GET /projects/:id/protected_branches/:name (a 404 means
	// unprotected). botUserID is the bot's forge user id, used to flag a per-user
	// grant. Every returned field is evaluated on every path — see
	// BranchProtection.
	DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error)
	// LatestPipeline returns the newest branch pipeline for a ref, or
	// ErrNoPipeline when the ref has none (no CI configured, or it never ran).
	// GitLab: GET /projects/:id/pipelines?ref=<ref>&per_page=1 (default
	// order_by=id desc, so the first row is newest).
	LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error)
	// LatestMRPipeline returns the newest (max-by-id) pipeline attached to a merge
	// request. This is what catches detached MR pipelines (refs/merge-requests/:iid/
	// head) and merged-results pipelines, which never appear under the source-branch
	// ref — so run-branch status and fix verification key on the run's MR, not its
	// branch ref. Returns ErrNoPipeline when the MR has no pipeline. GitLab: GET
	// /projects/:id/merge_requests/:iid/pipelines — note this endpoint groups
	// merge_request_event pipelines first (then id-desc within a group), NOT a plain
	// id-desc, so the driver picks the max id rather than trusting the first row.
	LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error)
	// ListPipelineJobs returns the pipeline's jobs with status/stage/name,
	// paginated internally. GitLab: GET /projects/:id/pipelines/:pipeline_id/jobs.
	// Called only at fix-trigger time (to snapshot failed jobs), never on the poll
	// tick.
	ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error)
	// JobLogTail returns at most maxBytes from the END of a job's trace (a
	// failure's cause concludes its log); maxBytes <= 0 returns the whole trace.
	// GitLab: GET /projects/:id/jobs/:job_id/trace — the endpoint has no range/tail
	// parameter, so this is a full download truncated client-side, capped by a
	// fail-closed download ceiling; acceptable because it runs only at fix-trigger
	// time, never on the poll tick. The returned tail passes through the driver's
	// PAT redactor (the snapshot path applies a second, pattern-based scrub). NOTE:
	// the returned tail is NOT itself a secret-safe log scrubber for arbitrary
	// third-party tokens — see the snapshot scrubber in the ci-fix handler.
	JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error)
}

// New constructs a driver for the given forge type. baseURL must already be
// allowlist-validated by the caller (the SSRF guard lives in config, not here).
// timeout bounds every HTTP call the driver makes.
func New(t Type, baseURL, token string, timeout time.Duration) (Forge, error) {
	switch t {
	case TypeGitLab:
		return newGitLab(baseURL, token, timeout)
	case TypeForgejo:
		return newForgejo(baseURL, token, timeout)
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
