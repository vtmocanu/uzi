package forge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gitea "code.gitea.io/sdk/gitea"
	"golang.org/x/mod/semver"
)

// forgejoMinVersion is the lowest Forgejo release uzi supports (D4). Below it the
// CI-fix loop's job-logs route (GET /repos/{o}/{r}/actions/jobs/{job_id}/logs) is
// absent — it first shipped in v16.0.0. The gate is checked in VerifyToken, never
// in New (see the version-gate note there), and is a FEATURE gate, never a
// security control: GET /api/v1/version is public, unauthenticated, and
// self-reported, so nothing security-relevant may hang on it (D4 L2).
const forgejoMinVersion = "v16.0.0"

// forgejoPerPage is the pagination page size for every list call. 50 is a safe
// value under Forgejo's default MAX_RESPONSE_ITEMS; the driver paginates until
// the Link header stops advancing, so the exact size only trades round-trips.
const forgejoPerPage = 50

// forgejoDefaultLabelColor is used when EnsureLabels is handed a label with no
// color. uzi's callers always pin one (board columns, PrdlessLabelColor), but
// Forgejo's CreateLabel rejects an empty color outright (its client-side
// validator requires a 6-hex value), where GitLab lets the server assign one —
// so the driver supplies a neutral default rather than fail a label create.
const forgejoDefaultLabelColor = "#ededed"

// forgejo is the Forgejo REST driver, built on code.gitea.io/sdk/gitea (D5):
// Forgejo ships a +gitea-1.22.0 compatibility surface, and the gitea SDK carries
// the Actions runs/jobs/logs and issue-timeline endpoints forgejo-sdk lacks.
//
// The SDK's context is client-scoped (c.ctx), so a single long-lived client would
// leak one request's cancellation across the next (D5's W1). The driver therefore
// holds no *gitea.Client: it builds a fresh one per call over a shared, timeout-
// bounded *http.Client (client), each bound to that call's context. This is not a
// data-race workaround — the SDK mutex-guards c.ctx — only a cancellation-scoping
// one.
//
// The uzi Forge interface addresses repos by a numeric projectID, but every gitea
// SDK method takes an owner/repo pair, so the driver resolves id → slug via GET
// /repositories/{id} and caches it for the driver's (short) lifetime — a driver
// is rebuilt per ForgeForConnection call, so a rename cannot go stale across
// operations.
type forgejo struct {
	baseURL string
	token   string
	client  *http.Client
	redact  redactor

	mu    sync.RWMutex
	slugs map[int64]repoSlug
}

// repoSlug is a repo's owner/name pair, the addressing the gitea SDK needs.
type repoSlug struct {
	owner string
	repo  string
}

// newForgejo builds a Forgejo driver against baseURL using token, with a bounded
// per-call HTTP timeout. baseURL is assumed allowlist-checked by the caller (the
// SSRF guard lives in config). No network call happens here — the version gate
// runs in VerifyToken, so a bad version surfaces to the user rather than as a
// generic "could not initialize forge client".
func newForgejo(baseURL, token string, timeout time.Duration) (*forgejo, error) {
	return &forgejo{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		client:  timeoutClient(timeout),
		redact:  newRedactor(token),
		slugs:   map[int64]repoSlug{},
	}, nil
}

// newClient builds a per-call gitea client bound to ctx. SetGiteaVersion("")
// skips NewClient's own GET /version round-trip (the driver does its own version
// gate in VerifyToken); SetHTTPClient shares the driver's timeout client so the
// per-call construction is cheap.
func (f *forgejo) newClient(ctx context.Context) (*gitea.Client, error) {
	c, err := gitea.NewClient(f.baseURL,
		gitea.SetToken(f.token),
		gitea.SetHTTPClient(f.client),
		gitea.SetContext(ctx),
		gitea.SetGiteaVersion(""),
	)
	if err != nil {
		// A NewClient failure here can only come from the base URL (the version
		// round-trip is disabled); route it through the redactor in case the token
		// ever appears.
		return nil, f.redact.error(fmt.Errorf("forgejo: new client: %w", err))
	}
	return c, nil
}

