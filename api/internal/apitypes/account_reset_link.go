package apitypes

import "time"

// Account reset link wire types (PRD #3989). The plaintext token appears exactly
// once, in AccountResetLinkDTO, at issue time; only its sha256 is stored.

// AccountResetLinkDTO is the response of POST /api/admin/users/{id}/reset-link.
// Kind is "password" or "vault_only"; TargetIsAdmin is a snapshot taken at issue.
type AccountResetLinkDTO struct {
	Token         string    `json:"token"`
	ExpiresAt     time.Time `json:"expires_at"`
	Kind          string    `json:"kind"`
	TargetIsAdmin bool      `json:"target_is_admin"`
}

// ResetLinkPreviewRequest is the body of POST /api/auth/reset/preview.
type ResetLinkPreviewRequest struct {
	Token string `json:"token"`
}

// ResetLinkPreviewDTO is the response of POST /api/auth/reset/preview: the kind
// derived from the current users row and the link expiry, nothing else.
type ResetLinkPreviewDTO struct {
	Kind      string    `json:"kind"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ResetLinkRedeemRequest is the body of POST /api/auth/reset/redeem. Password is
// required for kind=password and must be absent for kind=vault_only.
type ResetLinkRedeemRequest struct {
	Token    string `json:"token"`
	Password string `json:"password,omitempty"`
}
