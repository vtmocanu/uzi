package apitypes

import "time"

// The consent half of uzi's OAuth authorization server (PRD #1910 M2). These DTOs are the wire
// seam of GET /api/oauth/requests/{id} and the approve / deny POSTs under it, which only the
// SPA's /connect page calls. Neither carries a code, a challenge, a binding hash or the
// product's redirect URI list: the page learns the redirect HOST for display, and the only URL
// it ever navigates to is the server-built OAuthRedirectResponse.RedirectURL.

// OAuthAuthorizeRequestDTO is the consent-screen metadata of one pending authorize request.
// ProductName and ProductDescription are admin-written, termsafe-validated plain strings the
// page renders as text, never Markdown or HTML. RedirectHost is the host (and port) the browser
// is sent to after the user decides. Scopes are the scopes this request asks for. Status is the
// request's state (pending, approved, redeemed, denied or superseded); only a pending request
// can still be decided.
type OAuthAuthorizeRequestDTO struct {
	ProductName        string    `json:"product_name"`
	ProductDescription string    `json:"product_description"`
	RedirectHost       string    `json:"redirect_host"`
	Scopes             []string  `json:"scopes"`
	Status             string    `json:"status"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// OAuthRedirectResponse is the approve / deny response: the URL the SPA navigates to. It is the
// product's registered redirect URI plus code (approve) or error=access_denied (deny), the
// product's unchanged state and iss, built server-side.
type OAuthRedirectResponse struct {
	RedirectURL string `json:"redirect_url"`
}

// OAuthTokenResponse is the 200 body of POST /api/oauth/token (PRD #1910 D5, RFC 6749 section
// 5.1): the product's access token (a uzp_ product token), its type and lifetime, the grant's
// uzr_ refresh token and the granted scopes, space-joined. It is part of the external contract
// (ADR-1907's compatibility promise extends to it) and the SPA never reads it, so it has no
// TypeScript twin. The response also carries Cache-Control: no-store.
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// OAuthRefreshResponse is the 200 body of POST /api/oauth/token for grant_type=refresh_token (PRD
// #1910 M4, RFC 6749 section 6): a new access token with its type, lifetime and scope (the grant's
// scopes, or the narrower subset the request named). It deliberately carries NO refresh_token: the
// refresh token is not rotated (D5), so the product keeps the one it holds, and RFC 6749 section
// 6 lets the server omit it. Part of the external contract; no TypeScript twin.
type OAuthRefreshResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

// OAuthErrorResponse is the error body of the OAuth token endpoint (RFC 6749 section 5.2):
// an error code and, for some codes, a fixed human-readable description. Descriptions are fixed
// strings, never an echo of a request value.
type OAuthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// OAuthConnectionDTO is one live OAuth connection (grant) of the caller, from GET
// /api/me/oauth-connections (PRD #1910 M3): the product, the approved scopes and the connection's
// timestamps. It never carries a token, a hash or a refresh-token prefix. ConnectedAt is the
// latest consent (the 90-day refresh expiry counts from it), CreatedAt the first consent.
// LastUsedAt is the later of the refresh token's last use and the latest use of any of the
// grant's access tokens, null when none was ever used; RefreshIssuedAt is null until the
// authorization code is exchanged. A connection is listed while it is live, whatever the state of
// its access tokens. Scopes is never null on the wire.
type OAuthConnectionDTO struct {
	ID              string     `json:"id"`
	ProductID       string     `json:"product_id"`
	ProductName     string     `json:"product_name"`
	Scopes          []string   `json:"scopes"`
	ConnectedAt     time.Time  `json:"connected_at"`
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	RefreshIssuedAt *time.Time `json:"refresh_issued_at"`
}

// AdminOAuthConnectionDTO is one live OAuth connection of a product, from GET
// /api/admin/products/{id}/connections (PRD #1910 M5): the connecting user (id and email, as the
// admin product-token inventory shows an owner), the approved scopes and the timestamps. It never
// carries a token, a hash or a refresh-token prefix. ConnectedAt is the latest consent, CreatedAt
// the first one; LastUsedAt is as in OAuthConnectionDTO. Scopes is never null on the wire.
type AdminOAuthConnectionDTO struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	OwnerEmail  string     `json:"owner_email"`
	Scopes      []string   `json:"scopes"`
	ConnectedAt time.Time  `json:"connected_at"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
}
