package forge

// github_access.go is the GitHub access/role/branch-protection seam: ProjectRole
// and roleForGitHubPermissions (bot repo-role derivation) and
// DefaultBranchProtection (the fail-safe D6 branch-protection read).

import (
	"context"

	gh "github.com/google/go-github/v90/github"
)

func (g *github) ProjectRole(ctx context.Context, projectID, forgeUserID int64) (Role, bool, error) {
	// GET /repositories/{id} returns permissions COMPUTED FOR THE AUTHENTICATED USER
	// (the bot). uzi always calls ProjectRole for the bot's own id, so the repo
	// permissions ARE the answer (D6) — GitHub exposes no per-arbitrary-user
	// branch/role flag at write role. forgeUserID is accepted for interface parity
	// but not separately queried; the caller (privcheck) checks the bot.
	r, resp, err := g.client.Repositories.GetByID(ctx, projectID)
	if err != nil {
		// A 404 → not a member (never an error): the bot has no visibility of the repo.
		if resp != nil && resp.StatusCode == 404 {
			return RoleNone, false, nil
		}
		return RoleNone, false, g.wrapErr("project role", err)
	}
	role := roleForGitHubPermissions(r.GetPermissions())
	// member = role.AtLeast(RoleWrite), mirroring the Forgejo derivation: a bot at
	// read/none is not an effective member for uzi's purposes (and is a violation).
	member := role.AtLeast(RoleWrite)
	return role, member, nil
}

// roleForGitHubPermissions maps GitHub's repo permission booleans onto the neutral
// Role. admin→Admin (a violation); maintain or push→Write (both grant git push);
// triage or pull→Read (neither grants push); none→None. Checked most- to
// least-privileged so the highest true bit wins. GetX() are nil-safe.
func roleForGitHubPermissions(p *gh.RepositoryPermissions) Role {
	switch {
	case p.GetAdmin():
		return RoleAdmin
	case p.GetMaintain():
		return RoleWrite
	case p.GetPush():
		return RoleWrite
	case p.GetTriage():
		return RoleRead
	case p.GetPull():
		return RoleRead
	default:
		return RoleNone
	}
}

// DefaultBranchProtection is the D6 CRUX and is materially weaker on GitHub than
// on Forgejo, so it is built with a fail-safe disposition and its own comment
// block. GitHub gives a write-role bot NO branch-scoped push/merge flag: the
// admin-gated GET /branches/{b}/protection 403s the bot (so this method NEVER
// calls it — the same discipline #65 kept for Forgejo's branch_protections/), and
// the reader-gated surfaces answer only partially.
//
// UNRESOLVED SPIKE (R1) — NOT EXECUTABLE IN THIS SANDBOX: whether
// Repositories.ListRulesForBranch reflects the CALLING BOT'S OWN bypass ability
// (so a returned enforced rule genuinely means the bot cannot push) versus merely
// listing rules that exist, could not be probed live here (no GitHub credentials).
// The design below is FAIL-SAFE UNDER BOTH outcomes, because on a Protected branch
// it NEVER reports a fabricated `WriteRoleCanPush/Merge = true`:
//   - If the endpoint DOES reflect bypass: an enforced pull_request/non_fast_forward
//     rule that comes back genuinely blocks the bot → Can*=false is authoritative.
//   - If it does NOT: a returned rule may not prove the bot is blocked, but we still
//     report Can*=false (over-cautious, never a false "can push"); and when NO rule
//     comes back we flag ProtectionUnverified so the consumer fails closed rather
//     than reading the false as "safe".
//
// Dispositions:
//   - Unprotected branch → WriteRoleCanPush=true, WriteRoleCanMerge=true,
//     ProtectionUnverified=false. An unprotected branch is the WORST case (a write
//     bot CAN push and merge); reporting the zero value would call the most
//     dangerous state "cannot push, cannot merge" (see BranchProtection).
//   - Protected + a readable, covering, enforced ruleset (pull_request or
//     non_fast_forward) → Can*=false, ProtectionUnverified=false (authoritatively
//     protected).
//   - Protected + NO readable ruleset explaining it (legacy branch protection,
//     invisible at write role) → Can*=false AND ProtectionUnverified=true. Never a
//     fabricated true, never a false "safe" (D6/R1). #66 must fail-closed on it.
//   - BotCanPush/BotCanMerge stay false (GitLab-only).
func (g *github) DefaultBranchProtection(ctx context.Context, projectID int64, branch string, botUserID int64) (BranchProtection, error) {
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return BranchProtection{}, err
	}
	// GET /repos/{o}/{r}/branches/{branch} is reader-gated; its top-level `protected`
	// bool is readable at write role. maxRedirects=1 matches the SDK signature; this
	// endpoint does not redirect.
	b, resp, err := g.client.Repositories.GetBranch(ctx, slug.owner, slug.repo, branch, 1)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			// Branch absent → treat as unprotected-and-open, the not-safe shape the
			// other drivers report on a 404.
			return BranchProtection{WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
		}
		return BranchProtection{}, g.wrapErr("branch protection", err)
	}
	if !b.GetProtected() {
		return BranchProtection{WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil
	}
	// Protected. Read rulesets (Metadata:read, reader-gated) — the only reader-gated
	// protection detail available to a write bot.
	rules, _, err := g.client.Repositories.ListRulesForBranch(ctx, slug.owner, slug.repo, branch, nil)
	if err != nil {
		// Rulesets should be readable at write role; if we cannot read them we cannot
		// authoritatively determine push/merge on a protected branch. Fail-safe.
		return BranchProtection{Protected: true, ProtectionUnverified: true}, nil
	}
	if rules != nil && (len(rules.PullRequest) > 0 || len(rules.NonFastForward) > 0) {
		// A readable, active, enforced ruleset covers the branch and blocks a direct
		// push / direct merge by the write-role bot. Authoritative.
		return BranchProtection{Protected: true}, nil
	}
	// Protected:true but no readable ruleset explains it — legacy branch protection.
	return BranchProtection{Protected: true, ProtectionUnverified: true}, nil
}
