package oauthsrv

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// SecretPrefix is the OAuth client-secret class prefix (PRD #1910 D2). It is part of the secret
// bytes and covered by the hash. The secret scrubbers key on it (secretscrub, issuedraft,
// workersvc's CI-fix snapshot), bound by secretscrub's minted-prefix test, which ranges over it.
const SecretPrefix = "uzs_"

// RefreshPrefix is the OAuth refresh-token class prefix (PRD #1910 D5). Like SecretPrefix it is
// part of the token bytes and covered by the hash, and the secret scrubbers key on it (the same
// three copies and the same minted-prefix tests as SecretPrefix). A refresh token is valid only
// at the token endpoint with the issuing client's credentials; no Bearer path knows the class.
const RefreshPrefix = "uzr_"

// secretBytes is the random payload length in bytes (256 bits), as producttoken.
const secretBytes = 32

// secretDisplayBodyChars is how many body characters the stored display prefix keeps:
// "uzs_a1b2" names the secret in the admin UI without meaningfully reducing it.
const secretDisplayBodyChars = 4

// GenerateSecret returns a new plaintext client secret (shown once), its sha256 (to store in
// products.client_secret_hash) and its display prefix (SecretPrefix plus the first body
// characters). Like producttoken, a plain unsalted sha256 is safe because the body is 256 bits
// of randomness: there is no low-entropy keyspace to precompute against.
func GenerateSecret() (secret string, hash []byte, prefix string, err error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("oauthsrv: read random: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	secret = SecretPrefix + body
	return secret, HashSecret(secret), SecretPrefix + body[:secretDisplayBodyChars], nil
}

// HashSecret returns sha256(secret).
func HashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// GenerateRefreshToken returns a new plaintext refresh token (shown once), its sha256 (to store in
// oauth_grants.refresh_token_hash) and its display prefix (RefreshPrefix plus the first body
// characters). 256 bits of randomness, so a plain unsalted sha256 is safe, as for GenerateSecret.
func GenerateRefreshToken() (token string, hash []byte, prefix string, err error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, "", fmt.Errorf("oauthsrv: read random: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(buf)
	token = RefreshPrefix + body
	return token, HashSecret(token), RefreshPrefix + body[:secretDisplayBodyChars], nil
}
