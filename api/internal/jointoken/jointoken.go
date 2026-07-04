// Package jointoken issues and verifies worker join tokens. A join token is a
// high-entropy random string shown to the user exactly once at worker creation;
// the server stores only its sha256 (workers.token_hash) and re-derives that
// hash from the Bearer token on every worker call. The plaintext token is never
// persisted — losing it means issuing a new worker, exactly like an API key.
//
// This is a net-new mechanism (PRD #1 has no reusable token-hash precedent). A
// plain unsalted sha256 is safe here precisely because the token is uniformly
// random 256-bit data: there is no low-entropy keyspace to precompute against,
// so the per-record salt that password hashing needs buys nothing.
package jointoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
)

// Prefix marks a uzi worker join token so humans and secret scanners can
// recognize one on sight. It is part of the token bytes and is covered by the
// hash.
const Prefix = "uzw_"

// tokenBytes is the random payload length in bytes (256 bits of entropy).
const tokenBytes = 32

// Generate returns a new plaintext join token (to be shown once) and its sha256
// hash (to be stored in workers.token_hash).
func Generate() (token string, hash []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("jointoken: read random: %w", err)
	}
	token = Prefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, Hash(token), nil
}

// Hash returns sha256(token). The lookup that uses it (GetWorkerByTokenHash)
// compares the full 32-byte digest, so an attacker cannot narrow the token from
// a partial match.
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
// carries an empty credential.
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
