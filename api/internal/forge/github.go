package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v90/github"
)

// githubPerPage is the pagination page size for every GitHub list call. 100 is
// the API maximum, so it minimizes round-trips; the driver still loops on
// resp.NextPage until 0 (go-github does NOT auto-paginate), the same shape the
// Forgejo driver uses on its own NextPage.
const githubPerPage = 100

// githubDefaultLabelColor is used when EnsureLabels / CreateIssue is handed a
// label with no color. GitHub's create-label API requires a 6-hex color (no
// leading '#'), so the driver supplies a neutral default rather than fail a
// label create — the same accommodation the Forgejo driver makes.
const githubDefaultLabelColor = "ededed"

// github is the GitHub REST driver (D3/D5): github.com only, classic PAT,
// built on github.com/google/go-github/v90. Unlike the gitea SDK the Forgejo
// driver uses, go-github takes ctx PER METHOD, so a SINGLE long-lived
// *github.Client is safe to reuse across calls — no per-call client rebuild
// (D5's one ergonomic win over Forgejo). There is no version gate (D4):
// github.com is always current, so VerifyToken never version-checks.
//
// The uzi Forge interface addresses repos by a numeric projectID, but every
// go-github method takes an owner/repo pair, so the driver resolves id → slug
// via Repositories.GetByID and caches it for the driver's (short) lifetime — a
// driver is rebuilt per ForgeForConnection call, so a rename cannot go stale
// across operations. This mirrors the Forgejo driver's repoSlugFor exactly.
type github struct {
	token  string
	client *gh.Client
	redact redactor

	// logClient is the PURPOSE-BUILT second-hop client for JobLogTail's blob GET
	// (D5/H5/R5): it attaches NO Authorization header (it is a plain http.Client,
	// not go-github's auth-transport client) and refuses to follow any further
	// redirect (CheckRedirect → http.ErrUseLastResponse). See fetchJobLog.
	logClient *http.Client
	// allowInsecureLogHost relaxes fetchJobLog's https-only + private-host SSRF
	// guard. It is false in production and set true ONLY by the test harness, whose
	// blob "host" is an httptest server on loopback http. Never settable through
	// New — the field is unexported and the constructor leaves it false.
	allowInsecureLogHost bool

	mu    sync.RWMutex
	slugs map[int64]repoSlug
}

// newGitHub builds a GitHub driver against baseURL using token, with a bounded
// per-call HTTP timeout. baseURL is assumed allowlist-checked by the caller (the
// SSRF guard lives in config). No network call happens here.
//
// Base-URL handling (D3, reconciled with the no-GHES scope): production connects
// with the WEB host https://github.com; the driver targets the default API host
// api.github.com, which go-github uses out of the box — so an empty base or
// github.com means "use go-github's default". Any OTHER base (the httptest URL a
// test/fake points us at) is treated as the API base via WithEnterpriseURLs. This
// is NOT GHES support (out of scope, not validated or advertised): it is the same
// base-URL injection the gitlab/forgejo test harnesses use to reach a local server.
func newGitHub(baseURL, token string, timeout time.Duration) (*github, error) {
	opts := []gh.ClientOptionsFunc{
		gh.WithHTTPClient(timeoutClient(timeout)),
		gh.WithAuthToken(token),
	}
	if b := strings.TrimRight(strings.TrimSpace(baseURL), "/"); b != "" && !isDefaultGitHubBase(b) {
		// WithEnterpriseURLs appends /api/v3/ to a non-api. host, so the test harness
		// serves its routes under /api/v3 (the mockGitHub mux mounts there).
		opts = append(opts, gh.WithEnterpriseURLs(b, b))
	}
	redact := newRedactor(token)
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, redact.error(fmt.Errorf("github: new client: %w", err))
	}
	// The second-hop log client shares the per-call timeout but attaches no auth and
	// refuses every redirect (H5 guards (a) and (b)). Guard (c) — https + host
	// validation — lives in fetchJobLog so it can be relaxed for the test harness.
	logClient := &http.Client{
		Timeout:       timeoutClient(timeout).Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &github{
		token:     token,
		client:    c,
		redact:    redact,
		logClient: logClient,
		slugs:     map[int64]repoSlug{},
	}, nil
}

// isDefaultGitHubBase reports whether base is the github.com web host (or empty
// or api.github.com), i.e. the cases where go-github's default API base is correct
// and no override is wanted.
func isDefaultGitHubBase(base string) bool {
	switch base {
	case "", "https://github.com", "http://github.com", "https://api.github.com":
		return true
	}
	return false
}

// wrapErr wraps a go-github error with op context, recognizes the two rate-limit
// error shapes so a 403-because-rate-limited is not misread as a permission
// failure (R7), and routes the whole thing through the PAT redactor so no error
// this package surfaces can carry the token. errors.As runs on the RAW error,
// before redaction severs the Unwrap chain. A nil error passes through as nil.
func (g *github) wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var rle *gh.RateLimitError
	var arle *gh.AbuseRateLimitError
	switch {
	case errors.As(err, &rle):
		return g.redact.error(fmt.Errorf("github: %s: rate limited by github: %w", op, err))
	case errors.As(err, &arle):
		return g.redact.error(fmt.Errorf("github: %s: secondary (abuse) rate limited by github: %w", op, err))
	default:
		return g.redact.error(fmt.Errorf("github: %s: %w", op, err))
	}
}

