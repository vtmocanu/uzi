package privcheck

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

const (
	// defaultExpiryWarnWindow is how far out a pending token expiry becomes a
	// warning (advance notice, not a violation — a strict red at 13 days would
	// only train users to ignore badges).
	defaultExpiryWarnWindow = 14 * 24 * time.Hour
	// defaultConcurrency bounds the per-repo forge fan-out, matching the poller's
	// politeness discipline toward the upstream forge.
	defaultConcurrency = 4
)

// Checker computes privilege reports against a Forge. It holds no state beyond
// its tunables and is safe to share.
type Checker struct {
	warnWindow  time.Duration
	concurrency int
}

// NewChecker constructs a Checker with the default expiry-warning window and
// per-repo concurrency.
func NewChecker() *Checker {
	return &Checker{warnWindow: defaultExpiryWarnWindow, concurrency: defaultConcurrency}
}

// CheckToken runs only the token-level checks (the save-time path): scopes,
// active, expiry, and the instance-admin flag. isAdmin comes from the caller's
// existing VerifyToken, so this makes at most one extra forge call (TokenInfo).
// An introspection-unsupported forge or a transient introspection error becomes
// a warning, never a blocking violation; the admin check still applies (it rides
// on VerifyToken, not TokenInfo). now is injectable for tests.
func (c *Checker) CheckToken(ctx context.Context, f forge.Forge, forgeType forge.Type, isAdmin bool, now time.Time) TokenReport {
	info, err := f.TokenInfo(ctx)
	if err != nil {
		warn := "could not verify token scopes against the forge; scope/expiry checks were skipped"
		if errors.Is(err, forge.ErrTokenIntrospectionUnsupported) {
			warn = introspectionUnsupportedWarn(forgeType)
		}
		return evaluateToken(forgeType, forge.TokenInfo{}, isAdmin, now, c.warnWindow, warn)
	}
	return evaluateToken(forgeType, info, isAdmin, now, c.warnWindow, "")
}

// introspectionUnsupportedWarn is the per-forge message used when a token cannot
// be introspected for scopes/expiry (PRD #238 S4). It is kept per-forge on
// purpose: GitLab's version-gate is fixable by upgrading, so its hint names the
// version; GitHub's is a token-type problem — a fine-grained token exposes no
// scopes header — so it points the user at a classic PAT instead. Flattening
// either into a lossy generic string would drop an actionable hint. Forgejo (and
// any other forge) never reaches this arm in practice, but a generic default
// keeps the switch total.
func introspectionUnsupportedWarn(forgeType forge.Type) string {
	switch forgeType {
	case forge.TypeGitHub:
		return "cannot verify token scopes: fine-grained tokens can't be introspected; use a classic PAT"
	default:
		return "cannot verify token scopes on this GitLab version (requires 15.5+)"
	}
}

// Check runs the full report: the token-level checks plus every enabled repo's
// role and default-branch protection, with bounded per-repo concurrency. It is
// error-as-report: if the token cannot even be verified against the forge
// (revoked, forge unreachable) the report is StatusError with that finding, not
// a returned error — a revoked token is exactly what the report must surface.
func (c *Checker) Check(ctx context.Context, f forge.Forge, forgeType forge.Type, repos []Repo, now time.Time) Report {
	identity, err := f.VerifyToken(ctx)
	if err != nil {
		// A version-gate refusal is a different condition from a bad token, and
		// pointing the user at the wrong fix wastes their time. VerifyToken owns the
		// Forgejo >=16 comparison (D4/D4a) and wraps forge.ErrForgeVersionUnsupported
		// when the instance is below the driver minimum; the sweep re-fires that
		// gate here for free, so an instance that connected at 16.x and later
		// downgraded resurfaces on the next pass. Give it its own finding — the fix
		// is to upgrade the server, not to re-mint the token. The tier stays
		// StatusError (the check could not complete against a usable forge), and the
		// raw driver error is not embedded: it is PAT-redacted but we keep the
		// report generic on both arms, and the driver already surfaces the
		// reported/required versions verbatim at connect.
		if errors.Is(err, forge.ErrForgeVersionUnsupported) {
			return errorReport(now, "the forge server is older than the minimum version uzi requires; upgrade the server (re-minting the token will not help)")
		}
		// err is already PAT-redacted by the driver, but we keep the report
		// generic rather than embedding the raw forge error at all.
		return errorReport(now, "could not verify the bot token against the forge (revoked, expired, or forge unreachable)")
	}
	token := c.CheckToken(ctx, f, forgeType, identity.IsAdmin, now)
	repoReports := c.checkRepos(ctx, f, identity.ForgeUserID, repos)
	return Report{
		CheckedAt: now,
		Token:     token,
		Repos:     repoReports,
		Status:    computeStatus(token, repoReports),
	}
}