// repoSlugFor resolves a numeric projectID to its owner/repo pair, caching the
// result for the driver's lifetime. GET /repositories/{id} always returns the
// CURRENT path, so a cached slug can only go stale within a single short-lived
// driver — acceptable, and a stale slug surfaces as a redacted 404, not silent
// corruption.
func (f *forgejo) repoSlugFor(c *gitea.Client, projectID int64) (repoSlug, error) {
	f.mu.RLock()
	s, ok := f.slugs[projectID]
	f.mu.RUnlock()
	if ok {
		return s, nil
	}
	r, _, err := c.GetRepoByID(projectID)
	if err != nil {
		return repoSlug{}, f.redact.error(fmt.Errorf("forgejo: resolve repo %d: %w", projectID, err))
	}
	if r == nil || r.Owner == nil {
		return repoSlug{}, fmt.Errorf("forgejo: resolve repo %d: incomplete repository payload", projectID)
	}
	s = repoSlug{owner: r.Owner.UserName, repo: r.Name}
	f.mu.Lock()
	f.slugs[projectID] = s
	f.mu.Unlock()
	return s, nil
}

func (f *forgejo) VerifyToken(ctx context.Context) (BotIdentity, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return BotIdentity{}, err
	}
	// Version gate FIRST (D4/D4a). GET /version is public and unauthenticated, so
	// it needs no valid token; refusing an unsupported instance up front means the
	// user hears "wrong version" rather than a confusing later failure. The gate's
	// error is deliberately NOT redacted — it carries only the server's
	// self-reported version string plus uzi's copy, no secret, and CreateConnection
	// surfaces it to the user verbatim.
	raw, _, err := c.ServerVersion()
	if err != nil {
		return BotIdentity{}, f.redact.error(fmt.Errorf("forgejo: read server version: %w", err))
	}
	if err := f.checkForgejoVersion(raw); err != nil {
		return BotIdentity{}, err
	}
	u, _, err := c.GetMyUserInfo()
	if err != nil {
		return BotIdentity{}, f.redact.error(fmt.Errorf("forgejo: verify token: %w", err))
	}
	// Forgejo always emits is_admin (no omitempty), so false means a real
	// non-admin — the privilege checker treats true as a violation.
	return BotIdentity{ForgeUserID: u.ID, Username: u.UserName, IsAdmin: u.IsAdmin}, nil
}

// checkForgejoVersion applies the D4a gate: strict SemVer 2.0.0 against the
// forgejoMinVersion floor. Forgejo strips the leading "v" that x/mod/semver
// requires, so we add it back; build metadata (+gitea-1.22.0) is ignored by the
// comparison (SemVer §10) and a prerelease (-dev-N) sorts below the release and
// is refused (§11.3). An unparseable version refuses: the gate is a feature gate
// (buys and costs no security, D4 L2), so refusing is purely the safer failure
// mode — one clear error at connect beats a bare 404 mid-run.
//
// The error wraps ErrForgeVersionUnsupported (so a privcheck consumer can
// errors.Is it — see that sentinel's doc) and is NOT routed through redact.error,
// which would sever the Unwrap chain that errors.Is walks. But the reported
// version is the server's self-reported, UNTRUSTED string, so the interpolated
// value alone is scrubbed with the value-based string redactor: a hostile instance
// that holds the PAT and reflects it into /version (`{"version":"<token>"}` →
// unparseable → refused) cannot leak it through this error.
func (f *forgejo) checkForgejoVersion(reported string) error {
	min := strings.TrimPrefix(forgejoMinVersion, "v")
	// Parse from the raw value; interpolate only the scrubbed value into the message.
	v := "v" + strings.TrimPrefix(strings.TrimSpace(reported), "v")
	safe := f.redact.string(reported)
	if !semver.IsValid(v) {
		return fmt.Errorf("forgejo: server reports an unrecognized version %q; uzi requires Forgejo %s or newer: %w", safe, min, ErrForgeVersionUnsupported)
	}
	if semver.Compare(v, forgejoMinVersion) < 0 {
		return fmt.Errorf("forgejo: server version %q is below the required Forgejo %s: %w", safe, min, ErrForgeVersionUnsupported)
	}
	return nil
}

