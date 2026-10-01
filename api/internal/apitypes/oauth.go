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
