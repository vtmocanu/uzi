// Package clitoken issues and verifies CLI tokens: the Bearer credential the
// `uzi` CLI presents so humans and agents can drive the factory headlessly
// (PRD #64). It mirrors the jointoken posture that already works for workers — a
// high-entropy random string shown once at mint, of which the server stores only
// the sha256 (cli_tokens.token_hash) and re-derives that hash from the Bearer
// token on every request. The plaintext is never persisted; losing it means
// minting a new token, exactly like an API key.
//
// Two class prefixes mark a token's kind for humans and secret scanners (the same
// convention as the worker's uzw_):
//
//   - uzc_ — a user token, whose reach is CAPPED to its owner's own authority.
//   - uza_ — an admin_ro token, read-only across the whole factory.
//
// The prefix is a LABEL, not the enforcement: the reach ceiling is enforced live
// in middleware.RequireUser (a non-admin_ro token is handed a copy of the user row
// with is_admin cleared), never from the credential. As with jointoken, a plain
// unsalted sha256 is safe precisely because the token is uniformly random 256-bit
// data: there is no low-entropy keyspace to precompute against.
package clitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

// Scope values, mirrored from the cli_tokens.scope CHECK. Kept here so the token
// vocabulary (class prefix ⇄ scope) lives in one place.
const (
	ScopeUser    = "user"
	ScopeAdminRO = "admin_ro"
)

// Class prefixes. PrefixUser marks a user (uzc_) token, PrefixAdmin an admin_ro
// (uza_) one. Both are part of the token bytes and covered by the hash.
const (
	PrefixUser  = "uzc_"
	PrefixAdmin = "uza_"
)

// tokenBytes is the random payload length in bytes (256 bits of entropy), the
// same as jointoken.
const tokenBytes = 32

// displayBodyChars is how many characters of the encoded body are kept in
// token_prefix for the list display (4 chars ≈ 24 bits under RawURLEncoding,
// leaving 2^232 of the token unrevealed — enough to identify a row, no meaningful
// reduction of the secret). Total stored prefix is the 4-char class prefix plus
// these, e.g. "uzc_a1b2".
const displayBodyChars = 4

// classPrefix maps a scope to its class prefix. Any non-admin_ro scope (including
// the empty default) is a user token.
func classPrefix(scope string) string {
	if scope == ScopeAdminRO {
		return PrefixAdmin
	}
	return PrefixUser
}

// Generate returns a new plaintext CLI token for the given scope (to be shown
// once), its sha256 hash (to be stored in cli_tokens.token_hash), and the display
// prefix (class prefix + the first displayBodyChars body characters) for the token
// list.
func Generate(scope string) (token string, hash []byte, prefix string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("clitoken: read random: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	token = classPrefix(scope) + body
	return token, Hash(token), classPrefix(scope) + body[:displayBodyChars], nil
}

// Hash returns sha256(token). The lookup that uses it (GetCLITokenByHash) compares
// the full 32-byte digest, so an attacker cannot narrow the token from a partial
// match.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal compares two hashes in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// FromAuthorizationHeader extracts the bearer credential from an Authorization
// header value. ok is false when the header is empty, not the Bearer scheme, or
// carries an empty credential — i.e. it returns ok exactly for `Bearer
// <non-empty>`. RequireUser dispatches on this: ok ⇒ the CLI-token path (no CSRF,
// cookie ignored); !ok ⇒ the cookie path (still CSRF-enforced), so an
// `Authorization: Basic …` header falls to the safe branch. This is
// dispatch-on-parse, never a try-cookie-then-bearer fallback (the CSRF-bypass
// shape). Mirrors jointoken.FromAuthorizationHeader; it deliberately does NOT
// check the uzc_/uza_ prefix, exactly as the worker path does not check uzw_ — a
// non-cli Bearer simply fails the hash lookup, closed.
func FromAuthorizationHeader(h string) (token string, ok bool) {
	const scheme = "Bearer "
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	token = strings.TrimSpace(h[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
