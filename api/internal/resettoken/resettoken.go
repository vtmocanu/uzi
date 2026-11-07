// Package resettoken issues and parses the one-time account reset link token
// (PRD #3989). It follows the worker join-token and clitoken posture: 256 random
// bits shown once, of which only the sha256 is stored
// (account_reset_links.token_hash, CHECK octet_length = 32). A plain unsalted
// sha256 is safe because the token is uniformly random data.
package resettoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Prefix marks the token class for humans and secret scanners (uzw_, uzc_, uza_).
const Prefix = "uzr_"

// randomBytes is the random payload length (256 bits).
const randomBytes = 32

// bodyLen is the base64url (no padding) length of randomBytes.
const bodyLen = (randomBytes*8 + 5) / 6

// Length is the exact length of every token Generate returns.
const Length = len(Prefix) + bodyLen

// Generate returns a new plaintext token (shown once) and its sha256 for storage.
func Generate() (token string, hash []byte, err error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("resettoken: read random: %w", err)
	}
	token = Prefix + base64.RawURLEncoding.EncodeToString(buf)
	return token, hashOf(token), nil
}

// Hash returns sha256(token) for a presented token. ok is false, without hashing,
// when the token is not exactly the shape Generate produces in length and prefix,
// so untrusted input of any other size never reaches the hash or the database.
func Hash(token string) (hash []byte, ok bool) {
	if len(token) != Length || token[:len(Prefix)] != Prefix {
		return nil, false
	}
	return hashOf(token), true
}

func hashOf(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
