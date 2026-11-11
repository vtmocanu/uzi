package forge

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	gh "github.com/google/go-github/v92/github"
)

// GitHubAppPermissionsValid checks the effective downscoped token, not the
// broader permissions granted to the shared App installation.
func GitHubAppPermissionsValid(actual map[string]string) bool {
	return maps.Equal(actual, githubAppPermissions())
}

func (g *github) gitCredential(ctx context.Context) (string, error) {
	if g.app == nil {
		return g.token, nil
	}
	token, _, err := g.app.credential(ctx)
	return token, err
}

func (g *github) verifyApp(ctx context.Context) (BotIdentity, error) {
	if _, _, err := g.app.credential(ctx); err != nil {
		return BotIdentity{}, err
	}
	g.app.mu.Lock()
	slug := g.app.slug
	g.app.mu.Unlock()
	login := slug + "[bot]"
	user, _, err := g.client.Users.Get(ctx, login)
	if err != nil {
		return BotIdentity{}, g.wrapErr("verify App bot", err)
	}
	if user.GetID() <= 0 || !strings.EqualFold(user.GetLogin(), login) || user.GetType() != "Bot" || user.GetSiteAdmin() {
		return BotIdentity{}, errors.New("github app: could not verify App bot identity")
	}
	return BotIdentity{ForgeUserID: user.GetID(), Username: user.GetLogin()}, nil
}

func (g *github) appTokenInfo(ctx context.Context) (TokenInfo, error) {
	if _, _, err := g.app.credential(ctx); err != nil {
		return TokenInfo{}, err
	}
	g.app.mu.Lock()
	defer g.app.mu.Unlock()
	scopes := make([]string, 0, len(g.app.permissions))
	for name, level := range g.app.permissions {
		scopes = append(scopes, name+":"+level)
	}
	slices.Sort(scopes)
	return TokenInfo{GitHubApp: true, AppPermissions: maps.Clone(g.app.permissions), Scopes: scopes, Active: true, ExpiresAt: g.app.expires}, nil
}

func (g *github) listAppProjects(ctx context.Context) ([]Project, error) {
	return paginate(func(err error) error { return g.wrapErr("list installation repositories", err) }, func(page int) ([]Project, int, error) {
		req, err := g.client.NewRequest(ctx, http.MethodGet, fmt.Sprintf("installation/repositories?per_page=%d&page=%d", githubPerPage, page), nil)
		if err != nil {
			return nil, 0, err
		}
		var result struct {
			Repositories []*gh.Repository `json:"repositories"`
		}
		resp, err := g.client.Do(req, &result)
		if err != nil {
			return nil, 0, err
		}
		items := make([]Project, 0, len(result.Repositories))
		for _, repo := range result.Repositories {
			if repo == nil || repo.GetID() <= 0 || !g.appRepositorySelected(repo.GetName()) || !g.appRepositoryIDSelected(repo.GetID()) {
				continue
			}
			items = append(items, Project{ForgeProjectID: repo.GetID(), PathWithNamespace: repo.GetFullName(), WebURL: repo.GetHTMLURL(), DefaultBranch: repo.GetDefaultBranch()})
		}
		return items, resp.NextPage, nil
	})
}

func (g *github) appRepositoryIDSelected(id int64) bool {
	g.app.mu.Lock()
	defer g.app.mu.Unlock()
	return g.app.repositoryIDs[id]
}

func (g *github) appRepositorySelected(name string) bool {
	return slices.ContainsFunc(g.app.config.Repositories, func(selected string) bool { return strings.EqualFold(name, selected) })
}

func (g *github) appProjectRole(ctx context.Context, projectID int64) (Role, bool, error) {
	repo, resp, err := g.client.Repositories.GetByID(ctx, projectID)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return RoleNone, false, nil
		}
		return RoleNone, false, g.wrapErr("installation repository access", err)
	}
	if repo.GetID() != projectID || !g.appRepositorySelected(repo.GetName()) {
		return RoleNone, false, nil
	}
	info, err := g.appTokenInfo(ctx)
	if err != nil {
		return RoleNone, false, err
	}
	if !GitHubAppPermissionsValid(info.AppPermissions) {
		return RoleNone, false, errors.New("github app: invalid effective token permissions")
	}
	if !g.appRepositoryIDSelected(repo.GetID()) {
		return RoleNone, false, nil
	}
	return RoleWrite, true, nil
}
