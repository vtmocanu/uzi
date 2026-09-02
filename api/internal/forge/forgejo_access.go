package forge

// forgejo_access.go is the Forgejo access/role/branch-protection seam:
// ProjectRole and roleForForgejoPermission (bot repo-role derivation) and
// DefaultBranchProtection (the fail-safe D6 branch-protection read).

import (
	"context"
	"fmt"
	"net/http"

	gitea "code.gitea.io/sdk/gitea"
)

func (f *forgejo) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (Role, bool, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return RoleNone, false, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return RoleNone, false, err
	}
	// The collaborator-permission endpoint is keyed by username, but the interface
	// hands us a numeric id, so resolve it. This is the bot's own id in every uzi
	// caller (privcheck checks the bot); GetUserByID works for any visible user.
	u, _, err := c.GetUserByID(forgeUserID)
	if err != nil {
		return RoleNone, false, f.wrapErr(fmt.Sprintf("resolve user %d", forgeUserID), err)
	}
	res, resp, err := c.CollaboratorPermission(slug.owner, slug.repo, u.UserName)
	if err != nil {
		// A 404 here means the USER does not exist (or was removed from a PRIVATE
		// repo, whose middleware 404s the route) — reported as not-a-member, not an
		// error. This is NOT the removed-on-public case: a bot removed from a PUBLIC
		// repo gets 200 with permission:"read", handled by the derivation below (D7).
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return RoleNone, false, nil
		}
		return RoleNone, false, f.wrapErr("project role", err)
	}
	if res == nil {
		return RoleNone, false, fmt.Errorf("forgejo: project role: empty permission payload")
	}
	role := roleForForgejoPermission(res.Permission)
	// member is derived from the PERMISSION, not the status code (D7). On a public
	// repo a bot with no grant returns permission:"read" (the public baseline),
	// indistinguishable from an explicit read collaborator — uzi treats "read or
	// none" as not an effective member, so a bot removed from a public repo still
	// raises the "no longer a member" finding a naive 404-check would miss. A
	// consequence worth naming: a bot demoted to read reads as not-a-member here,
	// where the GitLab driver would say "below write role"; both are violations,
	// and the read/removed states are unobservable-apart on a public repo.
	member := role.AtLeast(RoleWrite)
	return role, member, nil
}

// roleForForgejoPermission maps Forgejo's permission vocabulary onto the neutral
// Role. Forgejo already speaks none|read|write|admin|owner (no numeric levels to
// launder), so this is a direct rename, not the lossy collapse the GitLab driver
// does over access-level integers.
func roleForForgejoPermission(p gitea.AccessMode) Role {
	switch p {
	case gitea.AccessModeOwner:
		return RoleOwner
	case gitea.AccessModeAdmin:
		return RoleAdmin
	case gitea.AccessModeWrite:
		return RoleWrite
	case gitea.AccessModeRead:
		return RoleRead
	default:
		return RoleNone
	}
}

func (f *forgejo) DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return BranchProtection{}, err
	}
	slug, err := f.repoSlugFor(c, projectID)
	if err != nil {
		return BranchProtection{}, err
	}
	// GET /repos/{o}/{r}/branches/{branch} is reader-gated (reqRepoReader), so a
	// write bot can read it — unlike /branch_protections/{name}, whose whole group
	// is reqAdmin() and 403s a write bot, degrading the guardrail to a warning
	// (D6). The endpoint returns protected / user_can_push / user_can_merge
	// COMPUTED FOR THE CALLING BOT by the same path the pre-receive hook enforces,
	// so it is a direct authoritative answer to "can this bot push/merge to main",
	// not GitLab's inference from access levels. We never call branch_protections.
	b, resp, err := c.GetRepoBranch(slug.owner, slug.repo, branch)
	if err != nil {
		// A 404 means the branch itself is absent (an unprotected branch still
		// returns 200 with protected:false — Forgejo's early-return path). Treat a
		// missing branch as unprotected-and-open, the same not-safe shape the GitLab
		// driver reports on its 404: reporting the zero value would call the most
		// dangerous state "cannot push, cannot merge" (see BranchProtection).
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return BranchProtection{Protected: false, WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
		}
		return BranchProtection{}, f.wrapErr("branch protection", err)
	}
	// user_can_push / user_can_merge are the calling bot's authoritative rights.
	// Forgejo folds the write-role capability and any per-user grant into these two
	// booleans (D6: "subsumed by user_can_push"), so they map onto WriteRoleCanPush
	// / WriteRoleCanMerge — the fields the checker and the R12 shared evaluator key
	// on. BotCanPush / BotCanMerge (the GitLab per-user-grant fields) stay false:
	// there is no separate signal to populate them, and leaving them clear keeps an
	// unprotected branch described identically to the GitLab driver (WriteRole*
	// true, Bot* false), which is what R12/test #9b require.
	return BranchProtection{
		Protected:         b.Protected,
		WriteRoleCanPush:  b.UserCanPush,
		WriteRoleCanMerge: b.UserCanMerge,
	}, nil
}
