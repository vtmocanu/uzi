package forge

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v92/github"
)

func (g *github) RepositoryAuthorEligibility(ctx context.Context, projectID, authorID int64) (AuthorEligibility, error) {
	if projectID <= 0 || authorID <= 0 {
		return AuthorUnknown, ErrAuthorUnknown
	}
	state := newResponseLimitState(objectResponseBytes)
	u, userResp, err := g.client.Users.GetByID(withResponseLimit(ctx, state), authorID)
	if state.overflow.Load() {
		return AuthorUnknown, g.wrapErr("resolve repository author", state.err())
	}
	if err != nil {
		return AuthorUnknown, g.wrapErr("resolve repository author", err)
	}
	if userResp == nil || !completeAuthorResponse(userResp.Response) || u == nil || u.GetID() != authorID || u.GetLogin() == "" {
		return AuthorUnknown, ErrAuthorUnknown
	}
	members, err := assessmentEvidence(ctx, evidenceKey{g, projectID, "collaborators"}, func(ctx context.Context) (map[int64]*gh.User, error) {
		state := newResponseLimitState(objectResponseBytes)
		r, repoResp, err := g.client.Repositories.GetByID(withResponseLimit(ctx, state), projectID)
		if state.overflow.Load() {
			return nil, g.wrapErr("author repository", state.err())
		}
		if err != nil {
			return nil, g.wrapErr("author repository", err)
		}
		if repoResp == nil || !completeAuthorResponse(repoResp.Response) || r == nil || r.GetID() != projectID || r.GetOwner().GetLogin() == "" || r.GetName() == "" {
			return nil, ErrAuthorUnknown
		}
		all, err := paginate(func(e error) error { return g.wrapErr("author collaborators", e) }, func(page int) ([]*gh.User, int, error) {
			state := newResponseLimitState(objectResponseBytes)
			users, resp, err := g.client.Repositories.ListCollaborators(withResponseLimit(ctx, state), r.GetOwner().GetLogin(), r.GetName(),
				&gh.ListCollaboratorsOptions{Affiliation: "all", ListOptions: gh.ListOptions{Page: page, PerPage: 100}})
			if state.overflow.Load() {
				return nil, 0, state.err()
			}
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
		index := make(map[int64]*gh.User)
		for _, m := range all {
			if m == nil || m.GetID() <= 0 || m.GetLogin() == "" || m.Permissions == nil {
				return nil, ErrAuthorUnknown
			}
			// The base booleans must be present, including false values.
			p := m.Permissions
			if p.Pull == nil || p.Triage == nil || p.Push == nil || p.Maintain == nil || p.Admin == nil {
				return nil, fmt.Errorf("github: author collaborator missing base permission: %w", ErrAuthorUnknown)
			}
			if _, exists := index[m.GetID()]; exists {
				return nil, ErrAuthorUnknown
			}
			index[m.GetID()] = m
		}
		return index, nil
	})
	if err != nil {
		return AuthorUnknown, err
	}
	m := members[authorID]
	if m == nil {
		return AuthorNotEligible, nil
	}
	if m.GetLogin() != u.GetLogin() {
		return AuthorUnknown, ErrAuthorUnknown
	}
	p := m.Permissions
	if p.GetTriage() || p.GetPush() || p.GetMaintain() || p.GetAdmin() {
		return AuthorEligible, nil
	}
	return AuthorNotEligible, nil
}
