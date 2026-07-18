package privcheck

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
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
			warn = "cannot verify token scopes on this GitLab version (requires 15.5+)"
		}
		return evaluateToken(forgeType, forge.TokenInfo{}, isAdmin, now, c.warnWindow, warn)
	}
	return evaluateToken(forgeType, info, isAdmin, now, c.warnWindow, "")
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
	rr := RepoReport{RepoID: repo.ID, Path: repo.Path, Violations: []string{}, Warnings: []string{}}

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
		rr.Warnings = append(rr.Warnings, "could not read the bot's role on this repo")
	} else {
		rr.Role = role
		rr.Member = member
		switch {
		case !member:
			rr.Violations = append(rr.Violations, "bot is no longer a member of this repo; sync is broken")
		case role == forge.RoleWrite:
			// Exactly the role uzi wants: no finding.
		case role.AtLeast(forge.RoleWrite):
			rr.Violations = append(rr.Violations, fmt.Sprintf("bot role is %s, above the expected write role", role))
		default:
			rr.Violations = append(rr.Violations, fmt.Sprintf("bot role is %s, below the expected write role", role))
		}
	}

	if !haveBranch {
		rr.Warnings = append(rr.Warnings, "repo has no default branch; branch-protection check skipped")
		return
	}
	if bpErr != nil {
		rr.Warnings = append(rr.Warnings, "could not read default-branch protection on this repo")
		return
	}
	// Protected FIRST (R12) — see the doc comment. An unprotected branch stops
	// here with the strongest finding; nothing below is reached for it.
	if !bp.Protected {
		rr.Violations = append(rr.Violations, fmt.Sprintf("default branch %q is not protected", repo.DefaultBranch))
		return
	}
	// Push findings are violations — pre-existing PRD #5 behaviour, unchanged.
	if bp.WriteRoleCanPush {
		rr.Violations = append(rr.Violations, fmt.Sprintf("the write role may push to protected %q", repo.DefaultBranch))
	}
	if bp.BotCanPush {
		rr.Violations = append(rr.Violations, fmt.Sprintf("the bot has a direct push grant on protected %q", repo.DefaultBranch))
	}
	// Merge findings are violations, the same tier as the push sibling above and
	// for the same reason: a bot that can merge its own PR into protected main
	// breaks the primary directive exactly as one that can push directly to it
	// does (report.go reserves Violations for "branch problems that break the
	// directive"). This is NOT a new GitLab behaviour — the push sibling is
	// already a Violation that flips a GitLab badge under PRD #5, so surfacing a
	// merge one is the same existing tier, not a change #66 was split out to
	// avoid. What #66 defers is the REFUSAL, not the surfacing: a per-repo
	// Violation never blocks a save (only token violations do, report.go:37-38)
	// and nothing gates a run on privilege_status — verified, the run/claim path
	// never reads it. #66 promotes these fields to run-refusals via a
	// Protected-first read (evaluateRepo above), never via the badge tier.
	if bp.WriteRoleCanMerge {
		rr.Violations = append(rr.Violations, fmt.Sprintf("the write role may merge into protected %q", repo.DefaultBranch))
	}
	if bp.BotCanMerge {
		rr.Violations = append(rr.Violations, fmt.Sprintf("the bot has a direct merge grant on protected %q", repo.DefaultBranch))
	}
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

	if !scopesEqualRequired(forgeType, info.Scopes) {
		tr.Violations = append(tr.Violations, fmt.Sprintf("token scopes %v are not exactly %v", info.Scopes, requiredScopesFor(forgeType)))
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
	if t == forge.TypeForgejo {
		return []string{"read:user", "write:issue", "write:repository"}
	}
	return []string{"api"}
}

// scopesEqualRequired reports whether scopes is exactly the forge's required set,
// compared as an unordered set. It must NOT be a string compare: Forgejo re-emits
// a token's scopes in its own canonical order, not the order they were minted in,
// so the required three come back reordered (D6b). The god-mode ["all"] token —
// which Forgejo produces by collapsing every write:* scope into one literal — is
// rejected here for free: it is simply a set that does not equal the required
// three, and this never tries to expand it back out. Fewer scopes would have
// failed VerifyToken/ListProjects already; more is over-privilege.
func scopesEqualRequired(t forge.Type, scopes []string) bool {
	req := requiredScopesFor(t)
	seen := map[string]struct{}{}
	for _, s := range scopes {
		seen[s] = struct{}{}
	}
	if len(seen) != len(req) {
		return false
	}
	for _, r := range req {
		if _, ok := seen[r]; !ok {
			return false
		}
	}
	return true
}
