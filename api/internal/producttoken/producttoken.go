// Package producttoken issues and verifies product tokens (PRD #1907): the uzp_
// Bearer credential a user mints for a registered external product, which acts as
// that user on /api/v1 only. It mirrors clitoken exactly in its crypto posture: a
// uniformly random 256-bit body shown once at mint, of which the server stores only
// the sha256 (product_tokens.token_hash) and re-derives that hash from the Bearer
// value on every request. The plaintext is never persisted; losing it means minting
// a new token. A plain unsalted sha256 is safe for the same reason as clitoken and
// jointoken: there is no low-entropy keyspace to precompute against.
//
// The uzp_ class prefix is what keeps product tokens out of every internal route:
// middleware.RequireUser resolves Bearer values against cli_tokens only, so a uzp_
// token is unknown there and fails closed. Only RequireV1Caller (PRD #1907 M2)
// resolves it, dispatching on this prefix to pick the product_tokens table. The
// prefix is also what the secret scrubbers key on (secretscrub, issuedraft,
// workersvc's CI-fix snapshot), bound by secretscrub's minted-prefix test, which
// ranges over Prefix.
package producttoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// Prefix is the product-token class prefix. It is part of the token bytes and
// covered by the hash.
const Prefix = "uzp_"

// Scope values, mirrored from the product_tokens scopes CHECK (migration 00269,
// PRD #1907 D5). jobs:run starts and cancels jobs; jobs:read reads their status and
// results.
const (
	ScopeJobsRun  = "jobs:run"
	ScopeJobsRead = "jobs:read"
)

// Scopes lists every known scope, in display order. The database CHECK carries the
// same list; ValidScopes is the Go-side mirror so a handler rejects an unknown scope
// with a 400 before the insert would fail the CHECK.
var Scopes = []string{ScopeJobsRun, ScopeJobsRead}

// tokenBytes is the random payload length in bytes (256 bits), as clitoken.
const tokenBytes = 32

// displayBodyChars is how many body characters token_prefix keeps for the list
// display, as clitoken: "uzp_a1b2" names a row without meaningfully reducing the
// secret.
const displayBodyChars = 4

// Generate returns a new plaintext product token (shown once), its sha256 hash (to
// store in product_tokens.token_hash) and its display prefix (Prefix plus the first
// displayBodyChars body characters).
func Generate() (token string, hash []byte, prefix string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("producttoken: read random: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	token = Prefix + body
	return token, Hash(token), Prefix + body[:displayBodyChars], nil
}

// Hash returns sha256(token). The auth lookup (GetProductTokenForAuth) compares the
// full 32-byte digest.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal compares two hashes in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// HasPrefix reports whether a bearer credential is of the product-token class. It
// is a dispatch label only, never authority: authority comes from the
// product_tokens row the hash resolves to.
func HasPrefix(token string) bool {
	return strings.HasPrefix(token, Prefix)
}

// FromAuthorizationHeader extracts a product-token credential from an Authorization
// header value. ok is true exactly for `Bearer <value>` whose value carries Prefix;
// the Bearer parsing itself is clitoken.FromAuthorizationHeader's, so the two token
// classes can never disagree about what a Bearer header is. A non-uzp_ Bearer is
// !ok here, which lets the caller dispatch on class deterministically rather than
// trying one table and falling back to another (PRD #1907 D2).
func FromAuthorizationHeader(h string) (token string, ok bool) {
	token, ok = clitoken.FromAuthorizationHeader(h)
	if !ok || !HasPrefix(token) {
		return "", false
	}
	return token, true
}

// ValidScope reports whether s is a known scope.
func ValidScope(s string) bool {
	for _, known := range Scopes {
		if s == known {
			return true
		}
	}
	return false
}

// ValidScopes reports whether ss is an acceptable scope set for a new token:
// non-empty, every element known, no duplicates. Mirrors (and is stricter than, on
// duplicates) the product_tokens scopes CHECKs.
func ValidScopes(ss []string) bool {
	if len(ss) == 0 {
		return false
	}
	seen := make(map[string]bool, len(ss))
	for _, s := range ss {
		if !ValidScope(s) || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}
