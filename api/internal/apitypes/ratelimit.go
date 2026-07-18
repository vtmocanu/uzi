package apitypes

// RateLimitWindow is one window of the frozen DTO (PRD #53): a 0..100 percent plus
// the reset as epoch seconds, or null when Anthropic reported none. resets_at has
// no omitempty — the contract keeps it PRESENT as `null` inside every window.
type RateLimitWindow struct {
	Pct      int    `json:"pct"`
	ResetsAt *int64 `json:"resets_at"`
}

// RateLimitDTO is the frozen union discriminated on status. Only the "ok" branch
// carries the windows/source/synced_at/stale; "no_token" and "unavailable" are
// status-only — omitempty drops the ok-only fields so they serialize to exactly
// {"status": "..."}. Exposes ONLY pct/resets_at/status/source/synced_at/stale, per
// the frozen contract (no is_admin, no internal fields).
type RateLimitDTO struct {
	Status   string           `json:"status"` // ok | no_token | unavailable
	FiveHour *RateLimitWindow `json:"five_hour,omitempty"`
	SevenDay *RateLimitWindow `json:"seven_day,omitempty"`
	Source   string           `json:"source,omitempty"`
	SyncedAt string           `json:"synced_at,omitempty"`
	Stale    *bool            `json:"stale,omitempty"`
}

// AdminRateLimitRowDTO is one user's row on the admin view: identity + the live
// vault lock state + the same union as /me. Every user appears, including
// no_token ones.
type AdminRateLimitRowDTO struct {
	ID          string       `json:"id"`
	Email       string       `json:"email"`
	Name        string       `json:"name"`
	VaultLocked bool         `json:"vault_locked"`
	Limits      RateLimitDTO `json:"limits"`
}
