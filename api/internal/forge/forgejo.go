package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
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
// color. uzi's callers always pin one (board columns, PromoteLabelColor), but
// Forgejo's CreateLabel rejects an empty color outright (its client-side
// validator requires a 6-hex value). GitLab's label-create API likewise requires
// a color, so all three drivers supply a neutral default rather than fail a
// label create.
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
		return nil, f.wrapErr("new client", err)
	}
	return c, nil
}

// wrapErr adds op context and routes the error through the PAT redactor so no
// error this driver surfaces can carry the token. A nil error passes through as
// nil. (No rate-limit classification: the gitea SDK never surfaces a typed
// rate-limit error, so there is no equivalent to github's errors.As branch.)
func (f *forgejo) wrapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return f.redact.error(fmt.Errorf("forgejo: %s: %w", op, err))
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
		return repoSlug{}, f.wrapErr(fmt.Sprintf("resolve repo %d", projectID), err)
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
	wrap := func(e error) error { return f.wrapErr("list projects", e) }
	return paginate(wrap, func(page int) ([]Project, int, error) {
		opt.Page = page
		repos, resp, err := c.ListMyRepos(opt)
		if err != nil {
			return nil, 0, err
		}
		var items []Project
		for _, r := range repos {
			if r == nil || r.Permissions == nil || !r.Permissions.Push {
				continue
			}
			items = append(items, Project{
				ForgeProjectID:    r.ID,
				PathWithNamespace: r.FullName,
				WebURL:            r.HTMLURL,
				DefaultBranch:     r.DefaultBranch,
			})
		}
		return items, resp.NextPage, nil
	})
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
			return nil, f.wrapErr("list issues", err)
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
			return nil, f.wrapErr("list issues", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list issues", forgePaginationCapErr("page", maxForgePages))
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
		return Issue{}, f.wrapErr("get issue", err)
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
		return f.wrapErr("update issue description: read current issue", err)
	}
	if _, _, err := c.EditIssue(slug.owner, slug.repo, issueIID, gitea.EditIssueOption{
		Title: cur.Title,
		Body:  &description,
	}); err != nil {
		return f.wrapErr("update issue description", err)
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
		return Issue{}, f.wrapErr("create issue", err)
	}
	return toForgejoIssue(i), nil
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
		return false, f.wrapErr("lookup user", err)
	}
	return true, nil
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
		Assignees:   []int64{},
	}
	for _, l := range i.Labels {
		if l != nil {
			issue.Labels = append(issue.Labels, l.Name)
		}
	}
	for _, a := range i.Assignees {
		if a != nil {
			issue.Assignees = append(issue.Assignees, a.ID)
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
		return MergeRequest{}, f.wrapErr("get merge request", err)
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
		return IssueNote{}, f.wrapErr("create issue note", err)
	}
	return IssueNote{ID: note.ID, Body: note.Body}, nil
}