// repoSlugFor resolves a numeric projectID to its owner/repo pair, caching the
// result for the driver's lifetime. Repositories.GetByID always returns the
// CURRENT path, so a cached slug can only go stale within a single short-lived
// driver — acceptable, and a stale slug surfaces as a redacted 404.
func (g *github) repoSlugFor(ctx context.Context, projectID int64) (repoSlug, error) {
	g.mu.RLock()
	s, ok := g.slugs[projectID]
	g.mu.RUnlock()
	if ok {
		return s, nil
	}
	r, _, err := g.client.Repositories.GetByID(ctx, projectID)
	if err != nil {
		return repoSlug{}, g.wrapErr(fmt.Sprintf("resolve repo %d", projectID), err)
	}
	owner := r.GetOwner().GetLogin()
	if owner == "" || r.GetName() == "" {
		return repoSlug{}, fmt.Errorf("github: resolve repo %d: incomplete repository payload", projectID)
	}
	s = repoSlug{owner: owner, repo: r.GetName()}
	g.mu.Lock()
	g.slugs[projectID] = s
	g.mu.Unlock()
	return s, nil
}

func (g *github) VerifyToken(ctx context.Context) (BotIdentity, error) {
	// Enforce classic-only UP FRONT (D3/H4). A fine-grained token (github_pat_
	// prefix) cannot be introspected — no X-OAuth-Scopes header, a buggy expiry
	// header (go-github #3708) — so accepting it would silently save a token whose
	// scopes uzi can never check, defeating the over-privilege guard for exactly the
	// token type whose breadth is unknown. Refuse it here (fail-safe) rather than
	// fall through to TokenInfo's introspection-unsupported WARNING path, which is
	// the right disposition for GitLab's known-coarse token but wrong for an
	// un-introspectable one. The message carries no secret.
	if strings.HasPrefix(g.token, "github_pat_") {
		return BotIdentity{}, errors.New("github: fine-grained tokens are not supported yet; use a classic PAT (ghp_…)")
	}
	// No version gate (D4). GET /user (empty user = the authenticated user).
	u, _, err := g.client.Users.Get(ctx, "")
	if err != nil {
		return BotIdentity{}, g.wrapErr("verify token", err)
	}
	// site_admin==true is an instance-admin (god-mode) PAT; the privilege checker
	// treats it as a violation exactly as GitLab/Forgejo instance-admin.
	return BotIdentity{ForgeUserID: u.GetID(), Username: u.GetLogin(), IsAdmin: u.GetSiteAdmin()}, nil
}

func (g *github) ListProjects(ctx context.Context) ([]Project, error) {
	opt := &gh.RepositoryListByAuthenticatedUserOptions{
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	}
	var out []Project
	page := 0
	for {
		page++
		repos, resp, err := g.client.Repositories.ListByAuthenticatedUser(ctx, opt)
		if err != nil {
			return nil, g.wrapErr("list projects", err)
		}
		for _, r := range repos {
			// GitHub's list has no min-permission filter, so a read-only repo comes
			// back and must be dropped client-side: the picker only offers repos the
			// bot can actually push to (D-item 2). GetPermissions/GetPush are nil-safe.
			if r == nil || !r.GetPermissions().GetPush() {
				continue
			}
			out = append(out, Project{
				ForgeProjectID:    r.GetID(),
				PathWithNamespace: r.GetFullName(),
				WebURL:            r.GetHTMLURL(),
				DefaultBranch:     r.GetDefaultBranch(),
			})
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list projects", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list projects", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *github) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	labels, err := g.listRepoLabels(ctx, slug)
	if err != nil {
		return nil, err
	}
	out := make([]Label, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		out = append(out, Label{Name: l.Name, Color: l.Color})
	}
	return out, nil
}

// listRepoLabels returns every label on a repo, paginated internally. GitHub
// (unlike Forgejo) keys label writes by NAME, so the driver needs no name→id
// resolution — this backs ListLabels and EnsureLabels' missing-set computation.
func (g *github) listRepoLabels(ctx context.Context, slug repoSlug) ([]*gh.Label, error) {
	opt := &gh.ListOptions{PerPage: githubPerPage}
	var out []*gh.Label
	page := 0
	for {
		page++
		labels, resp, err := g.client.Issues.ListLabels(ctx, slug.owner, slug.repo, opt)
		if err != nil {
			return nil, g.wrapErr("list labels", err)
		}
		out = append(out, labels...)
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list labels", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list labels", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *github) EnsureLabels(ctx context.Context, projectID int64, labels []Label) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	existing, err := g.listRepoLabels(ctx, slug)
	if err != nil {
		return err
	}
	have := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		if l != nil {
			have[l.Name] = struct{}{}
		}
	}
	for _, l := range labels {
		if _, ok := have[l.Name]; ok {
			continue
		}
		// GitHub requires a 6-hex color with NO leading '#'; uzi's callers pass
		// "#rrggbb", so strip the '#' and default an empty one.
		color := strings.TrimPrefix(l.Color, "#")
		if color == "" {
			color = githubDefaultLabelColor
		}
		req := gh.CreateIssueLabelRequest{Name: l.Name, Color: &color}
		if _, _, err := g.client.Issues.CreateLabel(ctx, slug.owner, slug.repo, req); err != nil {
			return g.wrapErr(fmt.Sprintf("create label %q", l.Name), err)
		}
	}
	return nil
}

func (g *github) ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	opt := &gh.IssueListByRepoOptions{
		// GitHub's default state is "open", so StateAll must send "all" EXPLICITLY or
		// the Closed column / de-label-close eviction never see closed issues.
		State:       githubIssueStateParam(opts.State),
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	}
	if len(opts.Labels) > 0 {
		opt.Labels = append([]string(nil), opts.Labels...)
	}
	if opts.UpdatedAfter != nil {
		opt.Since = *opts.UpdatedAfter
	}
	var out []Issue
	page := 0
	for {
		page++
		issues, resp, err := g.client.Issues.ListByRepo(ctx, slug.owner, slug.repo, opt)
		if err != nil {
			return nil, g.wrapErr("list issues", err)
		}
		for _, i := range issues {
			// GitHub's /issues returns PULL REQUESTS too — each carries a non-nil
			// pull_request object. Filter them out; a PR leaking onto the board as a
			// card is silent and embarrassing (R4, the trap most likely to ship broken).
			if i == nil || i.PullRequestLinks != nil {
				continue
			}
			out = append(out, toGitHubIssue(i))
			// opts.Limit == 0 is the no-cap default (every pre-#158 caller); a positive
			// Limit stops as soon as that many issues are collected, truncating this page.
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list issues", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list issues", forgePaginationCapErr("page", maxForgePages))
		}
		// IssueListByRepoOptions embeds both ListCursorOptions and ListOptions, so a
		// bare opt.Page is ambiguous — qualify the integer-page field explicitly.
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func (g *github) GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return Issue{}, err
	}
	i, _, err := g.client.Issues.Get(ctx, slug.owner, slug.repo, int(issueIID))
	if err != nil {
		return Issue{}, g.wrapErr("get issue", err)
	}
	return toGitHubIssue(i), nil
}

