package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	gitea "code.gitea.io/sdk/gitea"
)

func (f *forgejo) RepositoryAuthorEligibility(ctx context.Context, projectID, authorID int64) (AuthorEligibility, error) {
	if projectID <= 0 || authorID <= 0 {
		return AuthorUnknown, ErrAuthorUnknown
	}
	c, err := f.newClient(ctx)
	if err != nil {
		return AuthorUnknown, err
	}
	u, userResp, err := c.GetUserByID(authorID)
	if err != nil {
		return AuthorUnknown, f.wrapErr("resolve repository author", err)
	}
	if userResp == nil || !completeAuthorResponse(userResp.Response) || u == nil || u.ID != authorID || u.UserName == "" {
		return AuthorUnknown, ErrAuthorUnknown
	}
	r, err := assessmentEvidence(ctx, evidenceKey{f, projectID, "repository"}, func() (*gitea.Repository, error) {
		repo, repoResp, err := c.GetRepoByID(projectID)
		if err != nil {
			return nil, f.wrapErr("author repository", err)
		}
		if repoResp == nil || !completeAuthorResponse(repoResp.Response) || repo == nil || repo.ID != projectID || repo.Owner == nil || repo.Owner.ID <= 0 ||
			repo.Owner.UserName == "" || repo.Name == "" || repo.FullName != repo.Owner.UserName+"/"+repo.Name {
			return nil, ErrAuthorUnknown
		}
		return repo, nil
	})
	if err != nil {
		return AuthorUnknown, err
	}
	personal, ownershipErr := assessmentEvidence(ctx, evidenceKey{f, projectID, "ownership"}, func() (bool, error) {
		return f.authorPersonalRepository(ctx, r.Owner.UserName, r.Name)
	})
	if ownershipErr == nil && personal && r.Owner.ID == authorID {
		if r.Owner.UserName != u.UserName {
			return AuthorUnknown, ErrAuthorUnknown
		}
		return AuthorEligible, nil
	}
	direct, directErr := assessmentEvidence(ctx, evidenceKey{f, projectID, "direct collaborators"}, func() (map[int64]string, error) {
		all, err := paginate(func(e error) error { return f.wrapErr("author collaborators", e) }, func(page int) ([]*gitea.User, int, error) {
			users, resp, err := c.ListCollaborators(r.Owner.UserName, r.Name,
				gitea.ListCollaboratorsOptions{ListOptions: gitea.ListOptions{Page: page, PageSize: forgejoPerPage}})
			if err != nil {
				return nil, 0, err
			}
			if resp == nil || !completeAuthorResponse(resp.Response) || users == nil {
				return nil, 0, ErrAuthorUnknown
			}
			if err := validateAuthorNextPage(strings.Join(resp.Header.Values("Link"), ","), page, resp.NextPage); err != nil {
				return nil, 0, err
			}
			return users, resp.NextPage, nil
		})
		if err != nil {
			return nil, err
		}
		index := make(map[int64]string)
		for _, m := range all {
			if m == nil || m.ID <= 0 || m.UserName == "" {
				return nil, ErrAuthorUnknown
			}
			if _, exists := index[m.ID]; exists {
				return nil, ErrAuthorUnknown
			}
			index[m.ID] = m.UserName
		}
		return index, nil
	})
	if directErr == nil {
		if login, ok := direct[authorID]; ok {
			if login != u.UserName {
				return AuthorUnknown, ErrAuthorUnknown
			}
			return AuthorEligible, nil // an explicit direct Read grant is sufficient
		}
	}
	// No author-specific request is necessary for an extant nonowner excluded
	// from the complete direct list of a proven personal repository.
	if ownershipErr == nil && personal && r.Owner.ID != authorID && directErr == nil {
		return AuthorNotEligible, nil
	}
	p, permissionResp, err := c.CollaboratorPermission(r.Owner.UserName, r.Name, u.UserName)
	if err != nil {
		return AuthorUnknown, f.wrapErr("repository author permission", err)
	}
	if permissionResp == nil || !completeAuthorResponse(permissionResp.Response) || p == nil || p.User == nil || p.User.ID != authorID || p.User.UserName != u.UserName {
		return AuthorUnknown, ErrAuthorUnknown
	}
	switch p.Permission {
	case gitea.AccessModeWrite, gitea.AccessModeAdmin, gitea.AccessModeOwner:
		return AuthorEligible, nil
	case gitea.AccessModeNone, gitea.AccessModeRead:
		// Exclusion requires complete direct evidence plus ownership proof.
		if directErr == nil && ownershipErr == nil && (!personal || r.Owner.ID != authorID) {
			return AuthorNotEligible, nil
		}
	default:
		return AuthorUnknown, ErrAuthorUnknown
	}
	return AuthorUnknown, ErrAuthorUnknown
}

// authorPersonalRepository uses the existing capped client (32 MiB success,
// 4 KiB error). The v16 teams endpoint is ownership proof only; team roles,
// units, assignees, and organization membership are never authorization input.
func (f *forgejo) authorPersonalRepository(ctx context.Context, owner, repo string) (bool, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/teams"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/api/v1"+path, nil)
	if err != nil {
		return false, f.wrapErr("author ownership", err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return false, f.wrapErr("author ownership", err)
	}
	if resp == nil || resp.Body == nil {
		return false, ErrAuthorUnknown
	}
	defer func() { _ = resp.Body.Close() }()
	if len(resp.Header.Values("Content-Range")) != 0 {
		return false, ErrAuthorUnknown
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, f.wrapErr("author ownership", err)
	}
	if resp.StatusCode == http.StatusMethodNotAllowed {
		var payload struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Message == "repo is not owned by an organization" {
			return true, nil
		}
		return false, ErrAuthorUnknown
	}
	if !completeAuthorResponse(resp) {
		return false, f.wrapErr("author ownership", fmt.Errorf("status %d: %w", resp.StatusCode, ErrAuthorUnknown))
	}
	// v16 returns the whole repository team list. Reject an incomplete variant.
	if err := validateAuthorNextPage(strings.Join(resp.Header.Values("Link"), ","), 1, 0); err != nil {
		return false, err
	}
	var teams []*gitea.Team
	if json.Unmarshal(body, &teams) != nil || teams == nil {
		return false, ErrAuthorUnknown
	}
	seen := make(map[int64]bool)
	for _, team := range teams {
		if team == nil || team.ID <= 0 || team.Name == "" || seen[team.ID] {
			return false, ErrAuthorUnknown
		}
		seen[team.ID] = true
	}
	return false, nil
}
