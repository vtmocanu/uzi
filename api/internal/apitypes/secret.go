package apitypes

import "time"

// SecretDTO is the metadata-only view of one stored user secret (PRD #104 M2). It
// is the shape GET /api/me/secrets returns an array of, and the shape the CLI
// (`uzi token list`) decodes. The secret VALUE appears in no field — there is no
// reveal endpoint; a token is rotated by re-pasting, never read back.
type SecretDTO struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
