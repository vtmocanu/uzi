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

import (
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
)

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

// Code is the enumerable identifier of a per-repo finding (PRD #66 D5/D6). It
// replaces the old free-text per-repo Violations/Warnings so the blocking set is
// enumerable in code, not read out of prose. Severity is NEVER hand-set at the
// call site — it is looked up in findingSeverity, the single source of truth.
type Code string

const (
	// CodeDefaultBranchUnprotected: the repo's default branch is not protected at
	// all — the bot can push and merge freely (D6, BLOCK).
	CodeDefaultBranchUnprotected Code = "default_branch_unprotected"
	// CodeWriteRoleCanPush: the protected default branch admits the write role to
	// push directly (D6, BLOCK).
	CodeWriteRoleCanPush Code = "write_role_can_push"
	// CodeBotCanPush: a per-user allow-to-push grant lets the bot push directly to
	// the protected default branch (D6, BLOCK).
	CodeBotCanPush Code = "bot_can_push"
	// CodeWriteRoleCanMerge: the protected default branch admits the write role to
	// merge (D6, BLOCK).
	CodeWriteRoleCanMerge Code = "write_role_can_merge"
	// CodeBotCanMerge: a per-user merge grant lets the bot merge its own PR into
	// the protected default branch (D6, BLOCK).
	CodeBotCanMerge Code = "bot_can_merge"
	// CodeUnprotectedFilePatterns: a non-empty unprotected-file-patterns list means
	// the bot can push commits touching only those paths to main (D6/D7, BLOCK).
	// Forward-declared per D6: no driver reports it today (Forgejo-specific), so it
	// is never emitted by evaluateRepo yet.
	CodeUnprotectedFilePatterns Code = "unprotected_file_patterns"
	// CodeProtectionUnreadable: the branch-protection read errored, or the branch
	// is protected but the driver could not authoritatively read who may push/merge
	// (the GitHub legacy case). "Could not tell" is fail-closed the same as a read
	// error (D3, BLOCK).
	CodeProtectionUnreadable Code = "protection_unreadable"

	// CodeBotNotMember: the bot is no longer a member of this repo (D6, warn — the
	// bot is too weak, not too strong).
	CodeBotNotMember Code = "bot_not_member"
	// CodeBotRoleBelowWrite: the bot's role is below the expected write role (D6,
	// warn).
	CodeBotRoleBelowWrite Code = "bot_role_below_write"
	// CodeBotRoleAboveWrite: the bot's role is above the expected write role (D6,
	// warn — an admin/owner bot is already caught by the token gate at save).
	CodeBotRoleAboveWrite Code = "bot_role_above_write"
	// CodeGroupPushGrantUndetected: a group/team push whitelist uzi cannot resolve
	// to the bot (D6, warn). Forward-declared per D6: no driver reports it today
	// (documented gap), so it is never emitted by evaluateRepo yet.
	CodeGroupPushGrantUndetected Code = "group_push_grant_undetected"
	// CodeRoleUnreadable: the bot's role on the repo could not be read (warn — the
	// role check was skipped, distinct from an authoritative "not a member").
	CodeRoleUnreadable Code = "role_unreadable"
	// CodeNoDefaultBranch: the repo has no default branch (empty project), so the
	// branch-protection check was skipped (warn).
	CodeNoDefaultBranch Code = "no_default_branch"
)

// Severity is a finding's tier. It is set only via findingSeverity, never by a
// call site (D5).
type Severity string

const (
	// SeverityBlock is a finding that breaks the primary directive — the bot can
	// reach the default branch, or uzi could not tell (fail-closed). This is the
	// enumerable set M2 will refuse runs on.
	SeverityBlock Severity = "block"
	// SeverityWarn is an advisory finding — drift worth surfacing that does not, on
	// its own, mean the bot can reach the default branch.
	SeverityWarn Severity = "warn"
	// SeverityOverridden is a non-blocking tier a BLOCK finding is downgraded to
	// when an admin has allowed the repo through the guardrail (PRD #66 D8). It is
	// produced only by DowngradeOverridden, never looked up in findingSeverity, and
	// RepoReport.Blocks() (which tests == SeverityBlock) treats it as non-blocking —
	// so an overridden finding is recorded, rendered, and audited, but does not
	// refuse a run.
	SeverityOverridden Severity = "overridden"
)

