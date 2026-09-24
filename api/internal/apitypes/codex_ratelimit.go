package apitypes

// Codex per-ACCOUNT rate-limit DTOs (PRD #1209). The Claude/Anthropic meter DTOs in
// ratelimit.go are frozen and unrelated; Codex reports a NESTED bucket shape (a set of
// named buckets, each with up to two windows) rather than Anthropic's flat five-hour /
// seven-day pair, so it gets its own types. These carry ONLY labels, flags and the
// reading — never a token, a login blob, or a raw provider/account principal id (the
// account_id is uzi's own internal uuid, safe to expose as a client key).

// CodexRateLimitWindowDTO is one utilization window of a Codex bucket: the used percent
// (0..100, nullable when Codex reported none), the window length, and the reset timing in
// two forms Codex may report (seconds-until and/or an absolute epoch). Every field is a
// pointer so an absent sub-field serializes as null rather than a misleading zero — the
// contract keeps them PRESENT (no omitempty).
type CodexRateLimitWindowDTO struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

// CodexRateLimitBucketDTO is one named Codex rate-limit bucket. id is the bucket's stable
// key; display_name is a human label (omitempty — Codex may not name every bucket).
// allowed / limit_reached are the bucket's boolean signals (nullable when unreported).
// primary / secondary are the up-to-two windows the bucket carries (null when absent), the
// nested shape Codex reports.
type CodexRateLimitBucketDTO struct {
	ID           string                   `json:"id"`
	DisplayName  string                   `json:"display_name,omitempty"`
	Allowed      *bool                    `json:"allowed"`
	LimitReached *bool                    `json:"limit_reached"`
	Primary      *CodexRateLimitWindowDTO `json:"primary"`
	Secondary    *CodexRateLimitWindowDTO `json:"secondary"`
}

// CodexAccountRateLimitDTO is one Codex subscription account's meter (PRD #1209): the
// account's uzi id (a client key, never the raw provider principal), the linked-alias
// labels that name WHICH account this is, whether it is the user's default codex
// credential, the derived status, the last successful reading time (omitempty), a stale
// flag (omitempty — set true when a retained reading has aged past 3x the poll interval,
// and also when polling is disabled so any retained reading counts as stale), and the
// nested buckets.
//
// status is a CLOSED set derived server-side (M3), never re-derived by a client:
//
//	no_subscription | pending | no_reading | fresh | stale | vault_locked |
//	credential_action_required | polling_disabled
//
// reason (omitempty) says WHY a credential_action_required account needs action. It is a
// CLOSED set, set only when status is credential_action_required (issue #1594):
//
//	provider_rejected — the saved Codex login was rejected by the provider; add a login
//	                    used only by uzi.
//
// It is absent for every other status and for a re-login flagged for any other reason.
type CodexAccountRateLimitDTO struct {
	AccountID     string                    `json:"account_id"`
	Aliases       []string                  `json:"aliases"`
	IsDefault     bool                      `json:"is_default"`
	Status        string                    `json:"status"`
	Reason        string                    `json:"reason,omitempty"`
	LastSuccessAt string                    `json:"last_success_at,omitempty"`
	Stale         *bool                     `json:"stale,omitempty"`
	Buckets       []CodexRateLimitBucketDTO `json:"buckets"`
}

// CodexAdminRateLimitRowDTO is one user's row on the admin Codex view (PRD #1209): identity
// + the live vault-lock state (computed in-memory, not stored) + one meter PER ACCOUNT.
// Mirrors AdminRateLimitRowDTO's shape for the Anthropic side.
type CodexAdminRateLimitRowDTO struct {
	ID          string                     `json:"id"`
	Email       string                     `json:"email"`
	Name        string                     `json:"name"`
	VaultLocked bool                       `json:"vault_locked"`
	Accounts    []CodexAccountRateLimitDTO `json:"accounts"`
}
