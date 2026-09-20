package apitypes

// Admin-health wire types (PRD #1484). The response of GET /api/admin/health, in the
// RequireUser + RequireAdminRO group. Admin-only BY ROUTE: the document carries owner
// names, worker names and repo paths, and it must never migrate onto an unauthenticated
// response (the same standing property ReleaseCheckStatusDTO records). Every summary,
// action and command is composed by the server from a fixed template per check id plus
// numbers, closed enums and already-sanitized identifiers — never free text from
// Kubernetes, a forge, a run or a worker.

// HealthDocDTO is the whole health document: an overall verdict, the per-severity tally,
// the caller's own snooze/episode coordinates, and the full check registry in a stable
// order.
type HealthDocDTO struct {
	// Status is the overall verdict: the worst of the checks, "danger" then "warn", with
	// "unknown" ranking as "warn" for the rollup. A closed enum:
	// ok | warn | danger | unknown. "na" is never an overall status (an all-na, all-ok
	// instance is "ok").
	Status string `json:"status"`
	// CheckedAt is the evaluation time (RFC3339, UTC). The web card renders "Checked N s
	// ago" from it.
	CheckedAt string `json:"checked_at"`
	// Counts is the per-severity tally over Checks.
	Counts HealthCountsDTO `json:"counts"`
	// SnoozedUntil is the caller's own banner snooze against the open episode (RFC3339),
	// or null. Always null in M1 — the episode/snooze machinery lands in M2 — but the
	// field ships now so the wire shape is stable.
	SnoozedUntil *string `json:"snoozed_until"`
	// EpisodeID is the open danger episode's id, or null. Always null in M1 (episodes are
	// M2); the banner keys its snooze on it.
	EpisodeID *string `json:"episode_id"`
	// Checks is the full registry, always present and in a stable order (never a subset).
	Checks []HealthCheckDTO `json:"checks"`
}

// HealthCountsDTO is the per-severity tally over the registry.
type HealthCountsDTO struct {
	OK      int `json:"ok"`
	Warn    int `json:"warn"`
	Danger  int `json:"danger"`
	Unknown int `json:"unknown"`
	NA      int `json:"na"`
}

// HealthCheckDTO is one check's verdict plus its server-authored evidence and
// what-to-do line.
type HealthCheckDTO struct {
	// ID and Group are closed enums (e.g. "fleet.roll" / "workers"). Severity is one of
	// ok | warn | danger | unknown | na.
	ID       string `json:"id"`
	Group    string `json:"group"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	// Summary is the one-line server-authored verdict for this check.
	Summary string `json:"summary"`
	// Since is when this check first went non-ok, where the source carries one (RFC3339),
	// else null.
	Since *string `json:"since"`
	// Evidence is a small set of labelled facts (always present, possibly empty).
	Evidence []HealthEvidenceDTO `json:"evidence"`
	// Action is the what-to-do line, Command a fixed-template diagnostic command with
	// literal placeholders (e.g. <worker-namespace>), Doc a docs slug — each null when the
	// check has none.
	Action  *string `json:"action"`
	Command *string `json:"command"`
	Doc     *string `json:"doc"`
}

// HealthEvidenceDTO is one labelled fact under a check.
type HealthEvidenceDTO struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