// ListMergeRequestComments returns a PR's human + review-bot comments oldest-first
// (PRD #700). Forgejo/Gitea splits MR feedback across two surfaces, so the driver
// reads both (mirroring the existing ListIssueComments / pull-review calls):
//   - ListIssueComments on the PR index → the top-level conversation notes (summary
//     state, no diff anchor). A PR shares its issue index, so this is the same call
//     ListIssueComments uses for issues.
//   - ListPullReviews → each review's summary Body (non-empty only), and
//     ListPullReviewComments per review → the inline diff comments (Path/Line,
//     ReplyID = the review-comment id, HeadSHA from the comment/review commit id).
//
// ResolveID stays EMPTY on every comment: Forgejo has no resolvable-thread concept
// this feature uses (resolve is a documented no-op — see ResolveMergeRequestThread).
// Gitea's comment/review endpoints return no forge system notes, so no D2 filter is
// needed; the driver sorts the merged list by CreatedAt for oldest-first (D8).
func (f *forgejo) ListMergeRequestComments(ctx context.Context, projectID, mrIID int64) ([]MRComment, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}

	var out []MRComment

	// Top-level conversation notes (the PR shares its issue index).
	icOpt := gitea.ListIssueCommentOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	for page := 0; ; {
		page++
		comments, resp, err := c.ListIssueComments(slug.owner, slug.repo, mrIID, icOpt)
		if err != nil {
			return nil, f.wrapErr("list merge request comments", err)
		}
		for _, cm := range comments {
			if cm == nil {
				continue
			}
			mc := MRComment{ID: cm.ID, Body: cm.Body, CreatedAt: cm.Created, ReviewState: ReviewCommentSummary}
			if cm.Poster != nil {
				mc.AuthorForgeUserID = cm.Poster.ID
				mc.AuthorUsername = cm.Poster.UserName
			}
			out = append(out, mc)
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		icOpt.Page = resp.NextPage
	}

	// Pull reviews: a non-empty Body is a review-summary comment; each review's
	// inline comments carry the diff anchor and the reply id.
	revOpt := gitea.ListPullReviewsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	for page := 0; ; {
		page++
		reviews, resp, err := c.ListPullReviews(slug.owner, slug.repo, mrIID, revOpt)
		if err != nil {
			return nil, f.wrapErr("list merge request comments", err)
		}
		for _, r := range reviews {
			if r == nil {
				continue
			}
			if r.Body != "" {
				mc := MRComment{ID: r.ID, Body: r.Body, CreatedAt: r.Submitted, HeadSHA: r.CommitID, ReviewState: ReviewCommentSummary}
				if r.Reviewer != nil {
					mc.AuthorForgeUserID = r.Reviewer.ID
					mc.AuthorUsername = r.Reviewer.UserName
				}
				out = append(out, mc)
			}
			reviewComments, _, err := c.ListPullReviewComments(slug.owner, slug.repo, mrIID, r.ID)
			if err != nil {
				return nil, f.wrapErr("list merge request comments", err)
			}
			for _, rc := range reviewComments {
				if rc == nil {
					continue
				}
				mc := MRComment{
					ID:          rc.ID,
					Body:        rc.Body,
					CreatedAt:   rc.Created,
					HeadSHA:     rc.CommitID,
					ReplyID:     strconv.FormatInt(rc.ID, 10),
					ReviewState: ReviewCommentInline,
				}
				if rc.Reviewer != nil {
					mc.AuthorForgeUserID = rc.Reviewer.ID
					mc.AuthorUsername = rc.Reviewer.UserName
				}
				if rc.Path != "" {
					path := rc.Path
					mc.Path = &path
				}
				if rc.LineNum != 0 {
					line := int(rc.LineNum) //nolint:gosec // G115: a review-comment line number from the forge; cannot realistically exceed int range
					mc.Line = &line
				}
				out = append(out, mc)
			}
		}
		if len(out) > maxForgeItems {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("item", maxForgeItems))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, f.wrapErr("list merge request comments", forgePaginationCapErr("page", maxForgePages))
		}
		revOpt.Page = resp.NextPage
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ReplyMergeRequestComment posts an in-thread reply to the review comment
// identified by replyID (a review-comment id from ListMergeRequestComments).
func (f *forgejo) ReplyMergeRequestComment(ctx context.Context, projectID, mrIID int64, replyID, body string) error {
	c, err := f.newClient(ctx)
	if err != nil {
		return err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return err
	}
	commentID, err := strconv.ParseInt(strings.TrimSpace(replyID), 10, 64)
	if err != nil {
		return fmt.Errorf("forgejo: reply merge request comment: invalid reply id %q", replyID)
	}
	if _, _, err := c.CreatePullReviewCommentReply(slug.owner, slug.repo, mrIID, commentID,
		gitea.CreatePullReviewCommentReplyOptions{Body: body}); err != nil {
		return f.wrapErr("reply merge request comment", err)
	}
	return nil
}

// ResolveMergeRequestThread is a documented no-op on Forgejo: per PRD #700 this
// driver is reply-only and never resolves a thread, so it returns
// ErrResolveUnsupported for the worker to swallow. MRComment.ResolveID is
// correspondingly always empty on Forgejo, so a caller keyed on it never reaches
// here with a real anchor; the sentinel also covers a caller that tries anyway.
func (f *forgejo) ResolveMergeRequestThread(context.Context, int64, int64, string) error {
	return ErrResolveUnsupported
}

// ListIssueComments returns an issue's human comments, oldest-first (PRD #381).
// Gitea's ListIssueComments endpoint returns human comments only — system/timeline
// events live on a separate endpoint — so no in-SDK System filter exists or is
// needed (D2), and the list is already oldest-first (D8). Poster can be nil for a
// comment imported without a mapped user; guard it.
func (f *forgejo) ListIssueComments(ctx context.Context, projectID, issueIID int64) ([]IssueComment, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return nil, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return nil, err
	}
	opt := gitea.ListIssueCommentOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: forgejoPerPage}}
	wrap := func(e error) error { return f.wrapErr("list issue comments", e) }
	return paginate(wrap, func(page int) ([]IssueComment, int, error) {
		opt.Page = page
		comments, resp, err := c.ListIssueComments(slug.owner, slug.repo, issueIID, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []IssueComment
		for _, cm := range comments {
			if cm == nil {
				continue
			}
			ic := IssueComment{Body: cm.Body, CreatedAt: cm.Created}
			if cm.Poster != nil {
				ic.AuthorForgeUserID = cm.Poster.ID
				ic.AuthorUsername = cm.Poster.UserName
			}
			items = append(items, ic)
		}
		return items, resp.NextPage, nil
	})
}

// rawGetLimited is THE raw-GET helper for the three endpoints uzi cannot drive
// through the gitea SDK: the issue timeline (whose SDK type mis-models the label
// field), token introspection (whose SDK method imposes a client-side BasicAuth
// gate — both D5/M4), and the job log endpoint (whose SDK method
// GetRepoActionJobLogs buffers the whole body into memory before the driver sees
// its size). It performs an authenticated GET against {baseURL}/api/v1{path} using
// the driver's shared timeout client and reads the response body through an
// io.LimitReader(resp.Body, limit) so the TRANSFER itself is byte-bounded: a
// hostile forge streaming a multi-GB body therefore cannot OOM the api, as the read
// stops at limit bytes. Every error is routed through the PAT redactor, including a
// non-2xx body, so a hostile forge echoing the token in an error cannot leak it
// (test #12).
func (f *forgejo) rawGetLimited(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/api/v1"+path, nil)
	if err != nil {
		return nil, f.wrapErr("build request", err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, f.wrapErr(fmt.Sprintf("request %s", path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, f.wrapErr("read response", err)
	}
	if resp.StatusCode/100 != 2 {
		return body, f.wrapErr(fmt.Sprintf("GET %s", path), fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	return body, nil
}
