package apitypes

import "time"

// AdminCLITokenDTO is one row of the factory-wide standing-credential inventory
// (admin read-only). It lives in apitypes rather than beside the per-user
// cliTokenDTO in the handler package for the reason that type's own comment gives:
// a DTO belongs here once a Go CLI verb decodes it, and `uzi admin cli-tokens`
// does. The per-user endpoint stays cookie-only and its DTO correctly stays put.
//
// THE TOKEN VALUE AND ITS HASH APPEAR IN NO FIELD, AND THREE SEPARATE MECHANISMS
// KEEP IT THAT WAY. The value is never stored at all (shown once at mint), so it
// cannot be listed. The hash is excluded by the QUERY's explicit projection, so it
// is not even in the Go row type this is built from. And TestAdminCLITokenDTOTags
// enumerates the exact JSON keys, so adding one fails the build rather than
// shipping. A sha256 of a credential is offline-crackable; an admin-wide list of
// them would convert this visibility feature into a disclosure surface.
//
// What IS here is the forensic surface the schema calls out (Risk 8): token_prefix
// names a row without revealing it, and last_used_at + last_used_ip are the only
// detection controls that exist — there is no per-request audit log.
type AdminCLITokenDTO struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	OwnerEmail  string `json:"owner_email"`
	Name        string `json:"name"`
	TokenPrefix string `json:"token_prefix"`
	Scope       string `json:"scope"`
	Revoked     bool   `json:"revoked"`
	// ExpiresAt is null for a never-expiring token — the webui-minted uzc_ that the
	// agent/CI path depends on. Null here is the row an operator most wants to see,
	// not missing data.
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastUsedIP *string    `json:"last_used_ip"`
	ExpiresAt  *time.Time `json:"expires_at"`
}
