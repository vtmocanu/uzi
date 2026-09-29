// Package producttoken issues and verifies product tokens (PRD #1907): the uzp_
// Bearer credential a user mints for a registered external product, which acts as
// that user on /api/v1 only. Like clitoken and jointoken, the body is a uniformly
// random 256 bits shown once at mint, of which the server stores only the sha256
// (product_tokens.token_hash) and re-derives that hash from the Bearer value on every
// request. The plaintext is never persisted; losing it means minting a new token. A
// plain unsalted sha256 is safe for the same reason as clitoken and jointoken: there
// is no low-entropy keyspace to precompute against.
//
// One difference from clitoken's verification: the auth lookup
// (GetProductTokenForAuth) never projects token_hash (a rule for every query in
// queries/product_tokens.sql), so middleware.RequireV1Caller has no stored hash to
// compare, and this package has no constant-time Equal. The row is found by an indexed
// equality on the sha256 of a 256-bit random token, the same property RequireUser
// relies on; clitoken.Equal on the uzc_ path is belt-and-suspenders, not a separate
// control.
//
// What keeps product tokens out of every internal route is the separate
// product_tokens TABLE, not this prefix: middleware.RequireUser resolves Bearer values
// against cli_tokens only and never reads product_tokens, so a uzp_ token is unknown
// there and fails closed like any unknown Bearer (TestV1IsolationLiveDB measures this
// on both production routers). The prefix is the dispatch label: RequireV1Caller
// keys on it to pick the product_tokens table. It is also what the secret scrubbers
// key on (secretscrub, issuedraft, workersvc's CI-fix snapshot), bound by
// secretscrub's minted-prefix test, which ranges over Prefix.
package producttoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
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

// HasPrefix reports whether a bearer credential is of the product-token class. It
// is a dispatch label only, never authority: authority comes from the
// product_tokens row the hash resolves to.
func HasPrefix(token string) bool {
	return strings.HasPrefix(token, Prefix)
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