// checkRepos fans out the per-repo checks with a bounded worker pool, preserving
// input order in the result.
func (c *Checker) checkRepos(ctx context.Context, f forge.Forge, botUserID int64, repos []Repo) []RepoReport {
	out := make([]RepoReport, len(repos))
	if len(repos) == 0 {
		return out
	}
	limit := c.concurrency
	if limit <= 0 {
		limit = defaultConcurrency
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range repos {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = c.checkRepo(ctx, f, botUserID, repos[i])
		}(i)
	}
	wg.Wait()
	return out
}

// checkRepo runs the role + branch-protection checks for one repo. A forge call
// failing mid-repo is a warning on that repo, never a crash — the sweep must
// keep going across a flaky project.
func (c *Checker) checkRepo(ctx context.Context, f forge.Forge, botUserID int64, repo Repo) RepoReport {
	rr := RepoReport{RepoID: repo.ID, Path: repo.Path, Findings: []Finding{}}

	role, member, roleErr := f.ProjectRole(ctx, repo.ForgeProjectID, botUserID)

	haveBranch := repo.DefaultBranch != ""
	var (
		bp    forge.BranchProtection
		bpErr error
	)
	if haveBranch {
		bp, bpErr = f.DefaultBranchProtection(ctx, repo.ForgeProjectID, repo.DefaultBranch, botUserID)
	}

	evaluateRepo(&rr, repo, role, member, roleErr, haveBranch, bp, bpErr)
	return rr
}

// evaluateRepo turns one repo's forge answers (the bot's role, and the default
// branch's protection) into findings. It is pure — no forge calls — and it is
// the single evaluator both drivers flow through, because checkRepo talks only
// to the forge.Forge interface and never learns which forge answered. That is
// what "one shared evaluateRepo" (R12) means here: there is no per-forge branch
// to keep in step.
//
// The load-bearing rule is that Protected is examined FIRST. An unprotected
// default branch is the single worst case — the bot can push and can merge — so
// it short-circuits to the strongest finding, and the push/merge fields are read
// only on a branch that is protected. A consumer of this function therefore
// never has to reason about what a false push/merge field means on an
// unprotected branch, because it never reaches one. The drivers now also report
// those fields truthfully on the unprotected path (true, not a skipped false —
// see forge.BranchProtection), so this ordering is defence in depth rather than
// the only thing between a false and an inverted guardrail; PRD #65 keeps both,
// per CLAUDE.md's "don't weaken a layer because another covers it".
func evaluateRepo(rr *RepoReport, repo Repo, role forge.Role, member bool, roleErr error, haveBranch bool, bp forge.BranchProtection, bpErr error) {
	if roleErr != nil {
		rr.Findings = append(rr.Findings, newFinding(CodeRoleUnreadable, "could not read the bot's role on this repo"))
	} else {
		rr.Role = role
		rr.Member = member
		switch {
		case !member:
			// D6 downgrades bot-not-member from a violation to a warn: the bot being
			// too weak is not the "too strong" case #66 refuses on.
			rr.Findings = append(rr.Findings, newFinding(CodeBotNotMember, "bot is no longer a member of this repo; sync is broken"))
		case role == forge.RoleWrite:
			// Exactly the role uzi wants: no finding.
		case role.AtLeast(forge.RoleWrite):
			rr.Findings = append(rr.Findings, newFinding(CodeBotRoleAboveWrite, fmt.Sprintf("bot role is %s, above the expected write role", role)))
		default:
			rr.Findings = append(rr.Findings, newFinding(CodeBotRoleBelowWrite, fmt.Sprintf("bot role is %s, below the expected write role", role)))
		}
	}

	if !haveBranch {
		rr.Findings = append(rr.Findings, newFinding(CodeNoDefaultBranch, "repo has no default branch; branch-protection check skipped"))
		return
	}
	if bpErr != nil {
		// D6/D3 fail-closed: a read error escalates to a BLOCK finding. This changes
		// only the badge tier in M1 (was a Warning), not any run — no gate reads it
		// until M2.
		rr.Findings = append(rr.Findings, newFinding(CodeProtectionUnreadable, "could not read default-branch protection on this repo"))
		return
	}
	// Protected FIRST (R3/R12) — see the doc comment. An unprotected branch stops
	// here with the strongest finding; nothing below is reached for it.
	if !bp.Protected {
		rr.Findings = append(rr.Findings, newFinding(CodeDefaultBranchUnprotected, fmt.Sprintf("default branch %q is not protected", repo.DefaultBranch)))
		return
	}
	// ProtectionUnverified (PRD #238 D6/H1): the branch IS protected, but the
	// driver could not authoritatively read who may push/merge — the GitHub
	// legacy-branch-protection case, where /protection 403s a write bot and no
	// ruleset illuminates it. The driver reports WriteRoleCanPush/Merge=false
	// (never a fabricated true), so the push/merge branches below correctly do not
	// fire; without this the "undetermined" state would read as clean. "Could not
	// tell" is fail-closed the same as a read error (D3), so #66 folds it into the
	// protection_unreadable BLOCK code. It does NOT early-return: the Can* fields
	// are false here so nothing else fires, but the structure is preserved.
	if bp.ProtectionUnverified {
		rr.Findings = append(rr.Findings, newFinding(CodeProtectionUnreadable, fmt.Sprintf("could not fully verify push/merge rights on protected %q", repo.DefaultBranch)))
	}
	// Push/merge findings all BLOCK (D6): a bot that can push or merge into
	// protected main breaks the primary directive. M1 only surfaces them as coded
	// findings; the REFUSAL is deferred to M2 — nothing gates a run on these yet.
	if bp.WriteRoleCanPush {
		rr.Findings = append(rr.Findings, newFinding(CodeWriteRoleCanPush, fmt.Sprintf("the write role may push to protected %q", repo.DefaultBranch)))
	}
	if bp.BotCanPush {
		rr.Findings = append(rr.Findings, newFinding(CodeBotCanPush, fmt.Sprintf("the bot has a direct push grant on protected %q", repo.DefaultBranch)))
	}
	if bp.WriteRoleCanMerge {
		rr.Findings = append(rr.Findings, newFinding(CodeWriteRoleCanMerge, fmt.Sprintf("the write role may merge into protected %q", repo.DefaultBranch)))
	}
	if bp.BotCanMerge {
		rr.Findings = append(rr.Findings, newFinding(CodeBotCanMerge, fmt.Sprintf("the bot has a direct merge grant on protected %q", repo.DefaultBranch)))
	}
}

