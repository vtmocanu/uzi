// Package secretbox provides authenticated symmetric encryption for secrets
// stored at rest — bot PATs today, per-user Anthropic OAuth tokens in the next
// PRD — so no secret column ever appears in plaintext in a DB dump.
//
// It is deliberately generic (byte-slice in, byte-slice out): nothing here
// knows what a PAT is. Callers Seal on the way into the DB and Open on the way
// out.
//
// Construction: AES-256-GCM with a per-message 12-byte random nonce prepended
// to the ciphertext. GCM provides both confidentiality and integrity, so a
// tampered row decrypts to an error instead of silently garbled plaintext.
// (The package name echoes NaCl's secretbox, but the construction is AES-GCM,
// matching the reference implementation this was adapted from.)
//
// Key: 32 bytes, loaded from an env var as base64 (LoadKey). Rotating the key
// invalidates every stored ciphertext — there is no re-encrypt path in this
// iteration; every ciphertext is keyed by the one current master key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

// KeySize is the required master-key length in bytes (AES-256).
const KeySize = 32

// ErrInvalidKey is returned by New when the key length is not KeySize.
var ErrInvalidKey = errors.New("secretbox: key must be 32 bytes")

// ErrCiphertextTooShort is returned when the input to Open is smaller than the
// nonce + GCM tag overhead.
var ErrCiphertextTooShort = errors.New("secretbox: ciphertext too short")

// ErrWeakKey is returned by LoadKey when the decoded key has no entropy (every
// byte identical — e.g. the all-zero key, or a lazy "AAAA…"/"0000…" base64
// placeholder). A real random 32-byte key is astronomically unlikely to be
// all-identical, so this only ever fires on an obvious placeholder.
var ErrWeakKey = errors.New("secretbox: key is a low-entropy placeholder (all bytes identical); generate a random one with: openssl rand -base64 32")

// Box encrypts and decrypts byte slices using a fixed master key. Box instances
// are safe for concurrent use after construction — cipher.AEAD itself is
// goroutine-safe.
type Box struct {
	aead cipher.AEAD
}

// New constructs a Box bound to the given 32-byte master key. Callers should
// hold the returned *Box for the process lifetime; constructing it per request
// needlessly re-derives the AES round keys.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: cipher.NewGCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns nonce || ciphertext || tag. The nonce is
// randomly generated per call; callers must NOT cache or reuse the output as if
// it were deterministic (two Seal calls on the same plaintext produce different
// bytes, which is what stops content-fingerprinting stored secrets).
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	return b.SealWithAAD(plaintext, nil)
}

// Open reverses Seal. Returns ErrCiphertextTooShort or an authentication error
// (from GCM) if the input is malformed or tampered.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	return b.OpenWithAAD(sealed, nil)
}

// SealWithAAD is Seal with additional authenticated data: aad is bound into the
// GCM tag (not encrypted, not stored) so Open only succeeds when supplied the
// identical aad. Callers pass a domain-separating context — e.g. user_id||kind —
// so a DB-write operator cannot swap a ciphertext onto a different owner/kind and
// have it still authenticate. A nil aad is equivalent to plain Seal, keeping the
// existing at-rest ciphertexts (sealed with no aad) openable.
//
// The binding is only as fine-grained as the context the caller supplies: it pins
// a ciphertext to a ROW only where that context identifies one. It stopped doing
// so for user_secrets in PRD #104, which lets a user hold several secrets sharing
// one (user_id, kind) — see vault.secretAAD for the residual risk that leaves and
// why it is accepted. Choose an aad that identifies what you actually mean to pin.
func (b *Box) SealWithAAD(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: read nonce: %w", err)
	}
	// aead.Seal appends ciphertext+tag to its first argument; passing `nonce`
	// yields a single contiguous nonce||ciphertext||tag slice that Open splits.
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// OpenWithAAD reverses SealWithAAD. The aad must byte-match the one passed to
// SealWithAAD or GCM authentication fails (indistinguishable from a wrong key or
// a tampered ciphertext — all surface as a decrypt error carrying no plaintext).
func (b *Box) OpenWithAAD(sealed, aad []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < ns+b.aead.Overhead() {
		return nil, ErrCiphertextTooShort
	}
	nonce, ciphertext := sealed[:ns], sealed[ns:]
	return b.aead.Open(nil, nonce, ciphertext, aad)
}

// LoadKey reads a base64-encoded 32-byte key from the given env var. Returns a
// clear error (not a zero key) when the var is unset, not base64, or the wrong
// length, so a misconfigured deployment fails loud at boot rather than running
// with a guessable or empty key.
func LoadKey(envVar string) ([]byte, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return nil, fmt.Errorf("secretbox: %s is not set", envVar)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %s is not valid base64: %w", envVar, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: %s decodes to %d bytes, expected %d", envVar, len(key), KeySize)
	}
	if isAllIdentical(key) {
		return nil, ErrWeakKey
	}
	return key, nil
}

// isAllIdentical reports whether every byte of b equals the first. This catches
// the all-zero key and single-repeated-byte placeholder keys.
func isAllIdentical(b []byte) bool {
	for _, x := range b {
		if x != b[0] {
			return false
		}
	}
	return true
}
