package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// developerAccessLevel is GitLab's Developer role access level. The privilege
// checker requires the bot to sit at exactly this level, and a protected default
// branch must not admit push at or below it.
const developerAccessLevel = int(gitlab.DeveloperPermissions) // 30

// perPage is the pagination page size for every list call. 100 is GitLab's
// maximum, minimizing round-trips on busy projects.
const perPage = 100

// maxTraceBytes is the fail-closed ceiling on a single job trace JobLogTail will
// process. Well above any real CI log's tail need (the snapshot keeps only the
// last CI_FIX_LOG_TAIL_BYTES, 32 KiB), so a legitimate trace never trips it; a
// pathologically large one errors rather than being scanned/truncated.
const maxTraceBytes = 16 << 20 // 16 MiB

// gitLab is the GitLab REST driver. The embedded redactor scrubs the PAT out of
// every error the underlying client hands back before it leaves this package.
//
// baseURL/token/logClient are retained alongside the SDK client so JobLogTail
// can bypass the SDK and stream the job trace under an io.LimitReader: client-go's
// GetTraceFile buffers the whole trace into memory before returning it, so a raw
// bounded GET is the only way to cap the TRANSFER rather than a post-buffer copy.
//
// logClient is a dedicated client for that raw trace GET that REFUSES to follow
// redirects (CheckRedirect → http.ErrUseLastResponse). The SDK's own client
// follows up to 10 redirects, and Go does NOT strip the PRIVATE-TOKEN header on a
// cross-host redirect (only Authorization/Cookie/WWW-Authenticate), so a
// hostile-but-allowlisted forge answering the trace request with a cross-host 302
// would otherwise replay the bot PAT to the redirect target.
type gitLab struct {
	client    *gitlab.Client
	redact    redactor
	baseURL   string
	token     string
	logClient *http.Client
}

// newGitLab builds a GitLab driver against baseURL using token, with a bounded
// per-call HTTP timeout. The official client's retryable transport already
// honors 429 + Retry-After and backs off on 5xx; we route it through our
// timeout client so no call can hang forever (multica's untimeouted-client wart
// is avoided). baseURL is assumed allowlist-checked by the caller.
func newGitLab(baseURL, token string, timeout time.Duration) (*gitLab, error) {
	hc := timeoutClient(timeout)
	client, err := gitlab.NewClient(token,
		gitlab.WithBaseURL(baseURL),
		gitlab.WithHTTPClient(hc),
	)
	if err != nil {
		// NewClient failure can only stem from the base URL here; still route
		// it through a redactor in case the token ever appears.
		return nil, newRedactor(token).error(fmt.Errorf("gitlab: new client: %w", err))
	}
	// logClient is a separate client for JobLogTail's raw trace GET: it shares the
	// per-call timeout but REFUSES every redirect, so a cross-host 302 cannot replay
	// the PAT header to the redirect target (mirrors the GitHub driver's logClient).
	logClient := &http.Client{
		Timeout:       hc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &gitLab{
		client:    client,
		redact:    newRedactor(token),
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		token:     token,
		logClient: logClient,
	}, nil
}

func (g *gitLab) VerifyToken(ctx context.Context) (BotIdentity, error) {
	u, _, err := g.client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		return BotIdentity{}, g.redact.error(fmt.Errorf("gitlab: verify token: %w", err))
	}
	// IsAdmin rides on the same GET /user response VerifyToken already makes:
	// GitLab only returns is_admin:true for an admin caller, and omits it for a
	// regular user, so the decoded bool is false for every compliant bot.
	return BotIdentity{ForgeUserID: u.ID, Username: u.Username, IsAdmin: u.IsAdmin}, nil
}

func (g *gitLab) TokenInfo(ctx context.Context) (TokenInfo, error) {
	t, resp, err := g.client.PersonalAccessTokens.GetSinglePersonalAccessToken(gitlab.WithContext(ctx))
	if err != nil {
		// A 404 here means the endpoint itself is absent (GitLab < 15.5), not that
		// the token is bad — surface the distinct sentinel so the checker warns
		// rather than blocks. The sentinel carries no token material.
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return TokenInfo{}, ErrTokenIntrospectionUnsupported
		}
		return TokenInfo{}, g.redact.error(fmt.Errorf("gitlab: token info: %w", err))
	}
	info := TokenInfo{
		Scopes: append([]string(nil), t.Scopes...),
		Active: t.Active && !t.Revoked,
	}
	if t.ExpiresAt != nil {
		info.ExpiresAt = time.Time(*t.ExpiresAt)
	}
	return info, nil
}