// findingSeverity is the SINGLE source of a finding's severity (D5). A Code
// missing from this map is a programming error — a test asserts every declared
// Code is present. Never hand-set Severity at a call site; go through newFinding.
var findingSeverity = map[Code]Severity{
	CodeDefaultBranchUnprotected: SeverityBlock,
	CodeWriteRoleCanPush:         SeverityBlock,
	CodeBotCanPush:               SeverityBlock,
	CodeWriteRoleCanMerge:        SeverityBlock,
	CodeBotCanMerge:              SeverityBlock,
	CodeUnprotectedFilePatterns:  SeverityBlock,
	CodeProtectionUnreadable:     SeverityBlock,
	CodeBotNotMember:             SeverityWarn,
	CodeBotRoleBelowWrite:        SeverityWarn,
	CodeBotRoleAboveWrite:        SeverityWarn,
	CodeGroupPushGrantUndetected: SeverityWarn,
	CodeRoleUnreadable:           SeverityWarn,
	CodeNoDefaultBranch:          SeverityWarn,
}

// Finding is one coded per-repo finding: a stable Code, its Severity taken from
// findingSeverity, and a human message. Replaces the old free-text
// Violations/Warnings string slices (D5).
type Finding struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// newFinding builds a Finding with its severity looked up from findingSeverity —
// the only way severity is set (D5). A Code absent from the map yields the empty
// severity, which the coverage test forbids.
func newFinding(code Code, msg string) Finding {
	return Finding{Code: code, Severity: findingSeverity[code], Message: msg}
}