// BlocksRun reports whether the bot's rights on a protected-branch reading mean
// uzi must refuse (PRD #66 D3/D6). Protected FIRST (R3 inversion guard): an
// unprotected default branch is the worst case and short-circuits true; only on
// a protected branch are the push/merge fields consulted. This is the structured
// counterpart of the coded-findings BLOCK set M1/M2 build on — keep the two in
// agreement.
//
// ProtectionUnverified is deliberately NOT OR'd in here: that is the fail-closed
// error/unreadable path (a read the driver could not authoritatively resolve),
// which the CALLER handles by treating the whole read as unevaluable — not a bp
// field this predicate consumes.
func BlocksRun(bp forge.BranchProtection) bool {
	if !bp.Protected {
		return true
	}
	return bp.WriteRoleCanPush || bp.BotCanPush || bp.WriteRoleCanMerge || bp.BotCanMerge
}

// evaluateToken applies the token rules to introspection data + the admin flag.
// It is a pure function (no forge calls) so the rule matrix is trivially fixture
// -tested. introspectionWarn, when non-empty, means scopes/active/expiry could
// not be read: it is recorded as a warning and those three checks are skipped,
// but the admin check (which rides on VerifyToken) still runs.
func evaluateToken(forgeType forge.Type, info forge.TokenInfo, isAdmin bool, now time.Time, warnWindow time.Duration, introspectionWarn string) TokenReport {
	tr := TokenReport{Scopes: info.Scopes, Active: info.Active, Violations: []string{}, Warnings: []string{}}
	if tr.Scopes == nil {
		tr.Scopes = []string{}
	}
	if !info.ExpiresAt.IsZero() {
		e := info.ExpiresAt
		tr.ExpiresAt = &e
	}

	if isAdmin {
		tr.Violations = append(tr.Violations, "bot user is an instance admin")
	}

	if introspectionWarn != "" {
		tr.Warnings = append(tr.Warnings, introspectionWarn)
		return tr
	}

	if !scopesAcceptable(forgeType, info.Scopes) {
		tr.Violations = append(tr.Violations, fmt.Sprintf("token scopes %v are not acceptable: they must include %v and add nothing beyond the allowed optional scopes %v", info.Scopes, requiredScopesFor(forgeType), allowedOptionalScopesFor(forgeType)))
	}
	if !info.Active {
		tr.Violations = append(tr.Violations, "token is not active")
	}
	if !info.ExpiresAt.IsZero() {
		switch {
		case !info.ExpiresAt.After(now):
			tr.Violations = append(tr.Violations, "token has expired")
		case info.ExpiresAt.Before(now.Add(warnWindow)):
			days := int(warnWindow.Hours() / 24)
			tr.Warnings = append(tr.Warnings, fmt.Sprintf("token expires within %d days (on %s)", days, info.ExpiresAt.Format("2006-01-02")))
		}
	}
	return tr
}

