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
	// developerAccess is GitLab's Developer role access level: the exact level the
	// bot must hold on every enabled repo.
	developerAccess = 30
	// requiredScope is uzi's documented minimum PAT scope. Exactly this and
	// nothing more (see docs/gitlab-bot-setup.md); read_api is insufficient and
	// anything beyond api is over-privilege.
	requiredScope = "api"
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
func (c *Checker) CheckToken(ctx context.Context, f forge.Forge, isAdmin bool, now time.Time) TokenReport {
	info, err := f.TokenInfo(ctx)
	if err != nil {
		warn := "could not verify token scopes against the forge; scope/expiry checks were skipped"
		if errors.Is(err, forge.ErrTokenIntrospectionUnsupported) {
			warn = "cannot verify token scopes on this GitLab version (requires 15.5+)"
		}
		return evaluateToken(forge.TokenInfo{}, isAdmin, now, c.warnWindow, warn)
	}
	return evaluateToken(info, isAdmin, now, c.warnWindow, "")
}

// Check runs the full report: the token-level checks plus every enabled repo's
// role and default-branch protection, with bounded per-repo concurrency. It is
// error-as-report: if the token cannot even be verified against the forge
// (revoked, forge unreachable) the report is StatusError with that finding, not
// a returned error — a revoked token is exactly what the report must surface.
func (c *Checker) Check(ctx context.Context, f forge.Forge, repos []Repo, now time.Time) Report {
	identity, err := f.VerifyToken(ctx)
	if err != nil {
		// err is already PAT-redacted by the driver, but we keep the report
		// generic rather than embedding the raw forge error at all.
		return errorReport(now, "could not verify the bot token against the forge (revoked, expired, or forge unreachable)")
	}
	token := c.CheckToken(ctx, f, identity.IsAdmin, now)
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

	role, member, err := f.ProjectRole(ctx, repo.ForgeProjectID, botUserID)
	if err != nil {
		rr.Warnings = append(rr.Warnings, "could not read the bot's role on this repo")
	} else {
		rr.Role = role
		rr.Member = member
		switch {
		case !member:
			rr.Violations = append(rr.Violations, "bot is no longer a Developer member of this repo; sync is broken")
		case role > developerAccess:
			rr.Violations = append(rr.Violations, fmt.Sprintf("bot role is %s (%d), expected Developer (%d)", roleName(role), role, developerAccess))
		case role < developerAccess:
			rr.Violations = append(rr.Violations, fmt.Sprintf("bot role is %s (%d), below Developer (%d)", roleName(role), role, developerAccess))
		}
	}

	if repo.DefaultBranch == "" {
		rr.Warnings = append(rr.Warnings, "repo has no default branch; branch-protection check skipped")
		return rr
	}
	bp, err := f.DefaultBranchProtection(ctx, repo.ForgeProjectID, repo.DefaultBranch)
	if err != nil {
		rr.Warnings = append(rr.Warnings, "could not read default-branch protection on this repo")
		return rr
	}
	switch {
	case !bp.Protected:
		rr.Violations = append(rr.Violations, fmt.Sprintf("default branch %q is not protected", repo.DefaultBranch))
	case bp.DevelopersCanPush:
		rr.Violations = append(rr.Violations, fmt.Sprintf("Developers may push to protected %q", repo.DefaultBranch))
	}
	return rr
}

// evaluateToken applies the token rules to introspection data + the admin flag.
// It is a pure function (no forge calls) so the rule matrix is trivially fixture
// -tested. introspectionWarn, when non-empty, means scopes/active/expiry could
// not be read: it is recorded as a warning and those three checks are skipped,
// but the admin check (which rides on VerifyToken) still runs.
func evaluateToken(info forge.TokenInfo, isAdmin bool, now time.Time, warnWindow time.Duration, introspectionWarn string) TokenReport {
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

	if !scopesEqualRequired(info.Scopes) {
		tr.Violations = append(tr.Violations, fmt.Sprintf("token scopes %v exceed the required [%s]", info.Scopes, requiredScope))
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

// scopesEqualRequired reports whether the scope set is exactly {api}. Fewer would
// have failed VerifyToken/ListProjects already; more is over-privilege.
func scopesEqualRequired(scopes []string) bool {
	seen := map[string]struct{}{}
	for _, s := range scopes {
		seen[s] = struct{}{}
	}
	_, hasAPI := seen[requiredScope]
	return hasAPI && len(seen) == 1
}

// roleName maps a GitLab access level to a human label for finding messages.
func roleName(level int) string {
	switch level {
	case 10:
		return "Guest"
	case 20:
		return "Reporter"
	case 30:
		return "Developer"
	case 40:
		return "Maintainer"
	case 50:
		return "Owner"
	case 60:
		return "Admin"
	default:
		return fmt.Sprintf("access level %d", level)
	}
}
