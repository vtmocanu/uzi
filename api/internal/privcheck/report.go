// Package privcheck verifies that a forge bot PAT is least-privilege: the token
// carries only the scopes uzi needs, the bot is not an instance admin, and on
// every enabled repo the bot is exactly a Developer on a protected default
// branch. It answers plan.md's three questions (at save, on demand, and on a
// schedule) and produces a Report the UI surfaces as badges + findings.
//
// The rules here are the *verified* half of PRD #4's "GitLab-side bot =
// Developer + protected main" guardrail assumption: this package turns that from
// documented-and-hoped into continuously-checked.
package privcheck

import "time"

// Status is the denormalized worst-case tier of a report, stored on
// forge_connections.privilege_status for cheap list queries and badges.
type Status string

const (
	// StatusOK means no findings at all.
	StatusOK Status = "ok"
	// StatusWarnings means advisory findings only (e.g. token nearing expiry) —
	// nothing that breaks the primary directive.
	StatusWarnings Status = "warnings"
	// StatusViolations means at least one finding that breaks the primary
	// directive (over-privilege, admin bot, unprotected main, wrong role).
	StatusViolations Status = "violations"
	// StatusError means the check itself could not complete (forge unreachable,
	// token revoked at the top level) — recorded in the report, never crashing
	// the sweep. Distinct from violations: we could not determine compliance.
	StatusError Status = "error"
)

// TokenReport is the token-level half of a Report. Violations block a save;
// warnings never do.
type TokenReport struct {
	Scopes     []string   `json:"scopes"`
	Active     bool       `json:"active"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Violations []string   `json:"violations"`
	Warnings   []string   `json:"warnings"`
}

// RepoReport is one enabled repo's findings. Per-repo findings always warn (in
// the sense that they never block a save) — they are drift observed after the
// fact — but role/branch problems are Violations (they break the directive) and
// count toward the connection's violations status.
type RepoReport struct {
	// RepoID is the uzi repo row id (uuid string), so the UI can badge the exact
	// Repos-page row.
	RepoID string `json:"repo_id"`
	Path   string `json:"path"`
	// Role is the bot's effective access level (30 = Developer). Zero when the
	// bot is not a member.
	Role int `json:"role"`
	// Member is false when the bot has no effective membership on the repo.
	Member     bool     `json:"member"`
	Violations []string `json:"violations"`
	Warnings   []string `json:"warnings"`
}

// Report is one connection's full privilege picture: the token plus every
// enabled repo, with a denormalized worst-case Status. One jsonb blob per
// connection — no normalized findings table, since this is current-state
// display, not an audit log.
type Report struct {
	CheckedAt time.Time    `json:"checked_at"`
	Token     TokenReport  `json:"token"`
	Repos     []RepoReport `json:"repos"`
	Status    Status       `json:"status"`
}

// Repo is the minimal enabled-repo view the checker needs. The caller maps a
// store.Repo onto it (keeping privcheck free of a store dependency).
type Repo struct {
	ID             string
	Path           string
	ForgeProjectID int64
	// DefaultBranch is the repo's default branch name; empty for an empty project
	// (branch-protection check is then skipped with a note).
	DefaultBranch string
}

// errorReport builds a StatusError report carrying a single top-level warning.
// Used when the check could not even run (forge unreachable, token revoked, or a
// connection whose client can't be built) — surfaced, never crashed on.
func errorReport(now time.Time, warning string) Report {
	return Report{
		CheckedAt: now,
		Token:     TokenReport{Scopes: []string{}, Violations: []string{}, Warnings: []string{warning}},
		Repos:     []RepoReport{},
		Status:    StatusError,
	}
}

// computeStatus derives the worst-case tier from the findings. It never returns
// StatusError — that tier is set explicitly by the error paths (a check that
// could not run), which computeStatus cannot represent.
func computeStatus(token TokenReport, repos []RepoReport) Status {
	violations := len(token.Violations) > 0
	warnings := len(token.Warnings) > 0
	for _, r := range repos {
		if len(r.Violations) > 0 {
			violations = true
		}
		if len(r.Warnings) > 0 {
			warnings = true
		}
	}
	switch {
	case violations:
		return StatusViolations
	case warnings:
		return StatusWarnings
	default:
		return StatusOK
	}
}