func (f *forgejo) ListProjects(ctx context.Context) ([]Project, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	// GET /user/repos has no minimum-permission filter (unlike GitLab's
	// min_access_level), so a read-only repo comes back and must be dropped
	// client-side: the picker only offers repos the bot can actually push to.
	opt := gitea.ListReposOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	var out []Project
	for {
		repos, resp, err := c.ListMyRepos(opt)
		if err != nil {
			return nil, f.redact.error(fmt.Errorf("forgejo: list projects: %w", err))
		}
		for _, r := range repos {
			if r == nil || r.Permissions == nil || !r.Permissions.Push {
				continue
			}
			out = append(out, Project{
				ForgeProjectID:    r.ID,
				PathWithNamespace: r.FullName,
				WebURL:            r.HTMLURL,
				DefaultBranch:     r.DefaultBranch,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (f *forgejo) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	labels, err := f.listRepoLabels(c, slug)
	if err != nil {
		return nil, err
	}
	out := make([]Label, 0, len(labels))
	for _, l := range labels {
		out = append(out, Label{Name: l.Name, Color: l.Color})
	}
	return out, nil
}

// listRepoLabels returns every label on a repo, paginated internally. It backs
// both ListLabels and the name→id resolution EnsureLabels/UpdateIssueLabels/
// CreateIssue need (Forgejo's label writes are keyed by id, not name).
func (f *forgejo) listRepoLabels(c *gitea.Client, slug repoSlug) ([]*gitea.Label, error) {
	opt := gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	var out []*gitea.Label
	for {
		labels, resp, err := c.ListRepoLabels(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, f.redact.error(fmt.Errorf("forgejo: list labels: %w", err))
		}
		out = append(out, labels...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (f *forgejo) EnsureLabels(ctx context.Context, projectID int64, labels []Label) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	existing, err := f.listRepoLabels(c, slug)
	if err != nil {
		return err
	}
	have := make(map[string]struct{}, len(existing))
	for _, l := range existing {
		have[l.Name] = struct{}{}
	}
	for _, l := range labels {
		if _, ok := have[l.Name]; ok {
			continue
		}
		color := l.Color
		if color == "" {
			color = forgejoDefaultLabelColor
		}
		if _, _, err := c.CreateLabel(slug.owner, slug.repo, gitea.CreateLabelOption{Name: l.Name, Color: color}); err != nil {
			return f.redact.error(fmt.Errorf("forgejo: create label %q: %w", l.Name, err))
		}
	}
	return nil
}

func (f *forgejo) ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	opt := gitea.ListIssueOption{
		ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage},
		// state=all is mandatory (the Closed column + de-label/close eviction need
		// closed issues). type=issues asks Forgejo to omit pull requests, but the
		// client-side filter below is the actual guarantee (R4): a PR is modelled as
		// an issue with a non-nil pull_request, and one leaking onto the board as a
		// card is silent and embarrassing.
		State: gitea.StateAll,
		Type:  gitea.IssueTypeIssue,
	}
	if len(opts.Labels) > 0 {
		opt.Labels = append([]string(nil), opts.Labels...)
	}
	if opts.UpdatedAfter != nil {
		opt.Since = *opts.UpdatedAfter
	}
	var out []Issue
	for {
		issues, resp, err := c.ListRepoIssues(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, f.redact.error(fmt.Errorf("forgejo: list issues: %w", err))
		}
		for _, i := range issues {
			if i == nil || i.PullRequest != nil {
				continue // a pull request is an issue with pull_request != null; never a card.
			}
			out = append(out, toForgejoIssue(i))
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (f *forgejo) GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return Issue{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return Issue{}, err
	}
	i, _, err := c.GetIssue(slug.owner, slug.repo, issueIID)
	if err != nil {
		return Issue{}, f.redact.error(fmt.Errorf("forgejo: get issue: %w", err))
	}
	return toForgejoIssue(i), nil
}

func (f *forgejo) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return Issue{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return Issue{}, err
	}
	opt := gitea.CreateIssueOption{Title: title, Body: description}
	if len(labels) > 0 {
		// Forgejo's create takes label IDs, not names; resolve against the repo
		// catalog. uzi ensures its labels exist before use, so an unresolved name is
		// a real error, not something to silently drop.
		ids, err := f.resolveLabelIDs(c, slug, labels, nil)
		if err != nil {
			return Issue{}, err
		}
		opt.Labels = ids
	}
	i, _, err := c.CreateIssue(slug.owner, slug.repo, opt)
	if err != nil {
		return Issue{}, f.redact.error(fmt.Errorf("forgejo: create issue: %w", err))
	}
	return toForgejoIssue(i), nil
}

func (f *forgejo) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	// Forgejo has no add/remove delta — only a full-set PUT (ReplaceIssueLabels).
	// Read the current set immediately before the write (D3), compute the target
	// set client-side (current − remove + add), and PUT once. The read/write is not
	// transactional, so a label a human adds in the ~1 RTT window is dropped by our
	// PUT — the accepted, documented lost-update window (D3); the single-column
	// board invariant still holds because the PUT itself is atomic server-side.
	current, currentIDs, err := f.issueLabelNames(c, slug, issueIID)
	if err != nil {
		return err
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
	ids, err := f.resolveLabelIDs(c, slug, names, currentIDs)
	if err != nil {
		return err
	}
	if _, _, err := c.ReplaceIssueLabels(slug.owner, slug.repo, issueIID, gitea.IssueLabelsOption{Labels: ids}); err != nil {
		return f.redact.error(fmt.Errorf("forgejo: update issue labels: %w", err))
	}
	return nil
}

// issueLabelNames returns the labels currently on an issue as a name set plus a
// name→id map, paginated internally. Both are computed from one read so the
// no-op check and the id resolution share it.
func (f *forgejo) issueLabelNames(c *gitea.Client, slug repoSlug, issueIID int64) (map[string]struct{}, map[string]int64, error) {
	opt := gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	names := map[string]struct{}{}
	ids := map[string]int64{}
	for {
		labels, resp, err := c.GetIssueLabels(slug.owner, slug.repo, issueIID, opt)
		if err != nil {
			return nil, nil, f.redact.error(fmt.Errorf("forgejo: list issue labels: %w", err))
		}
		for _, l := range labels {
			names[l.Name] = struct{}{}
			ids[l.Name] = l.ID
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return names, ids, nil
}

// resolveLabelIDs maps label names to Forgejo label ids. It uses known (already
// on the issue) ids first, then fetches the repo catalog once for any remaining
// name. An unresolved name is an error: silently dropping it would corrupt the
// board's single-column invariant.
func (f *forgejo) resolveLabelIDs(c *gitea.Client, slug repoSlug, names []string, known map[string]int64) ([]int64, error) {
	ids := make([]int64, 0, len(names))
	var missing []string
	for _, name := range names {
		if id, ok := known[name]; ok {
			ids = append(ids, id)
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return ids, nil
	}
	catalog, err := f.listRepoLabels(c, slug)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]int64, len(catalog))
	for _, l := range catalog {
		byName[l.Name] = l.ID
	}
	for _, name := range missing {
		id, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("forgejo: label %q does not exist on the repo", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (f *forgejo) UserExists(ctx context.Context, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	c, err := f.newClient(ctx)
	if err != nil {
		return false, err
	}
	_, resp, err := c.GetUserInfo(username)
	if err != nil {
		// GET /users/{username} 404s when the account does not exist — a false, not
		// an error (the caller downgrades a miss to a warning, never a hard failure).
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, f.redact.error(fmt.Errorf("forgejo: lookup user: %w", err))
	}
	return true, nil
}

func (f *forgejo) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (Role, bool, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return RoleNone, false, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return RoleNone, false, err
	}
	// The collaborator-permission endpoint is keyed by username, but the interface
	// hands us a numeric id, so resolve it. This is the bot's own id in every uzi
	// caller (privcheck checks the bot); GetUserByID works for any visible user.
	u, _, err := c.GetUserByID(forgeUserID)
	if err != nil {
		return RoleNone, false, f.redact.error(fmt.Errorf("forgejo: resolve user %d: %w", forgeUserID, err))
	}
	res, resp, err := c.CollaboratorPermission(slug.owner, slug.repo, u.UserName)
	if err != nil {
		// A 404 here means the USER does not exist (or was removed from a PRIVATE
		// repo, whose middleware 404s the route) — reported as not-a-member, not an
		// error. This is NOT the removed-on-public case: a bot removed from a PUBLIC
		// repo gets 200 with permission:"read", handled by the derivation below (D7).
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return RoleNone, false, nil
		}
		return RoleNone, false, f.redact.error(fmt.Errorf("forgejo: project role: %w", err))
	}
	if res == nil {
		return RoleNone, false, fmt.Errorf("forgejo: project role: empty permission payload")
	}
	role := roleForForgejoPermission(res.Permission)
	// member is derived from the PERMISSION, not the status code (D7). On a public
	// repo a bot with no grant returns permission:"read" (the public baseline),
	// indistinguishable from an explicit read collaborator — uzi treats "read or
	// none" as not an effective member, so a bot removed from a public repo still
	// raises the "no longer a member" finding a naive 404-check would miss. A
	// consequence worth naming: a bot demoted to read reads as not-a-member here,
	// where the GitLab driver would say "below write role"; both are violations,
	// and the read/removed states are unobservable-apart on a public repo.
	member := role.AtLeast(RoleWrite)
	return role, member, nil
}

// roleForForgejoPermission maps Forgejo's permission vocabulary onto the neutral
// Role. Forgejo already speaks none|read|write|admin|owner (no numeric levels to
// launder), so this is a direct rename, not the lossy collapse the GitLab driver
// does over access-level integers.
func roleForForgejoPermission(p gitea.AccessMode) Role {
	switch p {
	case gitea.AccessModeOwner:
		return RoleOwner
	case gitea.AccessModeAdmin:
		return RoleAdmin
	case gitea.AccessModeWrite:
		return RoleWrite
	case gitea.AccessModeRead:
		return RoleRead
	default:
		return RoleNone
	}
}

func (f *forgejo) DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return BranchProtection{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return BranchProtection{}, err
	}
	// GET /repos/{o}/{r}/branches/{branch} is reader-gated (reqRepoReader), so a
	// write bot can read it — unlike /branch_protections/{name}, whose whole group
	// is reqAdmin() and 403s a write bot, degrading the guardrail to a warning
	// (D6). The endpoint returns protected / user_can_push / user_can_merge
	// COMPUTED FOR THE CALLING BOT by the same path the pre-receive hook enforces,
	// so it is a direct authoritative answer to "can this bot push/merge to main",
	// not GitLab's inference from access levels. We never call branch_protections.
	b, resp, err := c.GetRepoBranch(slug.owner, slug.repo, branch)
	if err != nil {
		// A 404 means the branch itself is absent (an unprotected branch still
		// returns 200 with protected:false — Forgejo's early-return path). Treat a
		// missing branch as unprotected-and-open, the same not-safe shape the GitLab
		// driver reports on its 404: reporting the zero value would call the most
		// dangerous state "cannot push, cannot merge" (see BranchProtection).
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return BranchProtection{Protected: false, WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
		}
		return BranchProtection{}, f.redact.error(fmt.Errorf("forgejo: branch protection: %w", err))
	}
	// user_can_push / user_can_merge are the calling bot's authoritative rights.
	// Forgejo folds the write-role capability and any per-user grant into these two
	// booleans (D6: "subsumed by user_can_push"), so they map onto WriteRoleCanPush
	// / WriteRoleCanMerge — the fields the checker and the R12 shared evaluator key
	// on. BotCanPush / BotCanMerge (the GitLab per-user-grant fields) stay false:
	// there is no separate signal to populate them, and leaving them clear keeps an
	// unprotected branch described identically to the GitLab driver (WriteRole*
	// true, Bot* false), which is what R12/test #9b require.
	return BranchProtection{
		Protected:         b.Protected,
		WriteRoleCanPush:  b.UserCanPush,
		WriteRoleCanMerge: b.UserCanMerge,
	}, nil
}

// toForgejoIssue maps a gitea Issue to the neutral domain type. Forgejo's issue
// index (json "number") is the per-repo sequential number GitLab calls iid, so it
// carries over as IID. State is normalized to the "opened"/"closed" vocabulary
// the GitLab driver and the issue cache (state='opened' filter) already use —
// Forgejo says "open".
func toForgejoIssue(i *gitea.Issue) Issue {
	issue := Issue{
		IID:         i.Index,
		Title:       i.Title,
		State:       forgejoIssueState(i.State),
		Description: i.Body,
		WebURL:      i.HTMLURL,
		UpdatedAt:   i.Updated,
	}
	for _, l := range i.Labels {
		if l != nil {
			issue.Labels = append(issue.Labels, l.Name)
		}
	}
	if i.Poster != nil {
		issue.Author = i.Poster.UserName
	}
	return issue
}

// forgejoIssueState maps Forgejo's issue state onto the neutral "opened"/"closed"
// values the rest of uzi (and the GitLab driver) expect.
func forgejoIssueState(s gitea.StateType) string {
	if s == gitea.StateClosed {
		return "closed"
	}
	return "opened"
}

// sameNameSet reports whether two name sets are equal.
func sameNameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// --- Stubs filled by later milestones -------------------------------------
//
// M2 lands the whole Forge interface as compiling code so M4 and M5 (which fill
// these in) touch disjoint files and can run in parallel. Each stub returns
// errForgejoNotImplemented; none is reachable from the API until the M6b gate
// flip, and no caller exercises them before M4/M5 land.

// errForgejoNotImplemented marks a Forge method whose Forgejo implementation has
// not landed yet. It carries no secret.
var errForgejoNotImplemented = fmt.Errorf("forgejo: method not implemented yet")

// GetMergeRequest is filled by M4.
func (f *forgejo) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	return MergeRequest{}, errForgejoNotImplemented
}

// ListIssueLabelEvents is filled by M4.
func (f *forgejo) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	return nil, errForgejoNotImplemented
}

// CreateIssueNote is filled by M4.
func (f *forgejo) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	return IssueNote{}, errForgejoNotImplemented
}

// TokenInfo is filled by M4 (hand-rolled — the SDK's ListAccessTokens gates on
// BasicAuth client-side, D5).
func (f *forgejo) TokenInfo(ctx context.Context) (TokenInfo, error) {
	return TokenInfo{}, errForgejoNotImplemented
}
