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

// TokenRateLimitDTO is one TOKEN's meter (PRD #104 M5, D4): the credential's label
// and default flag — a name, never the value — plus the same status union PRD #53
// froze. secret_id lets a client key a rebind or a delete off the row; label is what
// it renders. This is the element GET /api/me/rate-limits now returns an ARRAY of,
// replacing the single reading — a breaking response-shape change the web client,
// the CLI, and the e2e assertions all move for in the same MR.
type TokenRateLimitDTO struct {
	SecretID  string `json:"secret_id"`
	Label     string `json:"label"`
	IsDefault bool   `json:"is_default"`
	// Auto-selection (PRD #111 M2). AutoEligible is the owner's opt-in — the setting
	// they toggled. AutoStatus is the LIVE answer: whether the selector could pick
	// this token right now, and if not, why (`eligible` | `not_pooled` |
	// `no_reading` | `unmeasured` | `stale` | `below_threshold`).
	//
	// 🔴 AutoStatus IS COMPUTED SERVER-SIDE AND MUST BE RENDERED, NEVER RE-DERIVED
	// (D21). It comes from autoselect.Classify, the same single function the ranker
	// gates candidates on, precisely so the settings page cannot promise a token is
	// eligible that the selector silently skips. A client that reconstructs it from
	// pct/synced_at reintroduces exactly the drift this field exists to remove, and
	// nothing would fail when the two disagreed.
	//
	// The pair is not redundant: a token can be opted IN and still unpickable (its
	// gauge never polled, or the reading aged out), which is R7's silent no-op and
	// the reason the status is surfaced at all rather than left implicit.
	AutoEligible bool         `json:"auto_eligible"`
	AutoStatus   string       `json:"auto_status"`
	Limits       RateLimitDTO `json:"limits"`
}

// AdminRateLimitRowDTO is one user's row on the admin view: identity + the live
// vault lock state + one meter PER TOKEN (PRD #104 M5, D4 — the admin view groups by
// user then token). Every user appears, including token-less ones, whose Tokens is
// empty and whose status the client renders as no_token.
type AdminRateLimitRowDTO struct {
	ID          string              `json:"id"`
	Email       string              `json:"email"`
	Name        string              `json:"name"`
	VaultLocked bool                `json:"vault_locked"`
	Tokens      []TokenRateLimitDTO `json:"tokens"`
}
