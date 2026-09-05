package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// forgejoPatchErrBodyLimit caps how much of a non-2xx PATCH response patchIssue
// reads: the body is only ever folded into a redacted error message, never
// parsed, so a modest ceiling is enough and keeps a hostile forge's error body
// byte-bounded (same reasoning as rawGetLimited's LimitReader).
const forgejoPatchErrBodyLimit = 1 << 20

// patchIssue PATCHes a single issue with a caller-supplied struct, marshalling
// ONLY the fields that struct carries. It exists to sidestep the gitea SDK's
// gitea.EditIssueOption, whose Title field is a plain `string` tagged
// `json:"title"` with NO `omitempty`: marshalling that struct ALWAYS emits a
// "title" key, so any edit that goes through it round-trips (and can clobber) the
// title, and an empty Title wipes it outright. A field-only PATCH omits "title"
// entirely, so a field the caller does not send — including a title edited
// concurrently by someone else — survives untouched (no read, no TOCTOU).
//
// Auth and redaction mirror rawGetLimited: the shared timeout client, the
// "token " Authorization header, an io.LimitReader-bounded body read, and a
// non-2xx error routed through f.wrapErr so a PAT echoed in the error body is
// still redacted.
func (f *forgejo) patchIssue(ctx context.Context, slug repoSlug, issueIID int64, op string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return f.wrapErr(op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		f.baseURL+"/api/v1"+fmt.Sprintf("/repos/%s/%s/issues/%d", slug.owner, slug.repo, issueIID),
		bytes.NewReader(body))
	if err != nil {
		return f.wrapErr(op, err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return f.wrapErr(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, forgejoPatchErrBodyLimit))
	if err != nil {
		return f.wrapErr(op, err)
	}
	if resp.StatusCode/100 != 2 {
		return f.wrapErr(op, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
	}
	return nil
}
