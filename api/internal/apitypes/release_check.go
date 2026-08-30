package apitypes

// ReleaseCheckStatusDTO is the ADMIN release-check surface (PRD #836 M3): the data
// source for the admin Updates card and the response of both admin release-check
// endpoints. Unlike the world-readable BuildInfoDTO.Latest, this is served ONLY to a
// cookie-authenticated admin, so it carries the full persisted release Body (the
// markdown notes the card previews) — that field must never migrate onto an
// unauthenticated response.
//
// The derived booleans (UpdateAvailable / FarBehind / Security) are plain bools here,
// not pointers: this endpoint is admin-only and always returns the complete picture,
// and Status ("disabled" / "never" / "ok" / "error") already carries the "has a check
// run?" distinction the *bool pointers preserve on the public endpoint. No token is
// ever serialized.
type ReleaseCheckStatusDTO struct {
	// The two runtime toggles + poll cadence, read live from settings.
	Enabled       bool   `json:"release_check_enabled"`
	BannerEnabled bool   `json:"release_check_banner_enabled"`
	Interval      string `json:"interval"`

	// RunningVersion is this instance's own served version (bare, "dev" on an
	// un-stamped build) — the left-hand side of the update delta the card renders.
	RunningVersion string `json:"running_version"`

	// The persisted remote facts. Body is the RAW release markdown (admin-only): the
	// card excerpts/truncates it and scans it for the "### Security" heading. Every
	// fact is omitted when unknown (no check has run yet).
	LatestTag   string `json:"latest_tag,omitempty"`
	LatestName  string `json:"latest_name,omitempty"`
	Body        string `json:"body,omitempty"`
	NotesURL    string `json:"notes_url,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	CheckedAt   string `json:"checked_at,omitempty"`

	// Read-time derivations over the facts + RunningVersion (PRD #836 M1). All false
	// until a check has run and a newer release exists.
	UpdateAvailable bool `json:"update_available"`
	FarBehind       bool `json:"far_behind"`
	Security        bool `json:"security"`

	// BannerSnoozed reports whether the admin has snoozed the escalation banner for the
	// CURRENT release (PRD #836 M6): true iff a snooze tag is set AND equals LatestTag.
	// Because a newer upstream release changes LatestTag, the snooze auto-expires when a
	// newer release arrives (the snooze tag no longer matches), with no admin action.
	// Admin-only DTO, so a plain bool: the banner (surface 4) reads it to stay hidden
	// after a dismiss. It does NOT live on the public BuildInfoDTO — the banner is
	// admin-only.
	BannerSnoozed bool `json:"banner_snoozed"`

	// Status is "disabled" (master toggle off), "never" (enabled but no check has run),
	// "ok" (facts present), or "error" (the last "Check now" failed). Message carries a
	// token-scrubbed reason on an error status, empty otherwise.
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
