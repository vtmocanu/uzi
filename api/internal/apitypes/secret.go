package apitypes

import "time"

// SecretDTO is the metadata-only view of one stored user secret (PRD #104 M2). It
// is the shape GET /api/me/secrets returns an array of, and the shape the CLI
// (`uzi token list`) decodes. The secret VALUE appears in no field — there is no
// reveal endpoint; a token is rotated by re-pasting, never read back.
type SecretDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	IsDefault bool   `json:"is_default"`
	// AutoEligible is the owner's opt-in to the auto-selection pool (PRD #111 M2,
	// D2): an `auto` worker's claim spends only tokens flagged here. Default false —
	// opting a token in is a deliberate act, because a pool that helps itself to
	// every credential would spend the one a user reserved for something else.
	//
	// This is the SETTING, not the live answer. Whether the selector could actually
	// pick this token right now additionally depends on its gauge reading (fresh?
	// measurable? above the headroom floor?), which this DTO does not carry — see
	// TokenRateLimitDTO.AutoStatus, which does, and which is computed server-side so
	// no client re-derives it.
	AutoEligible bool      `json:"auto_eligible"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// CodexStatus is the codex_credential_state link status for a codex-kind row
	// (PRD #1147 M1): 'staging' (a codex_auth awaiting an account link), 'linked' (an
	// account resolved and bound), 'failed' (link resolution errored), or 'static' (a
	// standalone openai_api_key with no subscription lifecycle). Empty and omitted for
	// anthropic_token rows, which carry no such state — so a client reads its presence
	// as "this is a Codex credential" and its value as where that credential stands.
	CodexStatus string `json:"codex_status,omitempty"`
}
