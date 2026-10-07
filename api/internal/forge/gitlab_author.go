package forge

import (
	"context"
	"fmt"
	"net/http"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

func (g *gitLab) RepositoryAuthorEligibility(ctx context.Context, projectID, authorID int64) (AuthorEligibility, error) {
	if projectID <= 0 || authorID <= 0 {
		return AuthorUnknown, ErrAuthorUnknown
	}
	u, userResp, err := g.client.Users.GetUser(authorID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return AuthorUnknown, g.wrapErr("resolve repository author", err)
	}
	if userResp == nil || !completeAuthorResponse(userResp.Response) || u == nil || u.ID != authorID || u.Username == "" || u.State == "" {
		return AuthorUnknown, ErrAuthorUnknown
	}
	// Same effective members/all route as GetInheritedProjectMember. The SDK
	// ProjectMember omits membership_state; decode it here to reject inactive
	// memberships while retaining the existing SDK client, timeout and retries.
	req, err := g.client.NewRequest(http.MethodGet,
		fmt.Sprintf("projects/%d/members/all/%d", projectID, authorID), nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		return AuthorUnknown, g.wrapErr("repository author membership", err)
	}
	var m struct {
		ID              int64                    `json:"id"`
		Username        string                   `json:"username"`
		State           string                   `json:"state"`
		MembershipState *string                  `json:"membership_state"`
		AccessLevel     *gitlab.AccessLevelValue `json:"access_level"`
		ExpiresAt       *gitlab.ISOTime          `json:"expires_at"`
	}
	resp, err := g.client.Do(req, &m)
	if err != nil {
		// The stable identity was independently resolved above; members/all's 404
		// means absence of effective membership for that extant user.
		if resp != nil && resp.Response != nil && resp.StatusCode == http.StatusNotFound && len(resp.Header.Values("Content-Range")) == 0 {
			// Exclude a concealed or deleted project before treating this as absence.
			repo, repoResp, readErr := g.client.Projects.GetProject(projectID, nil, gitlab.WithContext(ctx))
			if readErr != nil {
				return AuthorUnknown, g.wrapErr("author repository", readErr)
			}
			if repoResp == nil || !completeAuthorResponse(repoResp.Response) || repo == nil || repo.ID != projectID {
				return AuthorUnknown, ErrAuthorUnknown
			}
			return AuthorNotEligible, nil
		}
		return AuthorUnknown, g.wrapErr("repository author membership", err)
	}
	if resp == nil || !completeAuthorResponse(resp.Response) || m.ID != authorID || m.Username != u.Username || m.State == "" || m.AccessLevel == nil {
		return AuthorUnknown, ErrAuthorUnknown
	}
	switch *m.AccessLevel {
	case 0, 5, 10, 15, 20, gitlab.SecurityManagerPermissions, 30, 40, 50, 60:
	default:
		return AuthorUnknown, ErrAuthorUnknown
	}
	if u.State != "active" || m.State != "active" ||
		(m.MembershipState != nil && *m.MembershipState != "active") ||
		(m.ExpiresAt != nil && !time.Time(*m.ExpiresAt).After(time.Now())) {
		return AuthorNotEligible, nil
	}
	if *m.AccessLevel >= gitlab.ReporterPermissions {
		return AuthorEligible, nil
	}
	return AuthorNotEligible, nil
}
