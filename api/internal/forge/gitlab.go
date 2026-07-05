package forge

import (
	"context"
	"fmt"
	"net/http"
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

// gitLab is the GitLab REST driver. The embedded redactor scrubs the PAT out of
// every error the underlying client hands back before it leaves this package.
type gitLab struct {
	client *gitlab.Client
	redact redactor
}

// newGitLab builds a GitLab driver against baseURL using token, with a bounded
// per-call HTTP timeout. The official client's retryable transport already
// honors 429 + Retry-After and backs off on 5xx; we route it through our
// timeout client so no call can hang forever (multica's untimeouted-client wart
// is avoided). baseURL is assumed allowlist-checked by the caller.
func newGitLab(baseURL, token string, timeout time.Duration) (*gitLab, error) {
	client, err := gitlab.NewClient(token,
		gitlab.WithBaseURL(baseURL),
		gitlab.WithHTTPClient(timeoutClient(timeout)),
	)
	if err != nil {
		// NewClient failure can only stem from the base URL here; still route
		// it through a redactor in case the token ever appears.
		return nil, newRedactor(token).error(fmt.Errorf("gitlab: new client: %w", err))
	}
	return &gitLab{client: client, redact: newRedactor(token)}, nil
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

func (g *gitLab) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (int, bool, error) {
	// members/all resolves EFFECTIVE membership (direct + inherited group), which
	// is what actually governs what the bot can do — a group-inherited Maintainer
	// role would be invisible to the direct-members endpoint.
	m, resp, err := g.client.ProjectMembers.GetInheritedProjectMember(projectID, forgeUserID, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// Not a member (removed or demoted below any membership after the repo
			// was enabled). Reported as a finding by the checker, not an error.
			return 0, false, nil
		}
		return 0, false, g.redact.error(fmt.Errorf("gitlab: project role: %w", err))
	}
	return int(m.AccessLevel), true, nil
}

func (g *gitLab) DefaultBranchProtection(ctx context.Context, projectID int64, branch string) (BranchProtection, error) {
	pb, resp, err := g.client.ProtectedBranches.GetProtectedBranch(projectID, branch, gitlab.WithContext(ctx))
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			// No protection rule for this branch at all.
			return BranchProtection{Protected: false}, nil
		}
		return BranchProtection{}, g.redact.error(fmt.Errorf("gitlab: branch protection: %w", err))
	}
	bp := BranchProtection{Protected: true}
	for _, pl := range pb.PushAccessLevels {
		lvl := int(pl.AccessLevel)
		// A push level of 0 is "No one"; >= Maintainer (40) excludes a Developer
		// bot. Only a nonzero level at or below Developer (30) lets the bot push.
		if lvl > 0 && lvl <= developerAccessLevel {
			bp.DevelopersCanPush = true
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
	for {
		projects, resp, err := g.client.Projects.ListProjects(opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list projects: %w", err))
		}
		for _, p := range projects {
			out = append(out, Project{
				ForgeProjectID:    p.ID,
				PathWithNamespace: p.PathWithNamespace,
				WebURL:            p.WebURL,
				DefaultBranch:     p.DefaultBranch,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return out, nil
}

func (g *gitLab) ListLabels(ctx context.Context, projectID int64) ([]Label, error) {
	opt := &gitlab.ListLabelsOptions{ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage}}
	var out []Label
	for {
		labels, resp, err := g.client.Labels.ListLabels(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list labels: %w", err))
		}
		for _, l := range labels {
			out = append(out, Label{Name: l.Name, Color: l.Color})
		}
		if resp.NextPage == 0 {
			break
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
		// state=all is mandatory: the Closed column and de-label/close eviction
		// both depend on seeing closed issues.
		State: gitlab.Ptr("all"),
	}
	if len(opts.Labels) > 0 {
		labels := gitlab.LabelOptions(opts.Labels)
		opt.Labels = &labels
	}
	if opts.UpdatedAfter != nil {
		opt.UpdatedAfter = opts.UpdatedAfter
	}
	var out []Issue
	for {
		issues, resp, err := g.client.Issues.ListProjectIssues(projectID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, g.redact.error(fmt.Errorf("gitlab: list issues: %w", err))
		}
		for _, i := range issues {
			out = append(out, toIssue(i))
		}
		if resp.NextPage == 0 {
			break
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

// toIssue maps a client-go issue to the neutral domain type. A nil author (rare
// but possible for system issues) yields an empty Author; a nil UpdatedAt
// yields the zero time, which the sync engine treats as "no HWM advance".
func toIssue(i *gitlab.Issue) Issue {
	issue := Issue{
		IID:         i.IID,
		Title:       i.Title,
		State:       i.State,
		Labels:      []string(i.Labels),
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
