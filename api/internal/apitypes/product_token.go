package apitypes

import "time"

// Product tokens and the product registry (PRD #1907). These DTOs are the wire seam
// for the admin registry (M4), the per-user mint/list (M5) and GET /api/v1/whoami
// (M3). They live in apitypes from the start so the PRD #982 contract fixtures pin
// them before any handler returns them.
//
// THE TOKEN HASH APPEARS IN NO FIELD, for the reason AdminCLITokenDTO gives: every
// product_tokens query projects its columns explicitly without token_hash (see
// queries/product_tokens.sql), so the hash is not even in the Go row types these are
// built from, and TestProductTokenDTOTags / TestAdminProductTokenDTOTags enumerate
// the exact JSON keys so adding one fails the build. The token VALUE appears exactly
// once, in MintProductTokenResponse.Token, at mint; it is never stored.

// ProductDTO is one registered external product. DeletedAt is null for a live
// product; a soft-deleted product (PRD #1907 D9) is always disabled and stays
// listed for the audit trail. ActiveTokenCount is the number of its tokens that are
// neither revoked nor expired, so the delete confirm can say how many it stops.
type ProductDTO struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	Enabled          bool       `json:"enabled"`
	DeletedAt        *time.Time `json:"deleted_at"`
	CreatedAt        time.Time  `json:"created_at"`
	ActiveTokenCount int64      `json:"active_token_count"`
	AllowedJobTypes  []string   `json:"allowed_job_types"`
}

// AdminDeleteProductResponse is the DELETE /api/admin/products/{id} response (PRD #1907
// M4): the soft-deleted product plus StoppedTokenCount, the number of its tokens this
// delete made unusable. That is the product's active (not revoked, not expired) tokens
// when the product was enabled at deletion, and 0 when it was already disabled: those
// tokens were already refused, so the delete stopped none of them. Product's own
// active_token_count keeps its registry meaning (active tokens, whatever the product's
// state), so the two differ exactly for an already-disabled product.
type AdminDeleteProductResponse struct {
	Product           ProductDTO `json:"product"`
	StoppedTokenCount int64      `json:"stopped_token_count"`
}

// MintableProductDTO is one entry of the user mint picker (GET
// /api/me/product-tokens/products, PRD #1907 M5): an enabled, live product a user may
// mint a token for. Deliberately only the three fields the picker shows: no token
// counts, no creator, no state (every listed product is enabled and not deleted).
type MintableProductDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ProductTokenDTO is the metadata-only view of one product token, the per-user
// list row (Settings > Access). As for CLI tokens, token_prefix + last_used_at +
// last_used_ip are the whole forensic surface: there is no per-request audit log.
// last_used_at, last_used_ip and expires_at are null until set; a null expires_at
// is a never-expiring token (PRD #1907 D10). Scopes is never null on the wire (the
// column is NOT NULL and non-empty).
type ProductTokenDTO struct {
	ID          string     `json:"id"`
	ProductID   string     `json:"product_id"`
	ProductName string     `json:"product_name"`
	Name        string     `json:"name"`
	TokenPrefix string     `json:"token_prefix"`
	Scopes      []string   `json:"scopes"`
	Revoked     bool       `json:"revoked"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	LastUsedIP  *string    `json:"last_used_ip"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// AdminProductTokenDTO is one row of the admin product-credential inventory: the
// per-user row plus the owner attribution (user id and email). Revoked rows and
// tokens of soft-deleted products are included; they are the incident trail.
type AdminProductTokenDTO struct {
	ProductTokenDTO
	UserID     string `json:"user_id"`
	OwnerEmail string `json:"owner_email"`
}

// MintProductTokenResponse is the mint response: the plaintext token, shown exactly
// once (only its sha256 is stored), plus the new row's metadata. It mirrors the CLI
// token mint's {token, cli_token} shape.
type MintProductTokenResponse struct {
	Token        string          `json:"token"`
	ProductToken ProductTokenDTO `json:"product_token"`
}

// V1WhoamiDTO is the GET /api/v1/whoami response (PRD #1907 D13): the caller's
// user, the product when the caller is a uzp_ product token (null for a uzc_ CLI
// caller acting directly), and the scopes the request holds. It is part of the
// stable /api/v1 contract (D12): changes are additive only.
type V1WhoamiDTO struct {
	User    V1WhoamiUserDTO     `json:"user"`
	Product *V1WhoamiProductDTO `json:"product"`
	Scopes  []string            `json:"scopes"`
}

// V1WhoamiUserDTO is the caller's own user, reduced to what an external product
// needs to label it. DisplayName is "" when the user has none set. No email, no
// admin flag: /api/v1 never runs with admin authority (D3), and the email is not
// needed to identify the account to the product.
type V1WhoamiUserDTO struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// V1WhoamiProductDTO is the product a uzp_ token is bound to.
type V1WhoamiProductDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
