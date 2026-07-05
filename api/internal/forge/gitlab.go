package forge

import (
	"context"
	"fmt"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

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
	return BotIdentity{ForgeUserID: u.ID, Username: u.Username}, nil
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

func (g *gitLab) GetMergeRequest(ctx context.Context, projectID, mrIID int64) (MergeRequest, error) {
	mr, _, err := g.client.MergeRequests.GetMergeRequest(projectID, mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return MergeRequest{}, g.redact.error(fmt.Errorf("gitlab: get merge request: %w", err))
	}
	return toMergeRequest(mr), nil
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
