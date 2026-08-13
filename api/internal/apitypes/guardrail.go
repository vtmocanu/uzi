package apitypes

import "time"

// GuardrailImpactRepoDTO is one enabled repo's line in the pre-flight guardrail
// impact scan (PRD #66 M3). Its wire shape mirrors privcheck.ImpactRepo, which the
// admin endpoint emits directly. Blocked and Unevaluable are mutually exclusive:
// an unevaluable repo is "unknown" (a forge read errored, or it has no default
// branch), counted apart from blocked and never read as safe (R1).
type GuardrailImpactRepoDTO struct {
	RepoID       string `json:"repo_id"`
	Path         string `json:"path"`
	UserID       string `json:"user_id"`
	ConnectionID string `json:"connection_id"`
	Blocked      bool   `json:"blocked"`
	Unevaluable  bool   `json:"unevaluable"`
}

// GuardrailImpactDTO is the CLI/SPA view of the live, non-persisting guardrail
// impact scan (PRD #66 M3): how many enabled repos would be refused under the new
// guardrail. BlockedCount and UnevaluableCount are separate so an empty result is
// never read as "zero affected" when it is really "could not tell" (R1). Its wire
// shape mirrors privcheck.ImpactReport.
type GuardrailImpactDTO struct {
	CheckedAt        time.Time                `json:"checked_at"`
	EnabledRepoCount int                      `json:"enabled_repo_count"`
	BlockedCount     int                      `json:"blocked_count"`
	UnevaluableCount int                      `json:"unevaluable_count"`
	Repos            []GuardrailImpactRepoDTO `json:"repos"`
}

// BlockedRepoDTO is one row of the admin cross-user blocked-repos list (PRD #66 M9,
// D8): a repo that is currently blocked by the guardrail OR carries an active admin
// override — the admin's action set. Unlike the impact scan (M3), this reads the
// STORED privilege_report (cheap, display-appropriate), so it inherits R1's
// INTERVAL=0 caveat: a repo whose connection was never checked reads Blocked=false
// here, which is "unknown, not none" — the envelope's ChecksUnknown flags that.
type BlockedRepoDTO struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	OwnerID    string `json:"owner_id"`
	OwnerEmail string `json:"owner_email"`
	ForgeType  string `json:"forge_type"`
	// Blocked is the stored-report equivalent of guardrail_blocked on RepoDTO: the
	// repo's findings run through the SINGLE shared DowngradeOverridden then Blocks().
	// An overridden-and-otherwise-clean repo reads Blocked=false but still appears
	// (it is in the admin's action set); an overridden repo whose only finding is
	// protection_unreadable reads Blocked=TRUE (the override never waives it, D8/R8).
	Blocked bool `json:"blocked"`
	// BlockMessages are the human messages of exactly the post-downgrade SeverityBlock
	// findings — the reasons a run is refused right now. Empty for an overridden-clean
	// repo. Never null.
	BlockMessages []string `json:"block_messages"`
	// Override is the audit metadata when an override is active (reason/actor/at),
	// null otherwise. By carries the actor's email when resolvable, else the raw uuid.
	Override *GuardrailOverrideDTO `json:"guardrail_override"`
	// PrivilegeStatus / PrivilegeCheckedAt are the owning connection's last check
	// state, so the UI can say "unknown" for a repo whose connection was never checked
	// (PrivilegeStatus null) rather than implying it is clean (R1).
	PrivilegeStatus    *string    `json:"privilege_status"`
	PrivilegeCheckedAt *time.Time `json:"privilege_checked_at"`
}

// AdminBlockedReposDTO is the GET /api/admin/blocked-repos envelope (PRD #66 M9, D8).
// The list is repos that are blocked OR overridden across ALL users; the envelope
// carries ChecksUnknown so an empty or short list is never silently read as "none
// blocked" when it is really "could not tell" (R1, D8's INTERVAL=0 caveat).
type AdminBlockedReposDTO struct {
	Repos []BlockedRepoDTO `json:"repos"`
	// ChecksUnknown is true when at least one forge connection has never had a
	// privilege check run (privilege_status IS NULL, e.g. under
	// UZI_PRIVILEGE_CHECK_INTERVAL=0). The UI must then say the list may be
	// incomplete — empty is "unknown", not "none blocked".
	ChecksUnknown bool `json:"checks_unknown"`
}