func (g *github) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return Issue{}, err
	}
	req := gh.CreateIssueRequest{Title: title, Body: &description}
	if len(labels) > 0 {
		// GitHub auto-creates a referenced label that does not yet exist on the repo,
		// so — like the GitLab driver — the labels param is forwarded as names with no
		// pre-resolution (the PRD trigger label is never EnsureLabels'd, so this matters).
		req.Labels = append([]string(nil), labels...)
	}
	i, _, err := g.client.Issues.Create(ctx, slug.owner, slug.repo, req)
	if err != nil {
		return Issue{}, g.wrapErr("create issue", err)
	}
	return toGitHubIssue(i), nil
}

func (g *github) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	// GitHub's set-replace is PUT /issues/{n}/labels (ReplaceLabelsForIssue). Read
	// the current set, compute the target client-side (current − remove + add), and
	// PUT once. An unrelated label the caller neither adds nor removes SURVIVES
	// because it stays in target. The read/write is not transactional, so the same
	// lost-update window the Forgejo/GitLab drivers accept (D3) applies.
	cur, _, err := g.client.Issues.Get(ctx, slug.owner, slug.repo, int(issueIID))
	if err != nil {
		return g.wrapErr("update issue labels: read current issue", err)
	}
	current := map[string]struct{}{}
	for _, l := range cur.Labels {
		if l != nil {
			current[l.Name] = struct{}{}
		}
	}
	removeSet := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		removeSet[r] = struct{}{}
	}
	target := map[string]struct{}{}
	for name := range current {
		if _, drop := removeSet[name]; !drop {
			target[name] = struct{}{}
		}
	}
	for _, a := range add {
		target[a] = struct{}{}
	}
	// No-op: a card move that changes nothing must issue ZERO PUTs (D3).
	if sameNameSet(current, target) {
		return nil
	}
	names := make([]string, 0, len(target))
	for name := range target {
		names = append(names, name)
	}
	if _, _, err := g.client.Issues.ReplaceLabelsForIssue(ctx, slug.owner, slug.repo, int(issueIID), names); err != nil {
		return g.wrapErr("update issue labels", err)
	}
	return nil
}

// UpdateIssueDescription replaces an issue's body (PRD #72 M5). Unlike the
// Forgejo driver — whose SDK forces a title back to avoid wiping it — go-github's
// UpdateIssueRequest leaves Title nil (unchanged) and Labels/Assignees omitzero
// (unchanged) when only Body is set, so this sends the body ALONE. This method is
// called generically by forgesvc/prd_link_patch.go, so it must be real: a stub
// would compile and silently no-op PRD-link auto-patch on GitHub (S1).
func (g *github) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	req := gh.UpdateIssueRequest{Body: &description}
	if _, _, err := g.client.Issues.Update(ctx, slug.owner, slug.repo, int(issueIID), req); err != nil {
		return g.wrapErr("update issue description", err)
	}
	return nil
}

func (g *github) UserExists(ctx context.Context, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	_, resp, err := g.client.Users.Get(ctx, username)
	if err != nil {
		// GET /users/{username} 404s when the account does not exist — a false, not
		// an error (the caller downgrades a miss to a warning, never a hard failure).
		if resp != nil && resp.StatusCode == 404 {
			return false, nil
		}
		return false, g.wrapErr("lookup user", err)
	}
	return true, nil
}

func (g *github) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (Role, bool, error) {
	// GET /repositories/{id} returns permissions COMPUTED FOR THE AUTHENTICATED USER
	// (the bot). uzi always calls ProjectRole for the bot's own id, so the repo
	// permissions ARE the answer (D6) — GitHub exposes no per-arbitrary-user
	// branch/role flag at write role. forgeUserID is accepted for interface parity
	// but not separately queried; the caller (privcheck) checks the bot.
	r, resp, err := g.client.Repositories.GetByID(ctx, projectID)
	if err != nil {
		// A 404 → not a member (never an error): the bot has no visibility of the repo.
		if resp != nil && resp.StatusCode == 404 {
			return RoleNone, false, nil
		}
		return RoleNone, false, g.wrapErr("project role", err)
	}
	role := roleForGitHubPermissions(r.GetPermissions())
	// member = role.AtLeast(RoleWrite), mirroring the Forgejo derivation: a bot at
	// read/none is not an effective member for uzi's purposes (and is a violation).
	member := role.AtLeast(RoleWrite)
	return role, member, nil
}

// roleForGitHubPermissions maps GitHub's repo permission booleans onto the neutral
// Role. admin→Admin (a violation); maintain or push→Write (both grant git push);
// triage or pull→Read (neither grants push); none→None. Checked most- to
// least-privileged so the highest true bit wins. GetX() are nil-safe.
func roleForGitHubPermissions(p *gh.RepositoryPermissions) Role {
	switch {
	case p.GetAdmin():
		return RoleAdmin
	case p.GetMaintain():
		return RoleWrite
	case p.GetPush():
		return RoleWrite
	case p.GetTriage():
		return RoleRead
	case p.GetPull():
		return RoleRead
	default:
		return RoleNone
	}
}

