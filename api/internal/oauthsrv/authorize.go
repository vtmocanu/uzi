package oauthsrv

import (
	"errors"
	"net/url"
	"strings"
)

const (
	// MaxStateBytes caps the client's opaque state (PRD #1910 D3). The cap is checked before
	// anything is stored, so the unauthenticated authorize endpoint cannot be made to store a blob.
	MaxStateBytes = 512
	// MaxScopeBytes caps the scope parameter (D3); two known scopes need 20.
	MaxScopeBytes = 64
	// CodeChallengeLen is the exact length of a PKCE S256 challenge: base64url(sha256), unpadded
	// (RFC 7636 section 4.2).
	CodeChallengeLen = 43
)

// Authorization-endpoint error codes (RFC 6749 section 4.1.2.1) the validator produces.
const (
	ErrInvalidRequest          = "invalid_request"
	ErrUnsupportedResponseType = "unsupported_response_type"
	ErrInvalidScope            = "invalid_scope"
	ErrAccessDenied            = "access_denied"
	ErrTemporarilyUnavailable  = "temporarily_unavailable"
)

// ErrClientRejected means the client_id / redirect_uri pair is unusable. Per RFC 6749
// section 4.1.2.1 the server MUST NOT redirect to an unverified URI and must not echo any
// parameter: the handler shows a fixed static page. It deliberately carries no detail.
var ErrClientRejected = errors.New("oauthsrv: client_id or redirect_uri rejected; do not redirect")

// AuthorizeQuery is the parsed authorize query string. Values are the single supplied value,
// or "" when absent; Repeated reports a parameter that was sent more than once, which this
// server treats as invalid_request (and as a static error for client_id and redirect_uri).
type AuthorizeQuery struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	Scope               string
	// HasScope separates a missing scope (all of the client's allowed scopes) from an empty one.
	HasScope bool

	repeated map[string]bool
}

// ParseAuthorizeQuery reads the authorize parameters out of a parsed query. A repeated
// parameter keeps its first value and is recorded in Repeated.
func ParseAuthorizeQuery(v url.Values) AuthorizeQuery {
	q := AuthorizeQuery{repeated: map[string]bool{}}
	for k, vals := range v {
		if len(vals) > 1 {
			q.repeated[k] = true
		}
	}
	get := func(name string) string {
		if vals := v[name]; len(vals) > 0 {
			return vals[0]
		}
		return ""
	}
	q.ClientID = get("client_id")
	q.RedirectURI = get("redirect_uri")
	q.ResponseType = get("response_type")
	q.CodeChallenge = get("code_challenge")
	q.CodeChallengeMethod = get("code_challenge_method")
	q.State = get("state")
	q.Scope = get("scope")
	_, q.HasScope = v["scope"]
	return q
}

// Repeated reports whether the named parameter was supplied more than once.
func (q AuthorizeQuery) Repeated(name string) bool { return q.repeated[name] }

// AnyRepeated reports whether any parameter at all was supplied more than once.
func (q AuthorizeQuery) AnyRepeated() bool { return len(q.repeated) > 0 }

// Client is a product as the authorize validator sees it, with no store types.
type Client struct {
	// Active is true for an enabled, not soft-deleted product.
	Active       bool
	RedirectURIs []string
	Scopes       []string
	HasSecret    bool
}

// IsClient reports whether the product can run the consent flow (PRD #1910 D2): active, with at
// least one redirect URI, a non-empty allowed scope list and a client secret.
func (c *Client) IsClient() bool {
	return c != nil && c.Active && len(c.RedirectURIs) > 0 && len(c.Scopes) > 0 && c.HasSecret
}

// CheckClient is the first stage of authorize validation (PRD #1910 D3). It returns
// ErrClientRejected for an unknown (nil) or inactive product, a product that is not a client, a
// repeated client_id or redirect_uri, or a redirect_uri that is not an exact string match for a
// registered one. No parameter is trusted until it returns a nil error. On success it returns the
// matching entry of c.RedirectURIs: that registered value, never the request's own string, is
// the one every later redirect goes to.
func CheckClient(c *Client, q AuthorizeQuery) (string, error) {
	if q.Repeated("client_id") || q.Repeated("redirect_uri") {
		return "", ErrClientRejected
	}
	if !c.IsClient() || q.RedirectURI == "" {
		return "", ErrClientRejected
	}
	for _, registered := range c.RedirectURIs {
		if q.RedirectURI == registered {
			return registered, nil
		}
	}
	return "", ErrClientRejected
}

// Request is a fully validated authorize request, safe to store.
type Request struct {
	RedirectURI   string
	Scopes        []string
	State         string
	CodeChallenge string
}