// requiredScopesFor is the exact PAT scope set uzi's bot must carry on the given
// forge — no more, no less. Fewer would have failed VerifyToken/ListProjects
// already; more is over-privilege that PRD #5 blocks at save. GitLab's api is one
// coarse scope; Forgejo has no api scope at all — its model is category-based, so
// the least-privilege equivalent is three finer scopes: write:repository (PRs +
// the worker's branch push), write:issue (the board), read:user (token/identity
// introspection). Verified minimal and sufficient in D6b.
func requiredScopesFor(t forge.Type) []string {
	switch t {
	case forge.TypeForgejo:
		return []string{"read:user", "write:issue", "write:repository"}
	case forge.TypeGitHub:
		// GitHub classic PAT: the single coarse `repo` scope is REQUIRED (PRD #238
		// D7). It is no longer the ONLY acceptable scope, though — see
		// allowedOptionalScopesFor: PRD #364's board↔Projects v2 sync needs the PAT to
		// also carry `project`, so the acceptance test below is a bounded superset,
		// not exact equality. Over-privilege outside that named allowance
		// ({repo, workflow}, {repo, delete_repo}) still blocks at save (D7a).
		return []string{"repo"}
	default:
		return []string{"api"}
	}
}

// allowedOptionalScopesFor returns the scopes a forge type ACCEPTS but does not
// REQUIRE — permitted when present, never demanded. This is the minimal, deliberate
// widening of the over-privilege guard PRD #364 needs: GitHub's board↔Projects v2
// Status sync (F4) mutates a Projects v2 board, which the classic PAT can only do
// when it carries `project` (or the read-only `read:project`), so those two must
// pass the save-time scope gate rather than being rejected as over-privilege. It is
// scoped to GitHub ALONE: GitLab and Forgejo have no Projects v2 equivalent, so
// their optional set is empty and required ∪ ∅ == required leaves their acceptance
// exactly the old set-equality (their behavior must not change).
func allowedOptionalScopesFor(t forge.Type) []string {
	switch t {
	case forge.TypeGitHub:
		return []string{"project", "read:project"}
	default:
		return nil
	}
}

// scopesAcceptable reports whether scopes is a permissible set for the forge,
// compared as an unordered set (never a string compare: Forgejo re-emits a token's
// scopes in its own canonical order, not mint order — D6b). A set is acceptable iff
// it is a SUPERSET of the required set AND a SUBSET of required ∪ allowedOptional:
//
//   - superset of required — fewer would have failed VerifyToken/ListProjects
//     already, so a missing required scope is rejected here too;
//   - subset of required ∪ allowedOptional — anything BEYOND the required scopes and
//     the named optional allowance is over-privilege PRD #5 blocks at save.
//
// This REPLACES the old exact set-equality (scopesEqualRequired). For GitLab and
// Forgejo allowedOptionalScopesFor is empty, so required ∪ ∅ == required and the
// two bounds collapse back to equality — their behavior is unchanged. Only GitHub
// gains the {project, read:project} allowance (PRD #364): {repo}, {repo, project}
// and {repo, read:project} pass, while {repo, workflow} and {repo, delete_repo}
// still fail (workflow/delete_repo are not in the optional set — D7a is preserved).
//
// The god-mode ["all"] Forgejo token — which collapses every write:* scope into one
// literal — is still rejected for free: "all" is neither a required scope nor an
// optional one, so it fails the subset half, and this never tries to expand it.
func scopesAcceptable(t forge.Type, scopes []string) bool {
	req := requiredScopesFor(t)
	allowed := map[string]struct{}{}
	for _, r := range req {
		allowed[r] = struct{}{}
	}
	for _, o := range allowedOptionalScopesFor(t) {
		allowed[o] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, s := range scopes {
		seen[s] = struct{}{}
	}
	// Superset of required: every required scope must be present.
	for _, r := range req {
		if _, ok := seen[r]; !ok {
			return false
		}
	}
	// Subset of required ∪ allowedOptional: no scope beyond the allowed set.
	for s := range seen {
		if _, ok := allowed[s]; !ok {
			return false
		}
	}
	return true
}