// DefaultBranchProtection is the D6 CRUX and is materially weaker on GitHub than
// on Forgejo, so it is built with a fail-safe disposition and its own comment
// block. GitHub gives a write-role bot NO branch-scoped push/merge flag: the
// admin-gated GET /branches/{b}/protection 403s the bot (so this method NEVER
// calls it — the same discipline #65 kept for Forgejo's branch_protections/), and
// the reader-gated surfaces answer only partially.
//
// UNRESOLVED SPIKE (R1) — NOT EXECUTABLE IN THIS SANDBOX: whether
// Repositories.ListRulesForBranch reflects the CALLING BOT'S OWN bypass ability
// (so a returned enforced rule genuinely means the bot cannot push) versus merely
// listing rules that exist, could not be probed live here (no GitHub credentials).
// The design below is FAIL-SAFE UNDER BOTH outcomes, because on a Protected branch
// it NEVER reports a fabricated `WriteRoleCanPush/Merge = true`:
//   - If the endpoint DOES reflect bypass: an enforced pull_request/non_fast_forward
//     rule that comes back genuinely blocks the bot → Can*=false is authoritative.
//   - If it does NOT: a returned rule may not prove the bot is blocked, but we still
//     report Can*=false (over-cautious, never a false "can push"); and when NO rule
//     comes back we flag ProtectionUnverified so the consumer fails closed rather
//     than reading the false as "safe".
//
// Dispositions:
//   - Unprotected branch → WriteRoleCanPush=true, WriteRoleCanMerge=true,
//     ProtectionUnverified=false. An unprotected branch is the WORST case (a write
//     bot CAN push and merge); reporting the zero value would call the most
//     dangerous state "cannot push, cannot merge" (see BranchProtection).
//   - Protected + a readable, covering, enforced ruleset (pull_request or
//     non_fast_forward) → Can*=false, ProtectionUnverified=false (authoritatively
//     protected).
//   - Protected + NO readable ruleset explaining it (legacy branch protection,
//     invisible at write role) → Can*=false AND ProtectionUnverified=true. Never a
//     fabricated true, never a false "safe" (D6/R1). #66 must fail-closed on it.
//   - BotCanPush/BotCanMerge stay false (GitLab-only).
func (g *github) DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return BranchProtection{}, err
	}
	// GET /repos/{o}/{r}/branches/{branch} is reader-gated; its top-level `protected`
	// bool is readable at write role. maxRedirects=1 matches the SDK signature; this
	// endpoint does not redirect.
	b, resp, err := g.client.Repositories.GetBranch(ctx, slug.owner, slug.repo, branch, 1)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			// Branch absent → treat as unprotected-and-open, the not-safe shape the
			// other drivers report on a 404.
			return BranchProtection{WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
		}
		return BranchProtection{}, g.wrapErr("branch protection", err)
	}
	if !b.GetProtected() {
		return BranchProtection{WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
	}
	// Protected. Read rulesets (Metadata:read, reader-gated) — the only reader-gated
	// protection detail available to a write bot.
	rules, _, err := g.client.Repositories.ListRulesForBranch(ctx, slug.owner, slug.repo, branch, nil)
	if err != nil {
		// Rulesets should be readable at write role; if we cannot read them we cannot
		// authoritatively determine push/merge on a protected branch. Fail-safe.
		return BranchProtection{Protected: true, ProtectionUnverified: true}, nil
	}
	if rules != nil && (len(rules.PullRequest) > 0 || len(rules.NonFastForward) > 0) {
		// A readable, active, enforced ruleset covers the branch and blocks a direct
		// push / direct merge by the write-role bot. Authoritative.
		return BranchProtection{Protected: true}, nil
	}
	// Protected:true but no readable ruleset explains it — legacy branch protection.
	return BranchProtection{Protected: true, ProtectionUnverified: true}, nil
}

// --- Merge requests, issue events, notes, token introspection (M3) ---------

func (g *github) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return MergeRequest{}, err
	}
	pr, _, err := g.client.PullRequests.Get(ctx, slug.owner, slug.repo, int(mrIID))
	if err != nil {
		return MergeRequest{}, g.wrapErr("get merge request", err)
	}
	return toGitHubMergeRequest(pr), nil
}

// toGitHubMergeRequest maps a go-github PullRequest onto the neutral MergeRequest.
// GitHub's PR number is GitLab's iid — see githubMRState for the state fold.
func toGitHubMergeRequest(pr *gh.PullRequest) MergeRequest {
	return MergeRequest{IID: int64(pr.GetNumber()), State: githubMRState(pr), WebURL: pr.GetHTMLURL()}
}

// githubMRState folds GitHub's PR lifecycle onto the neutral MRState* vocabulary.
// GitHub uses state open/closed with a SEPARATE `merged` bool — not a distinct
// merged state — so a merged PR is state="closed", merged=true. Merged therefore
// WINS over closed (mirrors forgejoMRState). An unknown state passes through
// verbatim; IsKnownMRState then ignores it, so a transient glitch cannot poison
// the MR-close watcher's baseline.
func githubMRState(pr *gh.PullRequest) string {
	if pr.GetMerged() {
		return MRStateMerged
	}
	switch pr.GetState() {
	case "open":
		return MRStateOpened
	case "closed":
		return MRStateClosed
	default:
		return pr.GetState()
	}
}

