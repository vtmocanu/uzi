package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
func newForgejo(baseURL, token string, timeout time.Duration) *forgejo {
	return &forgejo{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		client:  timeoutClient(timeout),
		redact:  newRedactor(token),
		slugs:   map[int64]repoSlug{},
	}
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
	page := 0
	for {
		page++
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
		if len(out) > maxForgeItems {
			return nil, f.redact.error(fmt.Errorf("forgejo: list projects: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.redact.error(fmt.Errorf("forgejo: list projects: %w", forgePaginationCapErr("page", maxForgePages)))
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
	page := 0
	for {
		page++
		labels, resp, err := c.ListRepoLabels(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, f.redact.error(fmt.Errorf("forgejo: list labels: %w", err))
		}
		out = append(out, labels...)
		if len(out) > maxForgeItems {
			return nil, f.redact.error(fmt.Errorf("forgejo: list labels: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.redact.error(fmt.Errorf("forgejo: list labels: %w", forgePaginationCapErr("page", maxForgePages)))
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
		// state=all remains the DEFAULT (opts.State's zero value): the Closed column
		// and de-label/close eviction need closed issues. type=issues asks Forgejo to
		// omit pull requests, but the client-side filter below is the actual guarantee
		// (R4): a PR is modelled as an issue with a non-nil pull_request, and one
		// leaking onto the board as a card is silent and embarrassing.
		//
		// THE STATE VALUE MUST BE TRANSLATED, and this is the one place in M6 where a
		// pass-through is wrong. Forgejo's REQUEST vocabulary is open/closed/all;
		// uzi's neutral vocabulary is opened/closed/all. Passing the neutral "opened"
		// straight through sends an invalid state and the filter silently does not
		// apply. The reason nothing else in the codebase notices the difference is
		// that forgejoIssueState already normalises the RESPONSE the other way, so the
		// divergence is invisible everywhere except right here.
		State: forgejoIssueStateParam(opts.State),
		Type:  gitea.IssueTypeIssue,
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
		issues, resp, err := c.ListRepoIssues(slug.owner, slug.repo, opt)
		if err != nil {
			return nil, f.redact.error(fmt.Errorf("forgejo: list issues: %w", err))
		}
		for _, i := range issues {
			if i == nil || i.PullRequest != nil {
				continue // a pull request is an issue with pull_request != null; never a card.
			}
			out = append(out, toForgejoIssue(i))
			// opts.Limit == 0 is the no-cap default (every pre-#158 caller); a positive
			// Limit stops as soon as that many issues are collected, truncating this page.
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.redact.error(fmt.Errorf("forgejo: list issues: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.redact.error(fmt.Errorf("forgejo: list issues: %w", forgePaginationCapErr("page", maxForgePages)))
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

// UpdateIssueDescription replaces an issue's body (PRD #72 M5).
//
// THE INTERNAL READ IS NOT OPTIONAL, and this is the hazard the whole method
// exists to work around. In code.gitea.io/sdk/gitea, EditIssueOption.Title is a
// plain `string` with the json tag "title" and NO `omitempty`, so a naive call
// PATCHes `"title": ""` and can wipe the issue's title. Whether Forgejo happens to
// ignore an empty title is not verifiable from this tree, and "probably ignores
// it" is not a basis for a write against a user's issue. So: read the issue first
// and send its current title back unchanged.
//
// A driver absorbing its own forge's quirk is the established shape here —
// UpdateIssueLabels above does a read-then-full-PUT for the same kind of reason
// (Forgejo has no add/remove delta). The interface stays neutral, which is the
// point; the GitLab driver sends the description alone.
func (f *forgejo) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	cur, _, err := c.GetIssue(slug.owner, slug.repo, issueIID)
	if err != nil {
		// Redaction is per-method and not automatic, so the INTERNAL read's error
		// needs wrapping too — it carries the same client and the same PAT.
		return f.redact.error(fmt.Errorf("forgejo: update issue description: read current issue: %w", err))
	}
	if _, _, err := c.EditIssue(slug.owner, slug.repo, issueIID, gitea.EditIssueOption{
		Title: cur.Title,
		Body:  &description,
	}); err != nil {
		return f.redact.error(fmt.Errorf("forgejo: update issue description: %w", err))
	}
	return nil
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
	page := 0
	for {
		page++
		labels, resp, err := c.GetIssueLabels(slug.owner, slug.repo, issueIID, opt)
		if err != nil {
			return nil, nil, f.redact.error(fmt.Errorf("forgejo: list issue labels: %w", err))
		}
		for _, l := range labels {
			names[l.Name] = struct{}{}
			ids[l.Name] = l.ID
		}
		if len(names) > maxForgeItems {
			return nil, nil, f.redact.error(fmt.Errorf("forgejo: list issue labels: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, nil, f.redact.error(fmt.Errorf("forgejo: list issue labels: %w", forgePaginationCapErr("page", maxForgePages)))
		}
		opt.Page = resp.NextPage
	}
	return names, ids, nil
}

// resolveLabelIDs maps label names to Forgejo label ids. It uses known (already
// on the issue) ids first, then fetches the repo catalog once for any remaining
// name. A name still unresolved is CREATED (with the default color) and its new id
// used — matching GitLab, whose issue-create / add_labels auto-create a referenced
// label server-side (gitlab.go CreateIssue just forwards the labels param). This is
// load-bearing: uzi's CreateIssue stamps the PRD trigger label, which is NOT a
// board column and so is never EnsureLabels'd, so erroring here would 502 every
// issue creation on a Forgejo repo that lacks the label — parity that a Forgejo
// e2e (M9) caught. Silently dropping a name is still never done: the board's
// single-column invariant needs every target label present.
//
// Shared with UpdateIssueLabels (not CreateIssue-only): in the rare case a board
// column is deleted mid-move, this recreates it with the DEFAULT color, not its
// configured one, until the next EnsureLabels re-pins it — a benign self-heal,
// delete-race edge only.
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
			created, _, err := c.CreateLabel(slug.owner, slug.repo, gitea.CreateLabelOption{Name: name, Color: forgejoDefaultLabelColor})
			if err != nil {
				return nil, f.redact.error(fmt.Errorf("forgejo: create missing label %q: %w", name, err))
			}
			id = created.ID
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
	// Labels starts as a non-nil empty slice for the same reason the GitLab driver
	// normalizes it: the append loop below leaves it nil for a label-less issue, and
	// a nil slice caches as the jsonb scalar `null`, which survives the round trip
	// and ships as JSON null to every consumer.
	issue := Issue{
		IID:         i.Index,
		Title:       i.Title,
		State:       forgejoIssueState(i.State),
		Labels:      []string{},
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

// forgejoIssueStateParam maps the NEUTRAL state onto Forgejo's request vocabulary.
// This is the outbound counterpart of forgejoIssueState below, and the two are not
// symmetric names by accident: Forgejo says "open" where uzi says "opened", so a
// caller's neutral StateOpened must become gitea.StateOpen here or the filter is
// silently ignored. Anything unrecognised falls back to StateAll, which is the
// pre-M6 behaviour and the safe direction (over-fetch, never under-fetch).
func forgejoIssueStateParam(s IssueState) gitea.StateType {
	switch s {
	case StateOpened:
		return gitea.StateOpen
	case StateClosed:
		return gitea.StateClosed
	default:
		return gitea.StateAll
	}
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

// --- Merge requests, issue timeline, notes, token introspection (M4) -------

func (f *forgejo) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return MergeRequest{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return MergeRequest{}, err
	}
	pr, _, err := c.GetPullRequest(slug.owner, slug.repo, mrIID)
	if err != nil {
		return MergeRequest{}, f.redact.error(fmt.Errorf("forgejo: get merge request: %w", err))
	}
	return toForgejoMergeRequest(pr), nil
}

// toForgejoMergeRequest maps a gitea PullRequest onto the neutral MergeRequest.
// Forgejo's PR index (json "number") is GitLab's iid, and its state vocabulary
// differs — see forgejoMRState.
func toForgejoMergeRequest(pr *gitea.PullRequest) MergeRequest {
	return MergeRequest{IID: pr.Index, State: forgejoMRState(pr), WebURL: pr.HTMLURL}
}

// forgejoMRState maps a Forgejo PR onto the neutral MRState* vocabulary. Forgejo
// says "open"/"closed" (not GitLab's "opened") and carries merged separately, so a
// merged PR is state="closed" with merged=true — verified live on 16.0.0. Merged
// therefore wins over closed. Forgejo has no "locked" lifecycle state (IsLocked is
// a separate flag, not a state), so MRStateLocked is never produced. An unknown
// state passes through verbatim; IsKnownMRState then ignores it, so a transient
// forge glitch cannot poison the MR-close watcher's baseline.
func forgejoMRState(pr *gitea.PullRequest) string {
	if pr.HasMerged {
		return MRStateMerged
	}
	switch pr.State {
	case gitea.StateOpen:
		return MRStateOpened
	case gitea.StateClosed:
		return MRStateClosed
	default:
		return string(pr.State)
	}
}

func (f *forgejo) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return IssueNote{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return IssueNote{}, err
	}
	note, _, err := c.CreateIssueComment(slug.owner, slug.repo, issueIID, gitea.CreateIssueCommentOption{Body: body})
	if err != nil {
		return IssueNote{}, f.redact.error(fmt.Errorf("forgejo: create issue note: %w", err))
	}
	return IssueNote{ID: note.ID, Body: note.Body}, nil
}

func (f *forgejo) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	// Hand-rolled parse rather than the SDK's ListIssueTimeline: gitea-sdk types the
	// timeline's `label` field as []*Label, but Forgejo 16.0.0 serializes a label
	// event's label as a SINGLE object (verified live + swagger $ref), so the SDK
	// call ERRORS on exactly the label events we need. See forgejoTimelineEntry.
	var out []LabelEvent
	for page := 1; ; page++ {
		raw, err := f.rawGet(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?page=%d&limit=%d",
			url.PathEscape(slug.owner), url.PathEscape(slug.repo), issueIID, page, forgejoPerPage))
		if err != nil {
			return nil, err // already redacted by rawGet
		}
		var entries []forgejoTimelineEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, f.redact.error(fmt.Errorf("forgejo: parse issue timeline: %w", err))
		}
		for _, e := range entries {
			// Only label events; other timeline entries (comments, milestone/title
			// changes, assignments) have type != "label" and a null label.
			if e.Type != "label" || e.Label == nil {
				continue
			}
			ev := LabelEvent{
				ID:        e.ID,
				Action:    forgejoLabelAction(e.Body),
				LabelName: e.Label.Name,
				CreatedAt: e.CreatedAt,
			}
			if e.User != nil {
				ev.Username = e.User.Login
			}
			out = append(out, ev)
		}
		if len(out) > maxForgeItems {
			return nil, f.redact.error(fmt.Errorf("forgejo: list issue label events: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		// The timeline is chronological (oldest first), matching the GitLab driver.
		if len(entries) < forgejoPerPage {
			break
		}
		if page >= maxForgePages {
			return nil, f.redact.error(fmt.Errorf("forgejo: list issue label events: %w", forgePaginationCapErr("page", maxForgePages)))
		}
	}
	return out, nil
}

// forgejoLabelAction decodes Forgejo's UNDOCUMENTED label-event convention (R6):
// a label add records body == "1" (issue_label.go: Content "1"), a remove records
// body == "" (deleteIssueLabel omits Content). Both verified live on 16.0.0. This
// is pinned by a test so an SDK/Forgejo change to the convention fails loudly
// rather than silently attributing every event as a remove.
func forgejoLabelAction(body string) string {
	if body == "1" {
		return "add"
	}
	return "remove"
}

// forgejoTimelineEntry is the subset of a Forgejo issue-timeline entry uzi reads.
// It exists because the gitea SDK's TimelineComment types `label` as []*Label,
// which cannot unmarshal Forgejo's single-object label (verified live). Only the
// fields the label-event mapping needs are modelled.
type forgejoTimelineEntry struct {
	ID        int64            `json:"id"`
	Type      string           `json:"type"`
	Body      string           `json:"body"`
	User      *forgejoUserRef  `json:"user"`
	Label     *forgejoLabelRef `json:"label"`
	CreatedAt time.Time        `json:"created_at"`
}

type forgejoUserRef struct {
	Login string `json:"login"`
}

type forgejoLabelRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (f *forgejo) TokenInfo(ctx context.Context) (TokenInfo, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return TokenInfo{}, err
	}
	// GET /users/{username}/tokens is keyed by username and gated reqSelfOrAdmin (it
	// admits the bot querying itself, D5), so resolve the bot's own login first.
	u, _, err := c.GetMyUserInfo()
	if err != nil {
		return TokenInfo{}, f.redact.error(fmt.Errorf("forgejo: token info: identify bot: %w", err))
	}
	// Hand-rolled, NOT via the SDK: gitea-sdk's ListAccessTokens refuses without
	// BasicAuth ("username not set: only BasicAuth allowed"), a CLIENT-side gate the
	// server does not impose for the GET (D5). The list carries no secret — sha1 is
	// empty except at creation, only token_last_eight is returned — but the error
	// path still routes through the redactor (rawGet does this).
	//
	// token_last_eight is the ONLY per-token fingerprint the list exposes (sha1 is
	// empty post-creation), so on the astronomically rare last-eight COLLISION
	// between two of the bot's own tokens, uzi cannot tell which one authenticated.
	// This is a known, accepted Forgejo-API limit, not an oversight — there is no
	// better disambiguator to invent. We therefore scan every page, require EXACTLY
	// one match, and on 0 or >1 fail SAFE: a generic error that the checker
	// downgrades to a "could not verify scopes" warning. Picking the first match
	// would fail OPEN — an over-scoped ("all") authenticating token could be masked
	// by a correctly-scoped colliding sibling, sliding it past PRD #5's only blocking
	// token check (D6b).
	var last8 string
	if len(f.token) >= 8 {
		last8 = f.token[len(f.token)-8:]
	}
	var matchedScopes []string
	matches := 0
	for page := 1; ; page++ {
		raw, err := f.rawGet(ctx, fmt.Sprintf("/users/%s/tokens?page=%d&limit=%d",
			url.PathEscape(u.UserName), page, forgejoPerPage))
		if err != nil {
			return TokenInfo{}, err // already redacted by rawGet
		}
		var tokens []forgejoAccessToken
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return TokenInfo{}, f.redact.error(fmt.Errorf("forgejo: parse tokens: %w", err))
		}
		for _, tk := range tokens {
			if tk.TokenLastEight == last8 {
				matches++
				matchedScopes = append([]string(nil), tk.Scopes...)
			}
		}
		if len(tokens) < forgejoPerPage {
			break
		}
		if page >= maxForgePages {
			return TokenInfo{}, f.redact.error(fmt.Errorf("forgejo: token info: %w", forgePaginationCapErr("page", maxForgePages)))
		}
	}
	if matches == 1 {
		// The unique, listed match authenticated this very request, so it is active.
		// Forgejo PATs report neither an active flag nor an expiry (the API has no such
		// fields — verified live on 16.0.0), so Active is true and ExpiresAt stays zero
		// ("never expires"). Scopes come back normalized and REORDERED (Forgejo
		// re-emits in canonical order, not mint order), which is why the privilege
		// checker compares them as an unordered set (D6b).
		return TokenInfo{Scopes: matchedScopes, Active: true}, nil
	}
	if matches > 1 {
		return TokenInfo{}, fmt.Errorf("forgejo: token info: %d tokens share the authenticating token's last-eight fingerprint; cannot uniquely identify its scopes", matches)
	}
	// 0 matches: the token authenticated but does not appear in its own owner's token
	// list. Surfaced as a generic error, which the privilege checker downgrades to a
	// "could not verify scopes" warning rather than a hard block.
	return TokenInfo{}, fmt.Errorf("forgejo: token info: the authenticating token was not found in its owner's token list")
}

// forgejoAccessToken is the subset of Forgejo's access-token payload uzi reads:
// the scopes, and token_last_eight to identify which listed token is the one
// authenticating the call. Forgejo emits no expiry or active field.
type forgejoAccessToken struct {
	TokenLastEight string   `json:"token_last_eight"`
	Scopes         []string `json:"scopes"`
}

// rawGet performs an authenticated GET against {baseURL}/api/v1{path} using the
// driver's shared timeout client, for the two endpoints uzi cannot drive through
// the gitea SDK (the issue timeline, whose SDK type mis-models the label field;
// and token introspection, whose SDK method imposes a client-side BasicAuth gate —
// both D5/M4). It returns the body and status. Every error is routed through the
// PAT redactor, including a non-2xx body, so a hostile forge echoing the token in
// an error cannot leak it (test #12).
func (f *forgejo) rawGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/api/v1"+path, nil)
	if err != nil {
		return nil, f.redact.error(fmt.Errorf("forgejo: build request: %w", err))
	}
	req.Header.Set("Authorization", "token "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, f.redact.error(fmt.Errorf("forgejo: request %s: %w", path, err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, f.redact.error(fmt.Errorf("forgejo: read response: %w", err))
	}
	if resp.StatusCode/100 != 2 {
		return body, f.redact.error(fmt.Errorf("forgejo: GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body))))
	}
	return body, nil
}

// rawGetLimited is rawGet's byte-bounded sibling: the response body is read through
// an io.LimitReader(resp.Body, limit) so the TRANSFER itself is capped, for the job
// log endpoint whose gitea SDK method (GetRepoActionJobLogs) buffers the whole body
// into memory with an unbounded io.ReadAll before the driver sees its size. A
// hostile forge streaming a multi-GB log body therefore cannot OOM the api: the read
// stops at limit bytes. Auth, non-2xx handling and PAT redaction match rawGet.
func (f *forgejo) rawGetLimited(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/api/v1"+path, nil)
	if err != nil {
		return nil, f.redact.error(fmt.Errorf("forgejo: build request: %w", err))
	}
	req.Header.Set("Authorization", "token "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, f.redact.error(fmt.Errorf("forgejo: request %s: %w", path, err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, f.redact.error(fmt.Errorf("forgejo: read response: %w", err))
	}
	if resp.StatusCode/100 != 2 {
		return body, f.redact.error(fmt.Errorf("forgejo: GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body))))
	}
	return body, nil
}
