package forge

import (
	"context"
	"errors"
	"net/url"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// projectIdentity requires an unambiguous owner/path and a matching web URL.
// A single terminal URL slash is ignored; other path spellings are not aliases.
// DefaultBranch is optional, as it is for Project returned by ListProjects.
func projectIdentity(p Project, requested int64, namespace, name string) bool {
	if p.ForgeProjectID != requested || namespace == "" || name == "" ||
		strings.Contains(name, "/") || p.PathWithNamespace != namespace+"/"+name {
		return false
	}
	for _, part := range strings.Split(p.PathWithNamespace, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\%?#") ||
			strings.IndexFunc(part, func(r rune) bool { return r <= ' ' || r == 127 }) >= 0 {
			return false
		}
	}
	u, err := url.Parse(p.WebURL)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") &&
		u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && !strings.Contains(p.WebURL, "#") &&
		strings.TrimSuffix(u.Path, "/") == "/"+p.PathWithNamespace
}

// projectLookupError restores transport refusals even when an SDK discarded the
// body read error while classifying or draining its response.
func projectLookupError(state *responseLimitState, err error) error {
	if state.overflow.Load() {
		return state.err()
	}
	if state.unsupportedEncoding.Load() {
		return &bodyReadError{category: "unsupported content encoding"}
	}
	return err
}

func (g *github) GetProject(ctx context.Context, projectID int64) (p Project, err error) {
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if err != nil {
			delete(g.slugs, projectID)
		} else {
			parts := strings.SplitN(p.PathWithNamespace, "/", 2)
			g.slugs[projectID] = repoSlug{owner: parts[0], repo: parts[1]}
		}
	}()
	if projectID <= 0 {
		return Project{}, errors.New("github: get project: positive project ID required")
	}
	state := newResponseLimitState(objectResponseBytes)
	state.projectLookup = true
	r, resp, readErr := g.client.Repositories.GetByID(withResponseLimit(ctx, state), projectID)
	if readErr = projectLookupError(state, readErr); readErr != nil {
		return Project{}, g.wrapErr("get project", readErr)
	}
	if resp == nil || !completeAuthorResponse(resp.Response) || r == nil {
		return Project{}, errors.New("github: get project: incomplete response")
	}
	p = Project{ForgeProjectID: r.GetID(), PathWithNamespace: r.GetFullName(), WebURL: r.GetHTMLURL(), DefaultBranch: r.GetDefaultBranch()}
	if !projectIdentity(p, projectID, r.GetOwner().GetLogin(), r.GetName()) ||
		strings.Count(p.PathWithNamespace, "/") != 1 {
		return Project{}, errors.New("github: get project: inconsistent project identity")
	}
	return p, nil
}

func (f *forgejo) GetProject(ctx context.Context, projectID int64) (p Project, err error) {
	defer func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err != nil {
			delete(f.slugs, projectID)
		} else {
			parts := strings.SplitN(p.PathWithNamespace, "/", 2)
			f.slugs[projectID] = repoSlug{owner: parts[0], repo: parts[1]}
		}
	}()
	if projectID <= 0 {
		return Project{}, errors.New("forgejo: get project: positive project ID required")
	}
	state := newResponseLimitState(objectResponseBytes)
	state.projectLookup = true
	c, err := f.newClient(withResponseLimit(ctx, state))
	if err != nil {
		return Project{}, err
	}
	r, resp, readErr := c.GetRepoByID(projectID)
	if readErr = projectLookupError(state, readErr); readErr != nil {
		status := 0
		if resp != nil && resp.Response != nil {
			status = resp.StatusCode
		}
		return Project{}, f.wrapErrStatus("get project", readErr, status)
	}
	if resp == nil || !completeAuthorResponse(resp.Response) || r == nil || r.Owner == nil {
		return Project{}, errors.New("forgejo: get project: incomplete response")
	}
	p = Project{ForgeProjectID: r.ID, PathWithNamespace: r.FullName, WebURL: r.HTMLURL, DefaultBranch: r.DefaultBranch}
	if !projectIdentity(p, projectID, r.Owner.UserName, r.Name) ||
		strings.Count(p.PathWithNamespace, "/") != 1 {
		return Project{}, errors.New("forgejo: get project: inconsistent project identity")
	}
	return p, nil
}

func (g *gitLab) GetProject(ctx context.Context, projectID int64) (Project, error) {
	if projectID <= 0 {
		return Project{}, errors.New("gitlab: get project: positive project ID required")
	}
	state := newResponseLimitState(objectResponseBytes)
	state.projectLookup = true
	r, resp, err := g.client.Projects.GetProject(projectID, nil, gitlab.WithContext(withResponseLimit(ctx, state)))
	if err = projectLookupError(state, err); err != nil {
		return Project{}, g.wrapErr("get project", err)
	}
	if resp == nil || !completeAuthorResponse(resp.Response) || r == nil || r.Namespace == nil {
		return Project{}, errors.New("gitlab: get project: incomplete response")
	}
	p := Project{ForgeProjectID: r.ID, PathWithNamespace: r.PathWithNamespace, WebURL: r.WebURL, DefaultBranch: r.DefaultBranch}
	if !projectIdentity(p, projectID, r.Namespace.FullPath, r.Path) {
		return Project{}, errors.New("gitlab: get project: inconsistent project identity")
	}
	return p, nil
}