func (g *github) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// GET /issues/{n}/events is well-typed in go-github (unlike Forgejo's timeline,
	// which needed a hand-parse): each IssueEvent carries event, actor, created_at,
	// and a single label object. "labeled"→add, "unlabeled"→remove; other events are
	// skipped. Chronological (oldest first), matching the GitLab driver. Paginated.
	opt := &gh.ListOptions{PerPage: githubPerPage}
	var out []LabelEvent
	page := 0
	for {
		page++
		events, resp, err := g.client.Issues.ListIssueEvents(ctx, slug.owner, slug.repo, int(issueIID), opt)
		if err != nil {
			return nil, g.wrapErr("list issue label events", err)
		}
		for _, e := range events {
			if e == nil {
				continue
			}
			var action string
			switch e.GetEvent() {
			case "labeled":
				action = "add"
			case "unlabeled":
				action = "remove"
			default:
				continue
			}
			ev := LabelEvent{
				ID:        e.GetID(),
				Action:    action,
				CreatedAt: e.GetCreatedAt().Time,
			}
			if l := e.GetLabel(); l != nil {
				ev.LabelName = l.Name
			}
			if a := e.GetActor(); a != nil {
				ev.Username = a.GetLogin()
			}
			out = append(out, ev)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list issue label events", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list issue label events", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

// ListIssueComments returns an issue's human comments, oldest-first (PRD #381).
// GitHub's issue-comments endpoint returns human comments only — events and the
// timeline are separate endpoints — so no System filter is needed (D2), and the
// default sort is created ASC (already oldest-first, D8). Paginated.
func (g *github) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]IssueComment, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	opt := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	var out []IssueComment
	page := 0
	for {
		page++
		comments, resp, err := g.client.Issues.ListComments(ctx, slug.owner, slug.repo, int(issueIID), opt)
		if err != nil {
			return nil, g.wrapErr("list issue comments", err)
		}
		for _, c := range comments {
			if c == nil {
				continue
			}
			ic := IssueComment{
				Body:      c.GetBody(),
				CreatedAt: c.GetCreatedAt().Time,
			}
			if u := c.GetUser(); u != nil {
				ic.AuthorForgeUserID = u.GetID()
				ic.AuthorUsername = u.GetLogin()
			}
			out = append(out, ic)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list issue comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list issue comments", forgePaginationCapErr("page", maxForgePages))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *github) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return IssueNote{}, err
	}
	c, _, err := g.client.Issues.CreateComment(ctx, slug.owner, slug.repo, int(issueIID), &gh.IssueComment{Body: &body})
	if err != nil {
		return IssueNote{}, g.wrapErr("create issue note", err)
	}
	return IssueNote{ID: c.GetID(), Body: c.GetBody()}, nil
}

// ListMergeRequestComments returns a PR's human + review-bot comments oldest-first
// (PRD #700). GitHub has no single endpoint for this, so the driver STITCHES three
// REST sources with a GraphQL read (Resolved facts):
//   - Issues.ListComments → the PR's top-level conversation notes (summary state,
//     no diff anchor, no reply/resolve thread).
//   - PullRequests.ListComments → inline review comments, each carrying Path/Line,
//     a REST ID that IS the databaseId (the reply anchor), and CommitID (HeadSHA).
//   - PullRequests.ListReviews → review-summary bodies (non-empty Body only).
//   - the GraphQL reviewThreads query → each thread's node id (the RESOLVE anchor)
//     joined to the REST inline comments on the shared databaseId.
//
// So an inline comment carries TWO anchors: ReplyID (REST databaseId) for
// CreateCommentInReplyTo and ResolveID (GraphQL thread node id) for
// resolveReviewThread. GitHub's issue/PR-comment and review endpoints return no
// forge system notes (those live on the separate events/timeline endpoints uzi
// never calls), so no D2 filter is needed; the driver sorts the merged list by
// CreatedAt for oldest-first (D8).
func (g *github) ListMergeRequestComments(ctx context.Context, projectID, mrIID int64) ([]MRComment, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return nil, err
	}
	number := int(mrIID)

	var out []MRComment

	// Source A: top-level PR conversation notes.
	icOpt := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	for page := 0; ; {
		page++
		comments, resp, err := g.client.Issues.ListComments(ctx, slug.owner, slug.repo, number, icOpt)
		if err != nil {
			return nil, g.wrapErr("list merge request comments", err)
		}
		for _, c := range comments {
			if c == nil {
				continue
			}
			mc := MRComment{
				ID:          c.GetID(), // REST databaseId of the issue comment
				Body:        c.GetBody(),
				CreatedAt:   c.GetCreatedAt().Time,
				ReviewState: ReviewCommentSummary,
			}
			if u := c.GetUser(); u != nil {
				mc.AuthorForgeUserID = u.GetID()
				mc.AuthorUsername = u.GetLogin()
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		icOpt.Page = resp.NextPage
	}

	// Source B: inline review comments. Record where each databaseId landed in out
	// so the GraphQL stitch can set its ResolveID.
	idxByDBID := map[int64]int{}
	prOpt := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: githubPerPage}}
	for page := 0; ; {
		page++
		comments, resp, err := g.client.PullRequests.ListComments(ctx, slug.owner, slug.repo, number, prOpt)
		if err != nil {
			return nil, g.wrapErr("list merge request comments", err)
		}
		for _, c := range comments {
			if c == nil {
				continue
			}
			mc := MRComment{
				ID:          c.GetID(), // REST databaseId of the inline review comment
				Body:        c.GetBody(),
				CreatedAt:   c.GetCreatedAt().Time,
				HeadSHA:     c.GetCommitID(),
				ReviewState: ReviewCommentInline,
			}
			if u := c.GetUser(); u != nil {
				mc.AuthorForgeUserID = u.GetID()
				mc.AuthorUsername = u.GetLogin()
			}
			if dbID := c.GetID(); dbID != 0 {
				mc.ReplyID = strconv.FormatInt(dbID, 10)
				idxByDBID[dbID] = len(out)
			}
			if p := c.Path; p != nil {
				path := *p
				mc.Path = &path
			}
			if c.Line != nil {
				line := *c.Line
				mc.Line = &line
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		prOpt.Page = resp.NextPage
	}

	// Source C: review-summary bodies (a review with only inline comments has an
	// empty Body and is skipped — its comments already came from source B).
	revOpt := &gh.ListOptions{PerPage: githubPerPage}
	for page := 0; ; {
		page++
		reviews, resp, err := g.client.PullRequests.ListReviews(ctx, slug.owner, slug.repo, number, revOpt)
		if err != nil {
			return nil, g.wrapErr("list merge request comments", err)
		}
		for _, r := range reviews {
			if r == nil || r.GetBody() == "" {
				continue
			}
			mc := MRComment{
				ID:          r.GetID(), // REST databaseId of the review summary
				Body:        r.GetBody(),
				CreatedAt:   r.GetSubmittedAt().Time,
				HeadSHA:     r.GetCommitID(),
				ReviewState: ReviewCommentSummary,
			}
			if u := r.GetUser(); u != nil {
				mc.AuthorForgeUserID = u.GetID()
				mc.AuthorUsername = u.GetLogin()
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		revOpt.Page = resp.NextPage
	}

	// GraphQL stitch: map each inline comment's databaseId → its thread node id
	// (the resolve anchor REST does not expose) and fold it onto the source-B
	// MRComments already collected.
	threadByDBID, err := g.reviewThreadIDsByDatabaseID(ctx, slug, number)
	if err != nil {
		return nil, err
	}
	for dbID, idx := range idxByDBID {
		if threadID, ok := threadByDBID[dbID]; ok {
			out[idx].ResolveID = threadID
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// reviewThreadIDsByDatabaseID runs the GraphQL reviewThreads query and returns a
// map from each thread comment's REST databaseId to the thread's node id — the
// join key ListMergeRequestComments uses to attach a resolve anchor to a REST
// inline comment (Resolved facts). It reuses the driver's graphqlDo helper (auth +
// endpoint + redaction), never a hand-rolled POST. It pages BOTH connections: the
// outer reviewThreads cursor here, and — for any thread with more than one page of
// comments — the inner comments cursor via reviewThreadCommentDBIDs, so neither a
// PR with >100 review threads nor a thread with >100 comments silently drops a
// ResolveID anchor. Both loops are backstopped by maxForgeItems/maxForgePages.
func (g *github) reviewThreadIDsByDatabaseID(ctx context.Context, slug repoSlug, number int) (map[int64]string, error) {
	const query = `query($owner: String!, $name: String!, $number: Int!, $threadCursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $threadCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          comments(first: 100) {
            pageInfo { hasNextPage endCursor }
            nodes { databaseId }
          }
        }
      }
    }
  }
}`
	m := map[int64]string{}
	var threadCursor *string
	page := 0
	// fetched counts every comment NODE returned (including databaseId==0 and
	// duplicate ids), accumulated across outer pages — not the deduplicated map
	// size. The cap must key off nodes actually fetched, because a single outer
	// page carrying duplicate or zero databaseIds can pull far more than
	// maxForgeItems nodes while len(m) stays under the ceiling.
	fetched := 0
	for {
		page++
		var resp struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID       string `json:"id"`
							Comments struct {
								PageInfo struct {
									HasNextPage bool   `json:"hasNextPage"`
									EndCursor   string `json:"endCursor"`
								} `json:"pageInfo"`
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": slug.owner, "name": slug.repo, "number": number}
		if threadCursor != nil {
			vars["threadCursor"] = *threadCursor
		} else {
			vars["threadCursor"] = nil
		}
		if err := g.graphqlDo(ctx, query, vars, &resp); err != nil {
			return nil, err
		}
		for _, node := range resp.Repository.PullRequest.ReviewThreads.Nodes {
			if node.ID == "" {
				continue
			}
			for _, cn := range node.Comments.Nodes {
				fetched++
				if cn.DatabaseID != 0 {
					m[cn.DatabaseID] = node.ID
				}
			}
			// Enforce the item cap PER THREAD, not once per outer page, and key it
			// off FETCHED comment nodes (duplicate and zero databaseIds included),
			// not the deduplicated map size: without this a single reviewThreads
			// page of up to 100 threads could each fan out to
			// reviewThreadCommentDBIDs and accumulate before the bound fired,
			// amplifying the maxForgeItems ceiling by the page size, and duplicate
			// or zero ids collapsing in the map could keep len(m) under the cap
			// while far more nodes were actually fetched.
			if fetched > maxForgeItems {
				return nil, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
			}
			if node.Comments.PageInfo.HasNextPage {
				// Bound the inner fan-out by the GLOBAL remaining budget so one
				// thread's overflow cannot exceed maxForgeItems either.
				rest, restFetched, err := g.reviewThreadCommentDBIDs(ctx, node.ID, node.Comments.PageInfo.EndCursor, maxForgeItems-fetched)
				if err != nil {
					return nil, err
				}
				for _, dbID := range rest {
					m[dbID] = node.ID
				}
				fetched += restFetched
				if fetched > maxForgeItems {
					return nil, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
				}
			}
		}
		if !resp.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage || resp.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor == "" {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(forgePaginationCapErr("page", maxForgePages))
		}
		next := resp.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
		threadCursor = &next
	}
	return m, nil
}

// reviewThreadCommentDBIDs pages the remaining comment databaseIds of one review
// thread past the first page the outer reviewThreads query already returned. GitHub
// cannot advance a nested connection cursor from the outer query, so it re-fetches
// the thread node by id and walks its comments connection from afterCursor (the
// outer page's comments.endCursor). It reuses graphqlDo and is backstopped by
// maxForgePages exactly as the outer loop; budget is the caller's remaining global
// item allowance, so one thread's comment fan-out cannot push the total past
// maxForgeItems. It counts every comment NODE fetched (duplicate and zero
// databaseIds included) against budget — not distinct ids — and returns that
// fetched-node count as its second return so the caller can accumulate it into the
// outer per-page fetched total.
func (g *github) reviewThreadCommentDBIDs(ctx context.Context, threadID, afterCursor string, budget int) ([]int64, int, error) {
	const query = `query($threadId: ID!, $commentCursor: String) {
  node(id: $threadId) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $commentCursor) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId }
      }
    }
  }
}`
	var out []int64
	cursor := afterCursor
	page := 0
	fetched := 0
	for {
		page++
		var resp struct {
			Node struct {
				Comments struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						DatabaseID int64 `json:"databaseId"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		}
		vars := map[string]any{"threadId": threadID, "commentCursor": cursor}
		if err := g.graphqlDo(ctx, query, vars, &resp); err != nil {
			return nil, fetched, err
		}
		for _, cn := range resp.Node.Comments.Nodes {
			fetched++
			if cn.DatabaseID != 0 {
				out = append(out, cn.DatabaseID)
			}
		}
		if fetched > budget {
			return nil, fetched, g.redact.error(forgePaginationCapErr("item", maxForgeItems))
		}
		if !resp.Node.Comments.PageInfo.HasNextPage || resp.Node.Comments.PageInfo.EndCursor == "" {
			break
		}
		if page >= maxForgePages {
			return nil, fetched, g.redact.error(forgePaginationCapErr("page", maxForgePages))
		}
		cursor = resp.Node.Comments.PageInfo.EndCursor
	}
	return out, fetched, nil
}

// ReplyMergeRequestComment posts an in-thread reply keyed on replyID, the REST
// databaseId of the review comment it answers (an MRComment.ReplyID).
func (g *github) ReplyMergeRequestComment(ctx context.Context, projectID, mrIID int64, replyID, body string) error {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return err
	}
	commentID, err := strconv.ParseInt(strings.TrimSpace(replyID), 10, 64)
	if err != nil {
		return fmt.Errorf("github: reply merge request comment: invalid reply id %q", replyID)
	}
	if _, _, err := g.client.PullRequests.CreateCommentInReplyTo(ctx, slug.owner, slug.repo, int(mrIID), body, commentID); err != nil {
		return g.wrapErr("reply merge request comment", err)
	}
	return nil
}

// ResolveMergeRequestThread resolves the review thread keyed on resolveID, the
// GraphQL thread node id (an MRComment.ResolveID). go-github ships no GraphQL
// client, so this issues the resolveReviewThread mutation through graphqlDo — the
// same authenticated, redacted endpoint path the read stitch uses (Resolved facts).
func (g *github) ResolveMergeRequestThread(ctx context.Context, _, _ int64, resolveID string) error {
	if strings.TrimSpace(resolveID) == "" {
		return fmt.Errorf("github: resolve merge request thread: empty resolve id")
	}
	const mutation = `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`
	return g.graphqlDo(ctx, mutation, map[string]any{"threadId": resolveID}, nil)
}

func (g *github) TokenInfo(ctx context.Context) (TokenInfo, error) {
	// HAND-ROLLED FROM RESPONSE HEADERS (D7): GitHub has no token-introspection JSON
	// endpoint. A classic PAT's granted scopes come back in X-OAuth-Scopes and its
	// expiry in GitHub-Authentication-Token-Expiration on ANY authenticated request;
	// validity is simply whether the request 200s. Make one lightweight call and read
	// the headers off *github.Response.Response.Header.
	_, resp, err := g.client.Users.Get(ctx, "")
	if err != nil {
		return TokenInfo{}, g.wrapErr("token info", err)
	}
	if resp == nil || resp.Response == nil {
		return TokenInfo{}, errors.New("github: token info: response carried no headers")
	}
	h := resp.Header
	if _, present := h[textproto.CanonicalMIMEHeaderKey("X-OAuth-Scopes")]; !present {
		// No scopes header at all → a fine-grained / un-introspectable token. VerifyToken
		// already rejects github_pat_, so this is a defensive fallback: surface the
		// introspection-unsupported sentinel (the caller downgrades to a warning).
		return TokenInfo{}, ErrTokenIntrospectionUnsupported
	}
	info := TokenInfo{
		Scopes: parseGitHubScopes(h.Get("X-OAuth-Scopes")),
		Active: true, // the call 200'd, so the token is usable
	}
	// Expiry: absent/empty means never expires (zero time). GitHub emits it as
	// "2006-01-02 15:04:05 -0700" or "... UTC"; on an unparseable value leave zero
	// rather than error (a missing expiry is not a scope-check failure).
	if exp := strings.TrimSpace(h.Get("GitHub-Authentication-Token-Expiration")); exp != "" {
		for _, layout := range []string{"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05 MST"} {
			if t, perr := time.Parse(layout, exp); perr == nil {
				info.ExpiresAt = t
				break
			}
		}
	}
	return info, nil
}

// parseGitHubScopes splits the X-OAuth-Scopes header (comma-separated) into a
// trimmed, empty-dropped scope list. A classic PAT with the {repo} scope yields
// []string{"repo"}; a scope-less header yields an empty slice.
func parseGitHubScopes(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toGitHubIssue maps a go-github Issue to the neutral domain type. GitHub's issue
// `number` is the per-repo sequential id GitLab calls iid. State is normalized to
// the "opened"/"closed" vocabulary the cache's state='opened' filter uses —
// GitHub says "open".
func toGitHubIssue(i *gh.Issue) Issue {
	// Labels starts as a non-nil empty slice: a nil slice caches as the jsonb scalar
	// `null`, which ships as JSON null to every consumer (same reason the other
	// drivers normalize it).
	issue := Issue{
		IID:         int64(i.GetNumber()),
		Title:       i.GetTitle(),
		State:       githubIssueState(i.GetState()),
		Labels:      []string{},
		Description: i.GetBody(),
		WebURL:      i.GetHTMLURL(),
		UpdatedAt:   i.GetUpdatedAt().Time,
		Assignees:   []int64{},
	}
	for _, l := range i.Labels {
		if l != nil {
			issue.Labels = append(issue.Labels, l.Name)
		}
	}
	for _, a := range i.Assignees {
		if a != nil {
			issue.Assignees = append(issue.Assignees, a.GetID())
		}
	}
	if u := i.GetUser(); u != nil {
		issue.Author = u.GetLogin()
	}
	return issue
}

// githubIssueState maps GitHub's "open"/"closed" onto the neutral
// "opened"/"closed" values the rest of uzi expects.
func githubIssueState(s string) string {
	if s == "closed" {
		return "closed"
	}
	return "opened"
}

// githubIssueStateParam maps the neutral state onto GitHub's request vocabulary
// (open/closed/all). GitHub defaults to "open", so StateAll must send "all"
// explicitly or closed issues are silently dropped.
func githubIssueStateParam(s IssueState) string {
	switch s {
	case StateOpened:
		return "open"
	case StateClosed:
		return "closed"
	default:
		return "all"
	}
}

// --- Raw GraphQL client for Projects v2 (PRD #364 M1) ----------------------
//
// Projects v2 has NO REST surface (F1), so the driver POSTs raw GraphQL to the
// forge's /graphql endpoint. Two transport preconditions are load-bearing and
// both are satisfied here:
//
//   - AUTH: the POST goes through g.client.Client(), the go-github *http.Client
//     whose transport injects "Authorization: Bearer <token>" (github.go:610) and
//     carries the per-call timeout. The struct's logClient is deliberately no-auth
//     and redirect-refusing and is NEVER used for Projects v2.
//   - ENDPOINT: derived from g.client.BaseURL() (never a hardcoded
//     api.github.com/graphql), so the httptest harness — which reaches the driver
//     via WithEnterpriseURLs and thus serves under <srv>/api/v3 — can mount and
//     intercept the GraphQL route. See graphqlEndpoint.

// graphqlEndpoint maps go-github's REST base URL onto the matching GraphQL
// endpoint. go-github's BaseURL() is "https://api.github.com/" for the default
// host and "<X>/api/v3/" for an enterprise/httptest base set via
// WithEnterpriseURLs. The default host's GraphQL lives at
// https://api.github.com/graphql; an enterprise/httptest base "<X>/api/v3" maps
// to "<X>/api/graphql" (GitHub's documented GHES convention), which is exactly
// what lets the mock mux intercept it.
func graphqlEndpoint(restBase string) string {
	base := strings.TrimRight(strings.TrimSpace(restBase), "/")
	if base == "" || base == "https://api.github.com" {
		return "https://api.github.com/graphql"
	}
	base = strings.TrimSuffix(base, "/api/v3")
	return base + "/api/graphql"
}

// graphqlURL is the driver's GraphQL POST target, derived from the authenticated
// client's configured REST base so tests can intercept it.
func (g *github) graphqlURL() string {
	return graphqlEndpoint(g.client.BaseURL())
}

// graphqlError is one entry of a GraphQL response's top-level errors array. A
// non-empty array means the operation failed even on an HTTP 200 (GraphQL's
// error model), so graphqlDo surfaces these as a redacted Go error.
type graphqlError struct {
	Message string `json:"message"`
	// Type is GitHub's machine-readable error classification (e.g. "NOT_FOUND",
	// "FORBIDDEN"). graphqlDo inspects it so a caller can distinguish a
	// non-existent login (NOT_FOUND) from a permission/transient failure without
	// parsing the redacted message; see ErrGitHubUserNotFound in projectsync.go.
	Type string `json:"type"`
}

// graphqlResponse is the standard GraphQL envelope: data plus an optional
// top-level errors array.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

// graphqlDo POSTs {"query":…, "variables":…} to the driver's GraphQL endpoint
// using the authenticated client's transport (auth header + timeout), then
// decodes the JSON "data" object into out. It returns a REDACTED error on a
// transport failure, a non-2xx status, an undecodable body, OR a non-empty
// GraphQL errors array (whose messages it surfaces, redacted so a reflected PAT
// never escapes). out may be nil when the caller does not need the data.
func (g *github) graphqlDo(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return g.wrapErr("graphql: encode request", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.graphqlURL(), bytes.NewReader(payload))
	if err != nil {
		return g.wrapErr("graphql: build request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// g.client.Client() carries the Bearer-token transport and the per-call
	// timeout; NEVER g.logClient (no auth, refuses redirects).
	resp, err := g.client.Client().Do(req)
	if err != nil {
		return g.wrapErr("graphql", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTraceBytes+1))
	if err != nil {
		return g.wrapErr("graphql: read response", err)
	}
	if resp.StatusCode/100 != 2 {
		return g.wrapErr("graphql", fmt.Errorf("status %d: %s", resp.StatusCode, string(body)))
	}
	var envelope graphqlResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return g.wrapErr("graphql: decode response", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		notFound := false
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
			if e.Type == "NOT_FOUND" {
				notFound = true
			}
		}
		// The joined message is still redacted so a reflected PAT never escapes.
		redactedErr := g.wrapErr("graphql", fmt.Errorf("%s", strings.Join(msgs, "; ")))
		if notFound {
			// Wrap the (already redacted) error so errors.Is(err,
			// ErrGitHubUserNotFound) is true for the caller while the scrubbed
			// message stays authoritative. Only a NOT_FOUND type is wrapped, so a
			// permission/transient error stays a plain redacted error and maps to
			// 500 rather than "bad username".
			return fmt.Errorf("%w: %w", ErrGitHubUserNotFound, redactedErr)
		}
		return redactedErr
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return g.wrapErr("graphql: decode data", err)
		}
	}
	return nil
}
