package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

func (g *github) ListProjects(ctx context.Context) ([]Project, error) {
	opt := &gh.RepositoryListByAuthenticatedUserOptions{
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	}
	wrap := func(e error) error { return g.wrapErr("list projects", e) }
	return paginate(wrap, func(page int) ([]Project, int, error) {
		opt.Page = page
		repos, resp, err := g.client.Repositories.ListByAuthenticatedUser(ctx, opt)
		if err != nil {
			return nil, 0, err
		}
		var items []Project
		for _, r := range repos {
			// GitHub's list has no min-permission filter, so a read-only repo comes
			// back and must be dropped client-side: the picker only offers repos the
			// bot can actually push to (D-item 2). GetPermissions/GetPush are nil-safe.
			if r == nil || !r.GetPermissions().GetPush() {
				continue
			}
			items = append(items, Project{
				ForgeProjectID:    r.GetID(),
				PathWithNamespace: r.GetFullName(),
				WebURL:            r.GetHTMLURL(),
				DefaultBranch:     r.GetDefaultBranch(),
			})
		}
		return items, resp.NextPage, nil
	})
}