// Error is an authorize failure to report by redirecting to the (already verified) registered
// redirect URI. State is the client's state when it was valid, so it can be returned unchanged;
// it is "" when the client sent none or an unusable one.
type Error struct {
	Code        string
	Description string
	State       string
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

// ValidateAuthorize is the second stage (PRD #1910 D3); call it only after CheckClient returned
// a nil error, passing the registered redirectURI it returned (the Request carries that value).
// It checks, in order: no repeated parameter, response_type=code, PKCE (S256 only, a
// 43-character base64url challenge), state (1..512 printable ASCII bytes), and scope (at most 64
// bytes, a non-empty subset of the client's allowed scopes; a missing scope means all of them).
// The descriptions are fixed strings: no parameter value is echoed.
func ValidateAuthorize(c *Client, q AuthorizeQuery, redirectURI string) (*Request, *Error) {
	state := ""
	if !q.Repeated("state") && validState(q.State) {
		state = q.State
	}
	fail := func(code, desc string) (*Request, *Error) {
		return nil, &Error{Code: code, Description: desc, State: state}
	}
	if q.AnyRepeated() {
		return fail(ErrInvalidRequest, "a parameter was supplied more than once")
	}
	if q.ResponseType == "" {
		return fail(ErrInvalidRequest, "response_type is required")
	}
	if q.ResponseType != "code" {
		return fail(ErrUnsupportedResponseType, "only response_type=code is supported")
	}
	if q.CodeChallengeMethod != "S256" {
		return fail(ErrInvalidRequest, "PKCE with code_challenge_method=S256 is required")
	}
	if !validCodeChallenge(q.CodeChallenge) {
		return fail(ErrInvalidRequest, "code_challenge must be a 43-character base64url S256 challenge")
	}
	if state == "" {
		return fail(ErrInvalidRequest, "state is required (1 to 512 printable ASCII characters)")
	}
	scopes := append([]string(nil), c.Scopes...)
	if q.HasScope {
		if len(q.Scope) > MaxScopeBytes {
			return fail(ErrInvalidScope, "scope is too long")
		}
		parsed, err := ParseScopes(q.Scope)
		if err != nil {
			return fail(ErrInvalidScope, "scope must be a space-separated list of known scopes")
		}
		for _, sc := range parsed {
			if !contains(c.Scopes, sc) {
				return fail(ErrInvalidScope, "scope is not allowed for this client")
			}
		}
		scopes = parsed
	}
	return &Request{
		RedirectURI:   redirectURI,
		Scopes:        scopes,
		State:         state,
		CodeChallenge: q.CodeChallenge,
	}, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validState reports whether s is a usable state: 1..MaxStateBytes bytes, each in the RFC 6749
// VSCHAR range 0x20-0x7E. The range also keeps NUL and invalid UTF-8 out of the database.
func validState(s string) bool {
	if s == "" || len(s) > MaxStateBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validCodeChallenge reports whether s is exactly CodeChallengeLen base64url (unpadded) characters.
func validCodeChallenge(s string) bool {
	if len(s) != CodeChallengeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ErrorRedirectURL builds the redirect for an authorize failure (RFC 6749 section 4.1.2.1 and
// RFC 9207): the registered redirect URI with error, error_description, state (when present) and
// iss added to the query. The registered URI's own query is preserved. There is never a fragment.
func ErrorRedirectURL(redirectURI string, e *Error, iss string) string {
	v := url.Values{}
	v.Set("error", e.Code)
	if e.Description != "" {
		v.Set("error_description", e.Description)
	}
	if e.State != "" {
		v.Set("state", e.State)
	}
	v.Set("iss", iss)
	return appendQuery(redirectURI, v)
}

// DeniedRedirectURL is ErrorRedirectURL for the user pressing Deny (access_denied).
func DeniedRedirectURL(redirectURI, state, iss string) string {
	return ErrorRedirectURL(redirectURI, &Error{Code: ErrAccessDenied, Description: "the user denied the request", State: state}, iss)
}

// CodeRedirectURL builds the success redirect: the registered redirect URI with code, state and
// iss added to the query.
func CodeRedirectURL(redirectURI, code, state, iss string) string {
	v := url.Values{}
	v.Set("code", code)
	v.Set("state", state)
	v.Set("iss", iss)
	return appendQuery(redirectURI, v)
}

// appendQuery adds v to base's existing query, keeping what is there except any pair whose key
// the server itself sets (a registered URI is refused such keys, so this only guards a URI stored
// before that rule). base is a registered redirect URI, which ValidateRedirectURI guarantees
// carries no fragment.
func appendQuery(base string, v url.Values) string {
	path, rawQuery, _ := strings.Cut(base, "?")
	kept := withoutReservedQueryKeys(rawQuery)
	if kept == "" {
		return path + "?" + v.Encode()
	}
	return path + "?" + kept + "&" + v.Encode()
}