// roleForAccessLevel maps a GitLab access level onto the neutral Role. GitLab's
// levels are 0 (none), 5 (Minimal), 10 (Guest), 20 (Reporter), 30 (Developer),
// 40 (Maintainer), 50 (Owner), 60 (Admin). Developer is exactly the write role —
// it is the level uzi's bot must hold — so everything under it collapses to read
// and everything over it to admin/owner. The precision uzi loses (Guest vs
// Reporter) is precision it never used: privcheck asserts write and reports
// anything else.
func roleForAccessLevel(lvl int) Role {
	switch {
	case lvl <= 0:
		return RoleNone
	case lvl < developerAccessLevel:
		return RoleRead
	case lvl == developerAccessLevel:
		return RoleWrite
	case lvl < int(gitlab.OwnerPermissions):
		return RoleAdmin
	default:
		return RoleOwner
	}
}

func (g *gitLab) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (Role, bool, error) {
	// members/all resolves EFFECTIVE membership (direct + inherited group), which
	// is what actually governs what the bot can do — a group-inherited Maintainer
	// role would be invisible to the direct-members endpoint.
	m, resp, err := g.client.ProjectMembers.GetInheritedProjectMember(projectID, forgeUserID, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// Not a member (removed or demoted below any membership after the repo
			// was enabled). Reported as a finding by the checker, not an error.
			return RoleNone, false, nil
		}
		return RoleNone, false, g.redact.error(fmt.Errorf("gitlab: project role: %w", err))
	}
	return roleForAccessLevel(int(m.AccessLevel)), true, nil
}

func (g *gitLab) DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error) {
	pb, resp, err := g.client.ProtectedBranches.GetProtectedBranch(projectID, branch, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// No protection rule for this branch at all — so nothing restricts push
			// or merge, and a write-role bot may do both. Reporting the zero value
			// here would say "cannot push, cannot merge" about the one branch state
			// where the bot certainly can: see BranchProtection's invariant.
			return BranchProtection{
				Protected:         false,
				WriteRoleCanPush:  true,
				WriteRoleCanMerge: true,
			}, nil
		}
		return BranchProtection{}, g.redact.error(fmt.Errorf("gitlab: branch protection: %w", err))
	}
	bp := BranchProtection{Protected: true}
	for _, pl := range pb.PushAccessLevels {
		lvl := int(pl.AccessLevel)
		// A push level of 0 is "No one"; >= Maintainer (40) excludes a Developer
		// bot. Only a nonzero level at or below Developer (30) lets the bot push.
		if lvl > 0 && lvl <= developerAccessLevel {
			bp.WriteRoleCanPush = true
		}
		// A per-user allow-to-push entry naming the bot lets it push regardless of
		// role (a false negative the role check alone would miss).
		if botUserID != 0 && pl.UserID == botUserID {
			bp.BotCanPush = true
		}
	}
	// merge_access_levels is the same shape as push_access_levels and governs who
	// may merge an MR into the branch. GitLab's initial default for a new project
	// sets it to Maintainer, so a Developer bot cannot merge — but it is a
	// setting, and safe-by-default is not safe. uzi modelled merge on no forge
	// until now, delegating "the agent can only ever open an MR" to a sentence in
	// the setup docs.
	for _, ml := range pb.MergeAccessLevels {
		lvl := int(ml.AccessLevel)
		if lvl > 0 && lvl <= developerAccessLevel {
			bp.WriteRoleCanMerge = true
		}
		if botUserID != 0 && ml.UserID == botUserID {
			bp.BotCanMerge = true
		}
	}
	return bp, nil
}