// TokenReport is the token-level half of a Report. Violations block a save;
// warnings never do.
type TokenReport struct {
	Scopes     []string   `json:"scopes"`
	Active     bool       `json:"active"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Violations []string   `json:"violations"`
	Warnings   []string   `json:"warnings"`
}

// RepoReport is one enabled repo's findings. Per-repo findings never block a save
// (only token violations do) — they are drift observed after the fact — but a
// SeverityBlock finding breaks the primary directive and counts toward the
// connection's violations status (and, from PRD #66 M2, will refuse a run).
type RepoReport struct {
	// RepoID is the uzi repo row id (uuid string), so the UI can badge the exact
	// Repos-page row.
	RepoID string `json:"repo_id"`
	Path   string `json:"path"`
	// Role is the bot's effective forge-neutral role ("write" is the compliant
	// one); RoleNone when the bot is not a member. This field was an int access
	// level until PRD #65 (D7) — reports written before then hold a number here
	// and no longer unmarshal, so they read blank until the next privilege sweep
	// re-stamps them.
	Role forge.Role `json:"role"`
	// Member is false when the bot has no effective membership on the repo.
	Member bool `json:"member"`
	// Findings is the coded per-repo finding set (PRD #66 D5), replacing the old
	// free-text Violations/Warnings slices. Never nil (JSON serializes []Finding{}
	// as [] rather than null).
	Findings []Finding `json:"findings"`
}

// Blocks reports whether this repo carries any SeverityBlock finding — the
// enumerable "does this repo block" predicate M2's gate consumes (PRD #66 D5).
// M1 does not call it from any gate; nothing refuses yet.
func (rr RepoReport) Blocks() bool {
	for _, f := range rr.Findings {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}

// BlockMessages returns the human messages of exactly this result's SeverityBlock
// findings — the reasons a run is refused, for the 422 "violations" array (PRD #66
// D1). Overridden and warn findings are excluded: only findings that actually
// refuse report as the reason. Provided here so the run-create service and the
// handlers do not each re-filter res.Findings. Never nil.
func (r GuardResult) BlockMessages() []string {
	msgs := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		if f.Severity == SeverityBlock {
			msgs = append(msgs, f.Message)
		}
	}
	return msgs
}

// waivableCodes is exactly the six "bot is too strong" BLOCK codes an admin
// per-repo override may downgrade (PRD #66 D8). CodeProtectionUnreadable is
// deliberately NOT here: a read error / unreadable protection is never waivable
// (D3/D8/R8) — a hostile or erroring forge must still refuse even an allowed
// repo. The warn codes are absent too: they never blocked, so there is nothing
// to downgrade.
var waivableCodes = map[Code]bool{
	CodeDefaultBranchUnprotected: true,
	CodeWriteRoleCanPush:         true,
	CodeBotCanPush:               true,
	CodeWriteRoleCanMerge:        true,
	CodeBotCanMerge:              true,
	CodeUnprotectedFilePatterns:  true,
}

// DowngradeOverridden applies an admin per-repo guardrail override (PRD #66 D8)
// as a POST-evaluation severity downgrade. This is the SINGLE downgrade function
// both the live gate (Service.GuardRepo) and M8/M9's render path call, so the
// "what an override waives" contract lives in exactly one place and cannot drift.
//
// Contract:
//   - overridden == false: findings is returned UNCHANGED (a no-op).
//   - overridden == true: a NEW slice is returned; the input slice and its
//     Finding elements are never mutated. Each finding whose Code is in
//     waivableCodes AND whose current Severity is SeverityBlock has its Severity
//     set to SeverityOverridden in the copy. Every other finding —
//     CodeProtectionUnreadable (never waivable, D3/R8) and the warns — is copied
//     through untouched.
//
// It is a downgrade, never a skip: evaluateRepo must have already run
// (Protected-first, R3/R8) and produced its findings, so an unprotected main is
// seen as a default_branch_unprotected BLOCK and only then waived — never elided.
func DowngradeOverridden(findings []Finding, overridden bool) []Finding {
	if !overridden {
		return findings
	}
	out := make([]Finding, len(findings))
	for i, f := range findings {
		out[i] = f
		if f.Severity == SeverityBlock && waivableCodes[f.Code] {
			out[i].Severity = SeverityOverridden
		}
	}
	return out
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

// ImpactRepo is one enabled repo's line in the pre-flight guardrail impact scan
// (PRD #66 M3). Blocked and Unevaluable are mutually exclusive: an unevaluable
// repo is "unknown" (a forge read errored, or it has no default branch to read
// protection on), counted apart from Blocked and NOT read as safe (R1).
type ImpactRepo struct {
	RepoID       string `json:"repo_id"`
	Path         string `json:"path"`
	UserID       string `json:"user_id"` // owning connection's user, for admin cross-user view
	ConnectionID string `json:"connection_id"`
	Blocked      bool   `json:"blocked"`
	Unevaluable  bool   `json:"unevaluable"` // forge error — unknown, not safe
}

// ImpactReport is the result of a live, non-persisting scan (PRD #66 M3) of every
// enabled repo across every forge connection: how many would be refused under the
// new guardrail (the bot can push/merge to the default branch). It reports
// BlockedCount and UnevaluableCount separately so an empty count is never read as
// "zero affected" when it is really "could not tell" (R1).
type ImpactReport struct {
	CheckedAt        time.Time    `json:"checked_at"`
	EnabledRepoCount int          `json:"enabled_repo_count"`
	BlockedCount     int          `json:"blocked_count"`
	UnevaluableCount int          `json:"unevaluable_count"`
	Repos            []ImpactRepo `json:"repos"`
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
	// The token half still uses its Violations/Warnings slices; per-repo tiers now
	// derive from coded finding severities (PRD #66 D5): any SeverityBlock finding
	// is a violation, any SeverityWarn is a warning.
	for _, r := range repos {
		for _, f := range r.Findings {
			switch f.Severity {
			case SeverityBlock:
				violations = true
			case SeverityWarn:
				warnings = true
			}
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
