package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// githubAppBranchProtection keeps installation credentials out of the PAT's
// role-based protection assumptions. A PR requirement alone still permits a
// writer to merge an approved PR, and non_fast_forward only forbids force-push.
// An effective update restriction must exclude this App from its bypass list.
// Omitted bypass_actors means GitHub withheld it, not that nobody can bypass.
func (g *github) githubAppBranchProtection(ctx context.Context, projectID int64, branch string, appID int64) (BranchProtection, error) {
	unknown := BranchProtection{Protected: true, ProtectionUnverified: true}
	if appID <= 0 || branch == "" {
		return unknown, errors.New("github: invalid App branch protection identity")
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return unknown, err
	}
	b, resp, err := g.client.Repositories.GetBranch(ctx, slug.owner, slug.repo, branch, 0)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return BranchProtection{WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
		}
		return unknown, g.wrapErr("App branch protection", err)
	}
	if !b.GetProtected() {
		return BranchProtection{WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
	}
	if b.GetName() != branch {
		return unknown, nil
	}

	base := fmt.Sprintf("repos/%s/%s/", url.PathEscape(slug.owner), url.PathEscape(slug.repo))
	var effective []struct {
		Type      string `json:"type"`
		RulesetID int64  `json:"ruleset_id"`
	}
	if err := g.githubAppAccessJSON(ctx, base+"rules/branches/"+url.PathEscape(branch), &effective); err != nil {
		return unknown, nil
	}
	ids := make(map[int64]struct{})
	for _, rule := range effective {
		if rule.Type == "update" {
			if rule.RulesetID <= 0 {
				return unknown, nil
			}
			ids[rule.RulesetID] = struct{}{}
		}
	}
	// Bound the number of follow-up calls even for a malformed upstream response.
	if len(ids) == 0 || len(ids) > 64 {
		return unknown, nil
	}
	for id := range ids {
		var ruleset struct {
			ID           int64           `json:"id"`
			Target       string          `json:"target"`
			Enforcement  string          `json:"enforcement"`
			BypassActors json.RawMessage `json:"bypass_actors"`
			Rules        []struct {
				Type string `json:"type"`
			} `json:"rules"`
		}
		path := fmt.Sprintf("%srulesets/%d?includes_parents=true", base, id)
		if err := g.githubAppAccessJSON(ctx, path, &ruleset); err != nil {
			return unknown, nil
		}
		if ruleset.ID != id || ruleset.Target != "branch" || ruleset.Enforcement != "active" {
			return unknown, nil
		}
		hasUpdate := false
		for _, rule := range ruleset.Rules {
			hasUpdate = hasUpdate || rule.Type == "update"
		}
		if !hasUpdate || len(ruleset.BypassActors) == 0 || bytes.Equal(bytes.TrimSpace(ruleset.BypassActors), []byte("null")) {
			return unknown, nil
		}
		var actors []struct {
			ID   *int64 `json:"actor_id"`
			Type string `json:"actor_type"`
			Mode string `json:"bypass_mode"`
		}
		if err := json.Unmarshal(ruleset.BypassActors, &actors); err != nil {
			return unknown, nil
		}
		for _, actor := range actors {
			switch actor.Type {
			case "Integration":
				if actor.ID == nil || *actor.ID <= 0 {
					return unknown, nil
				}
				if *actor.ID == appID {
					switch actor.Mode {
					case "always":
						return BranchProtection{Protected: true, BotCanPush: true, BotCanMerge: true}, nil
					case "pull_request":
						return BranchProtection{Protected: true, BotCanMerge: true}, nil
					default:
						return unknown, nil
					}
				}
			case "OrganizationAdmin", "RepositoryRole", "Team", "DeployKey", "User", "EnterpriseOwner", "EnterpriseRole":
				// Installation tokens are an Integration, not a member, team or key.
			default:
				return unknown, nil
			}
		}
	}
	return BranchProtection{Protected: true}, nil
}

// Use the driver's authenticated client so installation-token renewal also
// applies here. Bodies and upstream error messages never escape this helper.
func (g *github) githubAppAccessJSON(ctx context.Context, path string, out any) error {
	req, err := g.client.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return errors.New("github: cannot construct App protection request")
	}
	client := *g.client.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("github: App protection request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("github: App protection is unreadable")
	}
	body, err := readBounded(resp.Body, objectResponseBytes)
	if err != nil {
		return errors.New("github: App protection response exceeds bounds")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errors.New("github: invalid App protection response")
	}
	return nil
}