func (g *gitLab) ListProjects(ctx context.Context) ([]Project, error) {
	opt := &gitlab.ListProjectsOptions{
		ListOptions:    gitlab.ListOptions{Page: 1, PerPage: perPage},
		Membership:     gitlab.Ptr(true),
		MinAccessLevel: gitlab.Ptr(gitlab.DeveloperPermissions),
		// Simple trims the payload to the fields we map; we don't need the
		// heavy statistics/permissions blocks.
		Simple: gitlab.Ptr(true),
	}
	var out []Project
	page := 0
	for {
		page++
		projects, resp, err := g.client.Projects.ListProjects(opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list projects: %w", err))
		}
		for _, p := range projects {
			if p == nil {
				continue
			}
			out = append(out, Project{
				ForgeProjectID:    p.ID,
				PathWithNamespace: p.PathWithNamespace,
				WebURL:            p.WebURL,
				DefaultBranch:     p.DefaultBranch,
			})
		}
		if len(out) > maxForgeItems {
			return nil, g.redact.error(fmt.Errorf("gitlab: list projects: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(fmt.Errorf("gitlab: list projects: %w", forgePaginationCapErr("page", maxForgePages)))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	opt := &gitlab.ListLabelsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	var out []Label
	page := 0
	for {
		page++
		labels, resp, err := g.client.Labels.ListLabels(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list labels: %w", err))
		}
		for _, l := range labels {
			if l == nil {
				continue
			}
			out = append(out, Label{Name: l.Name, Color: l.Color})
		}
		if len(out) > maxForgeItems {
			return nil, g.redact.error(fmt.Errorf("gitlab: list labels: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(fmt.Errorf("gitlab: list labels: %w", forgePaginationCapErr("page", maxForgePages)))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) EnsureLabels(ctx context.Context, projectID int64, labels []Label) error {
	existing, err := g.ListLabels(ctx, projectID)
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
		opt := &gitlab.CreateLabelOptions{Name: gitlab.Ptr(l.Name)}
		if l.Color != "" {
			opt.Color = gitlab.Ptr(l.Color)
		}
		if _, _, err := g.client.Labels.CreateLabel(projectID, opt, gitlab.WithContext(ctx)); err != nil {
			return g.redact.error(fmt.Errorf("gitlab: create label %q: %w", l.Name, err))
		}
	}
	return nil
}

func (g *gitLab) ListIssues(ctx context.Context, projectID int64, opts ListIssuesOptions) ([]Issue, error) {
	opt := &gitlab.ListProjectIssuesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
		// state=all remains the DEFAULT (opts.State's zero value): the Closed column
		// and de-label/close eviction both depend on seeing closed issues, so every
		// pre-M6 caller must keep getting it.
		//
		// GitLab's REQUEST vocabulary is opened/closed/all, which happens to be
		// identical to uzi's neutral vocabulary — so this driver needs no translation
		// and the mapping below is a pass-through. That coincidence is exactly why a
		// GitLab-shaped fake cannot catch a driver that fails to translate; see the
		// Forgejo driver, where the vocabularies differ.
		State: gitlab.Ptr(gitlabIssueStateParam(opts.State)),
	}
	if len(opts.Labels) > 0 {
		labels := gitlab.LabelOptions(opts.Labels)
		opt.Labels = &labels
	}
	if opts.UpdatedAfter != nil {
		opt.UpdatedAfter = opts.UpdatedAfter
	}
	var out []Issue
	page := 0
	for {
		page++
		issues, resp, err := g.client.Issues.ListProjectIssues(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issues: %w", err))
		}
		for _, i := range issues {
			if i == nil {
				continue
			}
			out = append(out, toIssue(i))
			// opts.Limit == 0 is the no-cap default (every pre-#158 caller); a positive
			// Limit stops as soon as that many issues are collected, truncating this page.
			if opts.Limit > 0 && len(out) >= opts.Limit {
				return out, nil
			}
		}
		if len(out) > maxForgeItems {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issues: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issues: %w", forgePaginationCapErr("page", maxForgePages)))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) GetIssue(ctx context.Context, projectID, issueIID int64) (Issue, error) {
	i, _, err := g.client.Issues.GetIssue(projectID, issueIID, gitlab.WithContext(ctx))
	if err != nil {
		return Issue{}, g.redact.error(fmt.Errorf("gitlab: get issue: %w", err))
	}
	return toIssue(i), nil
}

func (g *gitLab) CreateIssue(ctx context.Context, projectID int64, title, description string, labels []string) (Issue, error) {
	opt := &gitlab.CreateIssueOptions{
		Title:       gitlab.Ptr(title),
		Description: gitlab.Ptr(description),
	}
	if len(labels) > 0 {
		l := gitlab.LabelOptions(labels)
		opt.Labels = &l
	}
	i, _, err := g.client.Issues.CreateIssue(projectID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return Issue{}, g.redact.error(fmt.Errorf("gitlab: create issue: %w", err))
	}
	return toIssue(i), nil
}

func (g *gitLab) UpdateIssueLabels(ctx context.Context, projectID, issueIID int64, add, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	opt := &gitlab.UpdateIssueOptions{}
	if len(add) > 0 {
		l := gitlab.LabelOptions(add)
		opt.AddLabels = &l
	}
	if len(remove) > 0 {
		l := gitlab.LabelOptions(remove)
		opt.RemoveLabels = &l
	}
	if _, _, err := g.client.Issues.UpdateIssue(projectID, issueIID, opt, gitlab.WithContext(ctx)); err != nil {
		return g.redact.error(fmt.Errorf("gitlab: update issue labels: %w", err))
	}
	return nil
}

// UpdateIssueDescription sends only the description (PRD #72 M5).
// gitlab.UpdateIssueOptions.Description is a *string with `omitempty`, so no other
// field of the issue is transmitted and nothing else can be clobbered.
func (g *gitLab) UpdateIssueDescription(ctx context.Context, projectID, issueIID int64, description string) error {
	opt := &gitlab.UpdateIssueOptions{Description: &description}
	if _, _, err := g.client.Issues.UpdateIssue(projectID, issueIID, opt, gitlab.WithContext(ctx)); err != nil {
		return g.redact.error(fmt.Errorf("gitlab: update issue description: %w", err))
	}
	return nil
}

func (g *gitLab) UserExists(ctx context.Context, username string) (bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return false, nil
	}
	// GitLab's username filter is an exact match, so a non-empty result means the
	// account exists. We only need to know if any row came back.
	users, _, err := g.client.Users.ListUsers(&gitlab.ListUsersOptions{
		Username: gitlab.Ptr(username),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return false, g.redact.error(fmt.Errorf("gitlab: lookup user: %w", err))
	}
	return len(users) > 0, nil
}

func (g *gitLab) ListIssueLabelEvents(ctx context.Context, projectID, issueIID int64) ([]LabelEvent, error) {
	opt := &gitlab.ListLabelEventsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	var out []LabelEvent
	page := 0
	for {
		page++
		events, resp, err := g.client.ResourceLabelEvents.ListIssueLabelEvents(projectID, issueIID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issue label events: %w", err))
		}
		for _, e := range events {
			if e == nil {
				continue
			}
			out = append(out, toLabelEvent(e))
		}
		if len(out) > maxForgeItems {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issue label events: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issue label events: %w", forgePaginationCapErr("page", maxForgePages)))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) CreateIssueNote(ctx context.Context, projectID, issueIID int64, body string) (IssueNote, error) {
	note, _, err := g.client.Notes.CreateIssueNote(projectID, issueIID, &gitlab.CreateIssueNoteOptions{
		Body: gitlab.Ptr(body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return IssueNote{}, g.redact.error(fmt.Errorf("gitlab: create issue note: %w", err))
	}
	return IssueNote{ID: note.ID, Body: note.Body}, nil
}

func (g *gitLab) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	mr, _, err := g.client.MergeRequests.GetMergeRequest(projectID, mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return MergeRequest{}, g.redact.error(fmt.Errorf("gitlab: get merge request: %w", err))
	}
	return toMergeRequest(mr), nil
}

func (g *gitLab) LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error) {
	// per_page=1 + the default order_by=id,sort=desc means the single returned row
	// is the newest BRANCH pipeline for the ref. Detached MR pipelines never appear
	// here (they live under refs/merge-requests/:iid/head) — LatestMRPipeline covers
	// those; a ref with no branch pipeline yields an empty list → ErrNoPipeline.
	opt := &gitlab.ListProjectPipelinesOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: 1},
		Ref:         gitlab.Ptr(ref),
	}
	pipelines, _, err := g.client.Pipelines.ListProjectPipelines(projectID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return Pipeline{}, g.redact.error(fmt.Errorf("gitlab: latest pipeline: %w", err))
	}
	// A hostile forge could return a one-element list whose single entry is a JSON
	// null, which decodes to a nil *PipelineInfo that passes len==0 but panics on
	// deref; reject it as "no pipeline".
	if len(pipelines) == 0 || pipelines[0] == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toPipeline(pipelines[0]), nil
}

func (g *gitLab) LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error) {
	// GitLab does NOT order an MR's pipelines by a simple id-desc: merge_request_event
	// pipelines are grouped FIRST, then id-desc within each group (Ci::
	// PipelinesForMergeRequestFinder). So pipelines[0] is the newest MR-EVENT pipeline
	// if any exist — which, on a project running BOTH push and MR-event pipelines, can
	// be OLDER by id than a later push pipeline. We therefore select the MAX BY ID, so
	// "latest" is unambiguous and the verification guard (observed id > snapshot id)
	// can never miss a newer pipeline. This still catches detached and merged-results
	// pipelines the branch-ref query misses.
	pipelines, _, err := g.client.MergeRequests.ListMergeRequestPipelines(projectID, mrIID, gitlab.WithContext(ctx))
	if err != nil {
		return Pipeline{}, g.redact.error(fmt.Errorf("gitlab: latest MR pipeline: %w", err))
	}
	// Skip nil elements: a hostile forge could return a null entry in the list,
	// which decodes to a nil *PipelineInfo and would panic the max-by-id scan.
	var newest *gitlab.PipelineInfo
	for _, p := range pipelines {
		if p == nil {
			continue
		}
		if newest == nil || p.ID > newest.ID {
			newest = p
		}
	}
	if newest == nil {
		return Pipeline{}, ErrNoPipeline
	}
	return toPipeline(newest), nil
}

func (g *gitLab) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	opt := &gitlab.ListJobsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	var out []Job
	page := 0
	for {
		page++
		jobs, resp, err := g.client.Jobs.ListPipelineJobs(projectID, pipelineID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list pipeline jobs: %w", err))
		}
		for _, j := range jobs {
			if j == nil {
				continue
			}
			out = append(out, Job{ID: j.ID, Name: j.Name, Stage: j.Stage, Status: j.Status, WebURL: j.WebURL})
		}
		if len(out) > maxForgeItems {
			return nil, g.redact.error(fmt.Errorf("gitlab: list pipeline jobs: %w", forgePaginationCapErr("item", maxForgeItems)))
		}
		if resp.NextPage == 0 {
			break
		}
		if page >= maxForgePages {
			return nil, g.redact.error(fmt.Errorf("gitlab: list pipeline jobs: %w", forgePaginationCapErr("page", maxForgePages)))
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error) {
	// The trace endpoint streams the WHOLE log (no server-side range/tail), so we
	// download it and keep only the last maxBytes. We bypass the SDK's GetTraceFile,
	// which buffers the entire trace into memory before returning it: a raw GET read
	// through an io.LimitReader byte-bounds the TRANSFER itself, so a hostile forge
	// streaming a multi-GB body cannot OOM the api before the ceiling check fires.
	// The request goes through g.logClient, which REFUSES redirects: Go does not
	// strip the PRIVATE-TOKEN header on a cross-host redirect, so a hostile forge
	// answering with a 302 to another host would otherwise replay the bot PAT there;
	// with http.ErrUseLastResponse the 302 surfaces here as a non-2xx and is refused
	// before any body read. The tail runs through the PAT redactor: a hostile pipeline
	// could echo the bot's own token into its log, and it must not survive into a snapshot.
	url := g.baseURL + "/api/v4/projects/" + strconv.FormatInt(projectID, 10) + "/jobs/" + strconv.FormatInt(jobID, 10) + "/trace"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", g.redact.error(fmt.Errorf("gitlab: job log tail: build request: %w", err))
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	resp, err := g.logClient.Do(req)
	if err != nil {
		return "", g.redact.error(fmt.Errorf("gitlab: job log tail: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", g.redact.error(fmt.Errorf("gitlab: job log tail: status %d", resp.StatusCode))
	}
	// Fail closed past a hard ceiling. The LimitReader bounds the transfer to
	// maxTraceBytes+1 bytes, so the read stops long before a pathological body is
	// fully received; the ceiling check below then errors rather than returning it.
	// The returned tail is separately capped to maxBytes.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTraceBytes+1))
	if err != nil {
		return "", g.redact.error(fmt.Errorf("gitlab: read job trace: %w", err))
	}
	if len(data) > maxTraceBytes {
		return "", g.redact.error(fmt.Errorf("gitlab: job %d trace exceeds the %d-byte ceiling", jobID, maxTraceBytes))
	}
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		// The byte cut may land mid-rune; drop leading UTF-8 continuation bytes so
		// the returned tail stays valid UTF-8 (a log tail need not be exact to the
		// byte, and "at most maxBytes" tolerates dropping a few).
		for len(data) > 0 && data[0]&0xC0 == 0x80 {
			data = data[1:]
		}
	}
	return g.redact.string(string(data)), nil
}

// ProjectCIConfigPath returns the project's configured ci_config_path (empty means
// the GitLab default, .gitlab-ci.yml). GitLab: GET /projects/:id. The value is a
// repo-relative path/glob, never a secret; any error is PAT-redacted like the rest.
func (g *gitLab) ProjectCIConfigPath(ctx context.Context, projectID int64) (string, error) {
	p, _, err := g.client.Projects.GetProject(int(projectID), nil, gitlab.WithContext(ctx))
	if err != nil {
		return "", g.redact.error(fmt.Errorf("gitlab: get project: %w", err))
	}
	return p.CIConfigPath, nil
}

// toPipeline maps a client-go PipelineInfo (the list-endpoint shape returned by
// both ListProjectPipelines and ListMergeRequestPipelines) to the neutral domain
// type. Nil timestamps yield the zero time, which the cache stores as NULL.
func toPipeline(p *gitlab.PipelineInfo) Pipeline {
	pl := Pipeline{
		ID:     p.ID,
		Ref:    p.Ref,
		SHA:    p.SHA,
		Status: p.Status,
		WebURL: p.WebURL,
	}
	if p.CreatedAt != nil {
		pl.CreatedAt = *p.CreatedAt
	}
	if p.UpdatedAt != nil {
		pl.UpdatedAt = *p.UpdatedAt
	}
	return pl
}

// toLabelEvent maps a client-go resource label event to the neutral domain type.
// A nil CreatedAt yields the zero time; a system event with no user yields an
// empty Username (the caller falls back to the issue author in that case).
func toLabelEvent(e *gitlab.LabelEvent) LabelEvent {
	le := LabelEvent{
		ID:        e.ID,
		Action:    e.Action,
		LabelName: e.Label.Name,
		Username:  e.User.Username,
	}
	if e.CreatedAt != nil {
		le.CreatedAt = *e.CreatedAt
	}
	return le
}

// toMergeRequest maps a client-go merge request to the neutral domain type. The
// State field is one of the MRState* constants (opened|closed|merged|locked).
func toMergeRequest(mr *gitlab.MergeRequest) MergeRequest {
	return MergeRequest{
		IID:    mr.IID,
		State:  mr.State,
		WebURL: mr.WebURL,
	}
}

// toIssue maps a client-go issue to the neutral domain type. A nil author (rare
// but possible for system issues) yields an empty Author; a nil UpdatedAt
// yields the zero time, which the sync engine treats as "no HWM advance".
//
// Labels is normalized to a non-nil slice. GitLab returns no labels array at all
// for an issue carrying none, so []string(i.Labels) is nil, which json.Marshal
// writes into the cache as the jsonb scalar `null` rather than `[]` — a value the
// column's NOT NULL does not exclude and that decodes back to nil without error.
// This is the belt; handler.decodeLabels is the braces, and it is the one that
// matters for rows already stored.
//
// Unreachable until PRD #102 M6, whose additive fetch is the first thing that
// caches an issue with no labels at all.
func toIssue(i *gitlab.Issue) Issue {
	labels := []string(i.Labels)
	if labels == nil {
		labels = []string{}
	}
	issue := Issue{
		IID:         i.IID,
		Title:       i.Title,
		State:       i.State,
		Labels:      labels,
		Description: i.Description,
		WebURL:      i.WebURL,
	}
	if i.Author != nil {
		issue.Author = i.Author.Username
	}
	if i.UpdatedAt != nil {
		issue.UpdatedAt = *i.UpdatedAt
	}
	return issue
}

// gitlabIssueStateParam maps the neutral state onto GitLab's `state` query
// parameter. The two vocabularies coincide (opened/closed/all), so this is a
// pass-through with an explicit default — written out rather than inlined so the
// Forgejo driver's genuine translation has a visible counterpart, and so an
// unrecognised value can never reach the API as a silent filter.
func gitlabIssueStateParam(s IssueState) string {
	switch s {
	case StateOpened:
		return "opened"
	case StateClosed:
		return "closed"
	default:
		return "all"
	}
}
