package oauthsrv

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/url"
)

// Token-endpoint error codes (RFC 6749 section 5.2) plus the temporarily_unavailable extension
// (PRD #1910 D7), which is an authorization-endpoint code in the RFC that this server also uses
// on the token endpoint for the ten-live-token bound and for storage errors.
const (
	ErrInvalidClient        = "invalid_client"
	ErrInvalidGrant         = "invalid_grant"
	ErrUnsupportedGrantType = "unsupported_grant_type"
)

// Verifier length bounds (RFC 7636 section 4.1).
const (
	MinVerifierLen = 43
	MaxVerifierLen = 128
)

// ValidVerifier reports whether v is a well-formed PKCE code_verifier: 43 to 128 characters of
// the RFC 7636 unreserved set (ALPHA, DIGIT, "-", ".", "_", "~").
func ValidVerifier(v string) bool {
	if len(v) < MinVerifierLen || len(v) > MaxVerifierLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// VerifierMatchesS256 reports whether verifier is well formed and base64url(sha256(verifier))
// equals the stored challenge, compared in constant time (RFC 7636 section 4.6).
func VerifierMatchesS256(verifier, challenge string) bool {
	if !ValidVerifier(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// TokenForm is a parsed token-endpoint form body. Values are the single supplied value, or ""
// when absent. Repeated reports that ANY parameter, known or not, was sent more than once, which is
// invalid_request (RFC 6749 section 3.2).
type TokenForm struct {
	GrantType    string
	Code         string
	RedirectURI  string
	CodeVerifier string
	ClientID     string
	ClientSecret string
	// RefreshToken is the refresh_token grant's credential (PRD #1910 M4).
	RefreshToken string
	// Scope is the refresh grant's optional narrowing scope; HasScope separates an absent scope
	// from an empty one, which is malformed rather than "no narrowing".
	Scope    string
	HasScope bool
	// Token and TokenTypeHint are the RFC 7009 revoke request's parameters (PRD #1910 M4).
	Token         string
	TokenTypeHint string
	// HasClientSecret separates an absent client_secret from an empty one: any client_secret in
	// the body is a second authentication method next to Basic.
	HasClientSecret bool
	// Repeated is true when some parameter had more than one value.
	Repeated bool
}

// ParseTokenForm reads the token-endpoint parameters out of a parsed form body.
func ParseTokenForm(v url.Values) TokenForm {
	var f TokenForm
	for _, vals := range v {
		if len(vals) > 1 {
			f.Repeated = true
		}
	}
	one := func(name string) string {
		if vals := v[name]; len(vals) > 0 {
			return vals[0]
		}
		return ""
	}
	f.GrantType = one("grant_type")
	f.Code = one("code")
	f.RedirectURI = one("redirect_uri")
	f.CodeVerifier = one("code_verifier")
	f.ClientID = one("client_id")
	f.ClientSecret = one("client_secret")
	f.RefreshToken = one("refresh_token")
	f.Scope = one("scope")
	_, f.HasScope = v["scope"]
	f.Token = one("token")
	f.TokenTypeHint = one("token_type_hint")
	_, f.HasClientSecret = v["client_secret"]
	return f
}

// SecretMatches reports whether secret hashes to the stored client_secret_hash, in constant time.
func SecretMatches(secret string, storedHash []byte) bool {
	if len(storedHash) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(HashSecret(secret), storedHash) == 1
}
