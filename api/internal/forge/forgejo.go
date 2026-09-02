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
