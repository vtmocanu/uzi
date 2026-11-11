package forge

import (
	"context"
	"time"
)

// Ancestry is the tri-state answer CompareAncestry gives to "is candidate an ancestor
// of, or equal to, head" (issue #1582 M1). Only AncestryAncestor is a positive proof;
// AncestryUnknown is the fail-closed answer for every error, rate limit, oversize or
// unrecognized response, and the caller must never read it as "not an ancestor
// either" (it simply proves nothing).
type Ancestry string

const (
	// AncestryAncestor: the forge explicitly reported candidate reachable from head
	// (or the two are the same commit).
	AncestryAncestor Ancestry = "ancestor"
	// AncestryNotAncestor: the forge explicitly reported candidate NOT reachable from
	// head (GitHub ahead/diverged, GitLab merge base != candidate).
	AncestryNotAncestor Ancestry = "not_ancestor"
	// AncestryUnknown: no conclusive answer. Always the result alongside an error.
	AncestryUnknown Ancestry = "unknown"
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
	// ProtectionUnverified is true when the driver could not authoritatively
	// determine the write role's push/merge rights on a PROTECTED branch — it is
	// meaningful only when Protected is true. It exists for the GitHub D6
	// legacy-branch-protection case (PRD #238): GitHub's admin-gated
	// /branches/{b}/protection endpoint 403s a write-role bot, and legacy branch
	// protection is invisible through the reader-gated rulesets endpoint, so on a
	// legacy-protected repo the bot can read that the branch is Protected but not
	// who may push or merge. There the driver reports WriteRoleCanPush=false and
	// WriteRoleCanMerge=false (NEVER a fabricated true) AND sets this flag, so
	// "undetermined" is never conflated with an authoritatively-verified false.
	//
	// The zero value is false = verified, which is the safe default the five test
	// fakes and the GitLab/Forgejo drivers get for free: they always determine
	// push/merge authoritatively, so they never set it. A consumer must treat
	// `Protected && ProtectionUnverified` as FAIL-CLOSED — the same tier as a
	// detected write-role-can-push violation — because the Can* fields being false
	// here means "could not tell", not "safe". Reading them as safe reopens the R12
	// inversion on exactly the case the flag exists to mark. This is a consumer
	// requirement recorded for PRD #66; adding the field changes no interface
	// method, so it does not break the fakes.
	ProtectionUnverified bool
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

// ReviewComment states classify an MRComment as either an inline diff comment
// (carries Path/Line) or a review-summary / top-level note (no Path/Line). It is
// a small closed vocabulary the snapshot layer uses to tell a review-summary body
// apart from an inline finding; a driver never invents another value.
const (
	// ReviewCommentInline is a comment anchored to a specific diff line — it
	// carries Path and Line, and (on a forge that supports threads) ReplyID /
	// ResolveID anchors.
	ReviewCommentInline = "inline"
	// ReviewCommentSummary is a review-summary body or a top-level MR note: no
	// Path/Line, and typically no resolvable thread.
	ReviewCommentSummary = "summary"
)

// MRComment ID namespaces. A forge draws comment ids from several distinct
// sequences, so an id is ordered only within its namespace; the identity of a
// comment is (IDNamespace, ID). A driver sets exactly one of these per comment.
const (
	// CommentNSIssueComment is a PR/issue-level conversation comment (GitHub issue
	// comments, Forgejo issue comments).
	CommentNSIssueComment = "issue_comment"
	// CommentNSReviewComment is an inline review (diff) comment (GitHub pull
	// review comments, Forgejo pull review comments).
	CommentNSReviewComment = "review_comment"
	// CommentNSReview is a review-summary body, keyed by the review id (GitHub and
	// Forgejo pull reviews).
	CommentNSReview = "review"
	// CommentNSNote is a GitLab note: one notes sequence covers inline and
	// top-level notes alike (discussions only group notes).
	CommentNSNote = "note"
)

// MRComment is one comment on a merge request as ListMergeRequestComments returns
// it oldest-first (PRD #700): a human or third-party review-bot comment, with the
// forge's own system notes filtered out driver-side (mirroring IssueComment's D2/
// D8 contract). The MR-review-rework watcher reasons about each as UNTRUSTED data
// (same class as IssueComment.Body / Issue.Description) and never follows
// instructions embedded in Body. The bot self-filter — dropping uzi's own bot
// notes while KEEPING third-party review bots — is the snapshot layer's job, not
// the driver's, so drivers return every non-system comment.
//
// It carries TWO thread anchors because on GitHub the reply target and the resolve
// target are different ids (Resolved facts): ReplyID is the REST databaseId while
// ResolveID is the GraphQL review-thread node id. On GitLab both are the single
// discussion id; on Forgejo ReplyID is the review-comment id and ResolveID is
// empty (Forgejo cannot resolve — see ErrResolveUnsupported).
type MRComment struct {
	// ID is the forge's numeric comment/note id. It is monotonic only within its
	// IDNamespace: different namespaces draw from distinct sequences, so ids are
	// not comparable across them. The MR-rework ledger keeps one advance-only
	// watermark per namespace (ADR-1199).
	ID int64
	// IDNamespace is the forge-defined id namespace (one of the CommentNS*
	// constants). Comment identity is (IDNamespace, ID); ids are ordered only
	// within one namespace.
	IDNamespace       string
	AuthorForgeUserID int64     // stable forge user id of the comment author
	AuthorUsername    string    // author login, may be empty
	Body              string    // comment body (untrusted)
	CreatedAt         time.Time // when the comment was created
	// Path is the diff file path an inline comment is anchored to; nil for a
	// review-summary body or a top-level MR note.
	Path *string
	// Line is the diff line an inline comment is anchored to; nil for a
	// review-summary body or a top-level MR note.
	Line *int
	// ReplyID is the REPLY anchor ReplyMergeRequestComment keys on: the GitLab
	// discussion id, the GitHub REST comment databaseId (as a string), or the
	// Forgejo review-comment id. Empty when the comment is not a repliable thread.
	ReplyID string
	// ResolveID is the RESOLVE anchor ResolveMergeRequestThread keys on: the
	// GitLab discussion id or the GitHub GraphQL review-thread node id. EMPTY on
	// Forgejo (no resolvable-thread concept) and for non-thread comments.
	ResolveID string
	// HeadSHA is the diff head SHA the comment was written against, for the
	// staleness gate (Decision 6). Empty when the driver cannot supply one (e.g. a
	// top-level note that carries no diff position).
	HeadSHA string
	// ReviewState is one of the ReviewComment* constants: "inline" for a diff-line
	// comment, "summary" for a review-summary body or top-level note.
	ReviewState string
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
//
// Steps and StartedAt/FinishedAt were added for the forge view (PRD #1255): a
// GitHub Actions job exposes per-step detail (WorkflowJob.Steps) and both
// timestamps; GitLab/Forgejo jobs carry no steps (Steps stays nil) but do carry
// timing. A driver fills what its SDK exposes and leaves the rest zero.
type Job struct {
	ID     int64
	Name   string
	Stage  string
	Status string
	WebURL string
	// Steps are the job's ordered steps (GitHub Actions only). Nil on GitLab and
	// Forgejo, whose job APIs expose no step breakdown.
	Steps []Step
	// StartedAt / FinishedAt bound the job's execution; zero when the SDK omits
	// them (a not-yet-started or not-yet-finished job), which stores as NULL.
	StartedAt  time.Time
	FinishedAt time.Time
	// CreatedAt is when the forge created the job (#3225); zero when unreported.
	// StartedAt - CreatedAt includes dependency and manual waits, so it is not
	// runner queue time.
	CreatedAt time.Time
	// Attempt is the forge-reported run attempt the job belongs to (GitHub and
	// Forgejo run_attempt); nil when the forge reports none (GitLab). Never derived.
	Attempt *int64
	// QueuedDuration is the forge-measured runner queue wait (GitLab only); nil when
	// unreported. GitLab's field cannot distinguish absent from zero, so a zero is
	// treated as unknown.
	QueuedDuration *time.Duration
}

// Step is one step of a job (PRD #1255). Only GitHub Actions exposes steps
// (WorkflowJob.Steps → TaskStep); the neutral type carries the raw Status and
// Conclusion verbatim so pipelinestatus.Tone classifies them exactly as the job
// and pipeline statuses. Number is the 1-based step index.
type Step struct {
	Name        string
	Status      string
	Conclusion  string
	Number      int
	StartedAt   time.Time
	CompletedAt time.Time
}

// ReviewDecision is the forge-neutral, closed review-decision enum for an open
// merge request (PRD #1255 D6). GitHub has no REST reviewDecision, GitLab has no
// review-decision concept at all, and Forgejo has neither — so every driver
// DERIVES this value (foldReviewDecision for GitHub/Forgejo, a free-tier
// approximation for GitLab), never trusts a single field. The four values are the
// only ones a driver may produce; an unrecognized state folds to ReviewNone.
type ReviewDecision string

const (
	// ReviewChangesRequested means a reviewer's latest decision asks for changes.
	// On GitLab it ALSO covers an unresolved blocking discussion (the closest
	// free-tier signal; see D6), so an MR with one open thread bands as NEEDS YOU
	// by design, not by bug.
	ReviewChangesRequested ReviewDecision = "changes_requested"
	// ReviewApproved means at least one reviewer approved and none of the latest
	// per-reviewer decisions requests changes.
	ReviewApproved ReviewDecision = "approved"
	// ReviewRequired means a review is requested but none has landed a decision.
	ReviewRequired ReviewDecision = "review_required"
	// ReviewNone means no review is requested and none has landed a decision.
	ReviewNone ReviewDecision = "none"
)

// Review is one per-reviewer review on an open merge/pull request as the forge-view
// DETAIL route renders it (PRD #1255 D6): the reviewer's login, the review's RAW
// forge state (GitHub: APPROVED/CHANGES_REQUESTED/COMMENTED/DISMISSED/PENDING;
// Forgejo: APPROVED/REQUEST_CHANGES/COMMENT/…), and when it was submitted. It is the
// same review stream the drivers fold into ReviewDecision, returned verbatim so the
// detail view can list each reviewer's latest state; the fold (foldReviewDecision) is
// the classification, this is the display list. GitLab has no per-reviewer review
// stream (approvals are a separate Premium concept), so its driver returns none.
// Author and State are UNTRUSTED forge text.
type Review struct {
	Author      string
	State       string
	SubmittedAt time.Time
}

// MergeRequestSummary is one merge/pull request's full neutral summary as
// GetMergeRequestSummary returns it per iid (PRD #1255 D5/M2b): the fields the pulls
// screen bands, sorts and renders. Conflicts is a pointer because "unknown" is a
// real third state — GitHub omits mergeability on the list and computes it lazily,
// so a driver reports nil rather than guessing false. Every string field is
// forge-authored UNTRUSTED text (same class as Issue.Title / MRComment.Body).
type MergeRequestSummary struct {
	IID            int64
	Title          string
	Author         string
	SourceBranch   string
	TargetBranch   string
	HeadSHA        string
	Draft          bool
	Conflicts      *bool
	ReviewDecision ReviewDecision
	// State is one of the MRState* constants (opened/closed/merged/locked),
	// populated by GetMergeRequestSummary so GetPull can 404 a non-open PR. It is
	// deliberately NOT carried on the wire DTOs — the forge view is open-only.
	State     string
	WebURL    string
	Additions int
	Deletions int
	Commits   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MergeRequestRef is the cheap list-row identity ListMergeRequestRefs returns for
// each open merge/pull request (PRD #1255 M2b): just what the forge yields in ONE
// list call, enough for the caller to address each PR by iid and enrich it with a
// per-iid GetMergeRequestSummary. Splitting the old bundled list into refs + per-iid
// summaries is what lets the memo (M2b part 2) make each PR's enrichment individually
// addressable, and what lets GetPull find ANY open PR by iid rather than scanning the
// most-recently-updated page. IID and HeadSHA are forge-authored identifiers,
// UpdatedAt the activity timestamp the list is ordered by.
type MergeRequestRef struct {
	IID       int64
	HeadSHA   string
	UpdatedAt time.Time
}

// Check is one status/check for a commit sha (PRD #1255 D5), merging the two
// GitHub surfaces (check-runs and commit statuses) and their GitLab/Forgejo
// analogues into one neutral shape, deduplicated by Name so a bot reporting
// through either surface appears once. Status is the run phase — one of
// "queued", "in_progress" or "completed" — and Conclusion, meaningful only once
// completed, is one of "success", "failure", "neutral", "cancelled", "skipped",
// "timed_out", "action_required" or "" (empty while still running). A consumer
// collapses the pair the same way the driver collapses a job status (Conclusion
// once completed, else Status) and classifies it with pipelinestatus.Tone. Source
// is the reporting app's slug (a check-run's App.Slug) or the literal "status" for
// a commit-status surface. Name and Description are UNTRUSTED forge text.
type Check struct {
	Name        string
	Status      string
	Conclusion  string
	Description string
	WebURL      string
	StartedAt   time.Time
	CompletedAt time.Time
	Source      string
}

// WorkflowRun is one CI run for the forge view's `ci` screen (PRD #1255 D5): a
// GitHub Actions workflow run, a GitLab pipeline, or a Forgejo Actions run. Status
// carries the raw run phase and Conclusion the terminal outcome where the forge
// splits them (GitHub); GitLab/Forgejo fold everything into Status and leave
// Conclusion empty, so a consumer collapses the pair exactly as for Check.
// JobsDone/JobsTotal are best-effort and filled only for in-flight rows by the
// route layer (no forge's runs list includes jobs), so a driver leaves them zero.
// Name, Title, Actor, Branch are UNTRUSTED forge text.
type WorkflowRun struct {
	// RunAttempt is nil when the forge does not report an attempt.
	RunAttempt       *int64
	RepositoryID     int64
	HeadRepositoryID int64
	// WorkflowKey is the provider identity: GitHub workflow ID, Forgejo path, GitLab name/ref.
	WorkflowKey string
	ID          int64
	Name        string
	Number      int64
	Event       string
	Branch      string
	SHA         string
	Status      string
	Conclusion  string
	Title       string
	Actor       string
	WebURL      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   time.Time
	JobsDone    int
	JobsTotal   int
}

// ListMergeRequestsOptions filters ListMergeRequestRefs. State's zero value means
// "opened" (the only state the pulls screen shows); Limit == 0 means the driver's
// default page (no explicit cap), a positive value stops once that many rows are
// collected.
type ListMergeRequestsOptions struct {
	State string
	Limit int
}

// ListWorkflowRunsOptions filters ListWorkflowRuns. Limit == 0 means the driver's
// default page; Branch/Event/Status are optional server-side filters passed
// through to the forge (an unset field is not filtered on).
type ListWorkflowRunsOptions struct {
	Limit  int
	Branch string
	Event  string
	Status string
}

// CodeHost is the code-host half of the driver contract: repository and identity
// reads, the guardrail reads (token, role, branch protection), merge requests, CI
// pipelines and checks, artifacts and refs. It holds every Forge method that is
// not on IssueTracker.
type CodeHost interface {
	// RepositoryAuthorEligibility assesses a stable user ID independently of the
	// authenticated bot's ProjectRole. Missing/deleted identities and incomplete
	// evidence return AuthorUnknown with an error.
	RepositoryAuthorEligibility(ctx context.Context, projectID, authorForgeUserID int64) (AuthorEligibility, error)
	// VerifyToken confirms the PAT works and returns the bot identity.
	VerifyToken(ctx context.Context) (BotIdentity, error)
	// ListProjects returns every project the bot has at least Developer access
	// to (membership=true).
	ListProjects(ctx context.Context) ([]Project, error)
	// GetProject freshly reads bounded metadata by positive numeric ID, without
	// inventory enumeration or cached identity. It requires complete, coherent
	// identity and WebURL; DefaultBranch may be empty. Any failure returns zero
	// Project and invalidates the requested ID's cached slug.
	GetProject(ctx context.Context, projectID int64) (Project, error)
	// UserExists reports whether a user with the given username exists on the
	// forge. It backs the best-effort verification of a user's self-declared
	// human_username (PRD #19 M3): a false — or an error — downgrades a save to
	// "saved with a warning", never a hard failure. A blank username is not a
	// forge lookup; it returns (false, nil).
	UserExists(ctx context.Context, username string) (bool, error)
	// GetMergeRequest returns one merge request by its project-scoped IID. The
	// MR-close watcher (PRD #24) polls this for cards parked in Human Review to
	// detect an opened→closed (reviewer rejected the MR) edge.
	GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error)
	// ListMergeRequestComments returns an MR's HUMAN and third-party review-bot
	// comments oldest-first (PRD #700), paginated internally and bounded by the
	// shared forge sanity ceilings (maxForgeItems/maxForgePages). Each driver drops
	// the forge's own system/timeline notes (D2) and normalizes ordering to
	// oldest-first (D8), and populates the reply/resolve anchors, HeadSHA, and
	// Path/Line wherever the SDK exposes them. It does NOT filter bot authors — the
	// snapshot layer does that against the connection's own bot id, so a
	// third-party review bot (CodeRabbit) stays readable. Bodies are untrusted free
	// text. GitHub stitches three REST sources with a GraphQL reviewThreads read to
	// supply both anchors (Resolved facts). Every comment carries its IDNamespace
	// (CommentNS*): ids are ordered only within one namespace.
	ListMergeRequestComments(ctx context.Context, projectID, mrIID int64) ([]MRComment, error)
	// ReplyMergeRequestComment posts a reply in the thread keyed on replyID (an
	// MRComment.ReplyID: the GitLab discussion id, the GitHub REST databaseId, or
	// the Forgejo review-comment id). The MR-review-rework worker uses it to answer
	// each finding ("done in <sha>" / "skipped because X"). No caller until PRD
	// #700's plumbing.
	ReplyMergeRequestComment(ctx context.Context, projectID, mrIID int64, replyID, body string) error
	// ResolveMergeRequestThread resolves the review thread keyed on resolveID (an
	// MRComment.ResolveID: the GitLab discussion id or the GitHub GraphQL thread
	// node id). Returns ErrResolveUnsupported on Forgejo, whose forge has no
	// resolvable-thread concept — the worker swallows that sentinel and keeps the
	// reply (reply-only is the documented Forgejo contract). No caller until PRD
	// #700's plumbing.
	ResolveMergeRequestThread(ctx context.Context, projectID, mrIID int64, resolveID string) error
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
	// ListPipelineJobAttempts returns the jobs of EVERY attempt of a run, not just
	// the latest (#3225, the CI scout's history read): GitHub filter=all, GitLab
	// include_retried=true. Timestamps are only what the forge reported: unlike
	// ListPipelineJobs, GitLab's FinishedAt is never derived from StartedAt plus
	// Duration. Same budget and pagination path as ListPipelineJobs. Returns
	// ErrJobAttemptsUnsupported where complete history is unavailable (Forgejo).
	ListPipelineJobAttempts(ctx context.Context, projectID, pipelineID int64) ([]Job, error)
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
	// ListMergeRequestRefs returns the project's open merge requests as cheap
	// list-row refs (PRD #1255 M2b) in ONE list call, newest activity first,
	// paginated internally — just IID/HeadSHA/UpdatedAt per row, with NO per-PR
	// enrichment (that is GetMergeRequestSummary's job). opts.State defaults to
	// "opened" (the only state the pulls screen shows); opts.Limit optionally caps
	// the rows. Pagination is bounded by the driver's caps; errors are PAT-redacted,
	// and on GitHub a rate-limit surfaces as *RateLimitError (GitLab/Forgejo return a
	// plain error; see RateLimitError).
	ListMergeRequestRefs(ctx context.Context, projectID int64, opts ListMergeRequestsOptions) ([]MergeRequestRef, error)
	// GetMergeRequestSummary returns ONE merge/pull request's full neutral summary by
	// iid (PRD #1255 M2b): IID, Title, Author, branches, HeadSHA, Draft, the Conflicts
	// tri-state, ReviewDecision, State, WebURL and (where the forge exposes them)
	// Additions/Deletions/Commits and timestamps. Each driver derives ReviewDecision
	// (no forge exposes it on the row) and reads whatever per-MR detail the summary
	// needs (GitHub: PullRequests.Get + ListReviews; GitLab: GetMergeRequest +
	// approvals; Forgejo: GetPullRequest + the raw reviews fold). Returns
	// ErrMergeRequestNotFound (errors.Is-matchable) when the forge 404s the iid. Every
	// string field is untrusted forge text; errors are PAT-redacted, and on GitHub a
	// rate-limit surfaces as *RateLimitError (GitLab/Forgejo return a plain error; see
	// RateLimitError).
	GetMergeRequestSummary(ctx context.Context, projectID, iid int64) (MergeRequestSummary, error)
	// ListChecks returns every status/check for a commit sha (PRD #1255 D5), merging
	// the forge's check and commit-status surfaces and DEDUPLICATING by Name so a bot
	// reporting through either appears once. GitHub: ListCheckRunsForRef +
	// GetCombinedStatus. GitLab: the sha's newest pipeline's jobs + GetCommitStatuses.
	// Forgejo: the Actions jobs of the runs on that sha + ListStatuses. Paginated
	// internally; errors are PAT-redacted.
	ListChecks(ctx context.Context, projectID int64, sha string) ([]Check, error)
	// ListWorkflowRuns returns the project's CI runs newest-first for the `ci` screen
	// (PRD #1255 D5): GitHub workflow runs, GitLab pipelines, or Forgejo Actions runs.
	// One page (opts.Limit caps it, bounded by the driver's max page size);
	// Branch/Event/Status are optional server-side filters. Where the forge version
	// lacks the endpoint the driver returns an error wrapping ErrForgeVersionUnsupported
	// (the honest degrade path). Errors are PAT-redacted; on GitHub a rate-limit
	// surfaces as *RateLimitError (GitLab/Forgejo return a plain error; see RateLimitError).
	ListWorkflowRuns(ctx context.Context, projectID int64, opts ListWorkflowRunsOptions) ([]WorkflowRun, error)
	// GetWorkflowRun returns one CI run's header (the neutral WorkflowRun) by its
	// forge-native run/pipeline id, for the `ci` drill-in. ErrForgeVersionUnsupported
	// on a forge whose Actions/pipelines API is absent (mirrors ListWorkflowRuns).
	GetWorkflowRun(ctx context.Context, projectID, runID int64) (WorkflowRun, error)

	// ListRunArtifacts returns bounded metadata for this exact run, without URL authority.
	ListRunArtifacts(ctx context.Context, projectID, runID int64) ([]Artifact, error)
	// DownloadArtifact fetches an ID-addressed archive, enforcing maxBytes on actual bytes.
	DownloadArtifact(ctx context.Context, projectID, artifactID, maxBytes int64) ([]byte, error)
	// ListMergeRequestReviews returns an open MR's per-reviewer reviews for the
	// forge-view detail route (PRD #1255 D6), oldest-first — the same review stream the
	// driver folds into ReviewDecision, returned verbatim (State is the raw forge value)
	// so the PR view can render each reviewer's latest state. GitHub reads
	// PullRequests.ListReviews; Forgejo reads /pulls/{iid}/reviews; GitLab returns an
	// empty slice (no per-reviewer review stream on the free tier — its decision is the
	// BlockingDiscussionsResolved / approvals approximation, not a review list). Every
	// string field is untrusted forge text; errors are PAT-redacted.
	ListMergeRequestReviews(ctx context.Context, projectID, mrIID int64) ([]Review, error)
	// ProjectCIConfigPath returns the project's configured CI config path (GitLab
	// ci_config_path); empty string means the driver's default (.gitlab-ci.yml). It
	// carries no secret material — a project's ci_config_path is a repo-relative
	// glob, not a token — and any driver error is PAT-redacted like every other
	// method. Used by the PRD #71 auto-fix guard to know which paths a fix may touch.
	ProjectCIConfigPath(ctx context.Context, projectID int64) (string, error)
	// BranchHead returns the 40-hex commit id the named branch currently points at
	// (issue #1582 M1). GitHub: GET /repos/{o}/{r}/branches/{branch} (commit.sha);
	// GitLab: GET /projects/:id/repository/branches/:branch (commit.id); Forgejo: GET
	// /repos/{o}/{r}/branches/{branch} (commit.id). Every driver sends it through a
	// redirect-refusing client, so ANY 3xx (a GitHub rename 301 included) is an error,
	// never ErrRefNotFound and never another host's answer; GitHub and Forgejo also
	// require the response to name the requested branch. A 404 returns ErrRefNotFound; a
	// head that is not a 40-char lowercase hex id is an error. Errors are PAT-redacted.
	BranchHead(ctx context.Context, projectID int64, branch string) (string, error)
	// RefHead returns the 40-hex commit id a FULL ref name (for example
	// refs/uzi-checkpoints/agent/issue-7) currently points at (issue #1751 M2), the head a
	// live predecessor settle proves against when the successor published to its forge
	// checkpoint ref rather than a branch. The ref must start with "refs/" and be a
	// well-formed git ref name (no "..", "//", "@{", control character, space or any of
	// ~ ^ : ? * [ \); anything else is an error with no request. GitHub: GET
	// /repos/{o}/{r}/git/ref/{ref without "refs/"} (object.sha of a commit object, the
	// echoed `ref` must equal the requested ref); GitLab: GET
	// /projects/:id/repository/commits/{url-escaped ref} (id); Forgejo: GET
	// /repos/{o}/{r}/git/refs/{ref without "refs/"} (the entry whose `ref` equals the
	// requested ref exactly, object.sha of a commit object). Every driver refuses redirects
	// like BranchHead. A 404 returns ErrRefNotFound; a head that is not a 40-char lowercase
	// hex id, or an answer naming another ref, is an error. Errors are PAT-redacted.
	RefHead(ctx context.Context, projectID int64, ref string) (string, error)
	// CompareAncestry answers "is candidate an ancestor of, or equal to, head" using the
	// forge's own compare API (issue #1582 M1). Both arguments must be 40-char
	// lowercase hex commit ids (never ref names); anything else is AncestryUnknown with
	// an error. head == candidate is AncestryAncestor WITHOUT a request. Every error,
	// rate limit (a 429, or GitHub's rate-limit 403), oversize body, or unrecognized
	// response is AncestryUnknown; only an explicit, recognized positive answer is
	// AncestryAncestor. Every driver's compare requests refuse redirects (a 3xx is
	// AncestryUnknown with an error). Errors are PAT-redacted.
	CompareAncestry(ctx context.Context, projectID int64, head, candidate string) (Ancestry, error)
}
