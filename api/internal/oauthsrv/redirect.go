package oauthsrv

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	// MaxRedirectURIs is the per-client cap on registered redirect URIs (PRD #1910 D2). The
	// products_redirect_uris_check CHECK carries the same number.
	MaxRedirectURIs = 5
	// MaxRedirectURIBytes is the cap on one registered redirect URI.
	MaxRedirectURIBytes = 2048
)

// ValidateRedirectURI reports whether s is an acceptable registered redirect URI (PRD #1910
// D2). The rules, each an RFC 6749 section 3.1.2 or RFC 9700 requirement:
//
//   - at most MaxRedirectURIBytes bytes, printable ASCII only (no whitespace, control or
//     non-ASCII byte), so the exact-match comparison at authorize time has one spelling;
//   - an absolute URI with a lowercase https scheme, or http only for the loopback IP literals
//     127.0.0.1 and [::1] WITH an explicit port (native-app loopback redirect, RFC 8252
//     section 7.3). "localhost" is never accepted: it is a name resolved by the client's
//     resolver, not a literal;
//   - a host, and no userinfo;
//   - no fragment (RFC 6749 section 3.1.2), including an empty one ("https://a/b#");
//   - no query parameter the server itself appends to the redirect (reservedRedirectQueryKeys),
//     so a registered URI cannot pre-seed a code, state, iss or error that a client might read
//     instead of the server's.
func ValidateRedirectURI(s string) error {
	if s == "" {
		return errors.New("redirect URI must not be empty")
	}
	if len(s) > MaxRedirectURIBytes {
		return fmt.Errorf("redirect URI must be at most %d bytes", MaxRedirectURIBytes)
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return errors.New("redirect URI must contain only printable ASCII characters")
		}
	}
	if strings.Contains(s, "#") {
		return errors.New("redirect URI must not contain a fragment")
	}
	var https bool
	switch {
	case strings.HasPrefix(s, "https://"):
		https = true
	case strings.HasPrefix(s, "http://"):
	default:
		return errors.New("redirect URI must be an https URL (http is allowed only for 127.0.0.1 and [::1] with a port)")
	}
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("redirect URI is not a valid URL")
	}
	if hasReservedQueryKey(u.RawQuery) {
		return errors.New("redirect URI query must not contain code, state, iss, error, error_description or error_uri")
	}
	if u.User != nil {
		return errors.New("redirect URI must not contain userinfo")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("redirect URI must have a host")
	}
	if https {
		return nil
	}
	// http: loopback IP literal only, with a port.
	if host != "127.0.0.1" && host != "::1" {
		return errors.New("http redirect URIs are allowed only for the loopback IP literals 127.0.0.1 and [::1], never localhost")
	}
	port := u.Port()
	if port == "" {
		return errors.New("a loopback http redirect URI must include a port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("redirect URI port must be between 1 and 65535")
	}
	return nil
}

// ValidateRedirectURIs validates a client's whole redirect-URI list: at most MaxRedirectURIs
// entries, each passing ValidateRedirectURI, no exact duplicates. The empty list is valid (the
// product is not a client). The error names the offending entry's position, never echoing it.
func ValidateRedirectURIs(uris []string) error {
	if len(uris) > MaxRedirectURIs {
		return fmt.Errorf("at most %d redirect URIs are allowed", MaxRedirectURIs)
	}
	seen := make(map[string]bool, len(uris))
	for i, u := range uris {
		if err := ValidateRedirectURI(u); err != nil {
			return fmt.Errorf("redirect_uris[%d]: %w", i, err)
		}
		if seen[u] {
			return fmt.Errorf("redirect_uris[%d]: duplicate redirect URI", i)
		}
		seen[u] = true
	}
	return nil
}

// reservedRedirectQueryKeys are the query parameters the server adds to a redirect (RFC 6749
// sections 4.1.2 and 4.1.2.1, RFC 9207). Matching is case-sensitive on the decoded key.
var reservedRedirectQueryKeys = map[string]bool{
	"code": true, "state": true, "iss": true,
	"error": true, "error_description": true, "error_uri": true,
}

// hasReservedQueryKey reports whether a raw query string carries a reserved key. It splits on "&"
// itself (not url.ParseQuery) so a malformed pair is still examined; a key that does not decode is
// treated as reserved, since it cannot be shown to be harmless.
func hasReservedQueryKey(rawQuery string) bool {
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		k, _, _ := strings.Cut(pair, "=")
		dk, err := url.QueryUnescape(k)
		if err != nil || reservedRedirectQueryKeys[dk] {
			return true
		}
	}
	return false
}

// withoutReservedQueryKeys returns rawQuery minus every reserved pair, in order. The redirect
// builders use it so a stored URI that predates the registration rule still cannot shadow the
// server's own parameters.
func withoutReservedQueryKeys(rawQuery string) string {
	var kept []string
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		k, _, _ := strings.Cut(pair, "=")
		if dk, err := url.QueryUnescape(k); err != nil || reservedRedirectQueryKeys[dk] {
			continue
		}
		kept = append(kept, pair)
	}
	return strings.Join(kept, "&")
}
