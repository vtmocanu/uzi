// Package secretbox provides authenticated symmetric encryption for byte-slice
// secrets held at rest. A single fixed 32-byte master key drives an AES-256-GCM
// AEAD, so every sealed value is both confidential and tamper-evident.
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

var (
	// ErrInvalidKey is returned when a master key is not exactly KeySize bytes.
	ErrInvalidKey = errors.New("secretbox: master key must be exactly 32 bytes long")
	// ErrCiphertextTooShort is returned when a sealed value is too small to hold
	// even an empty payload's nonce and authentication tag.
	ErrCiphertextTooShort = errors.New("secretbox: sealed value is shorter than a nonce and tag")
	// ErrWeakKey is returned when a supplied key is a placeholder made of one
	// repeated byte (for example all zeros). Generate a real random 32-byte key.
	ErrWeakKey = errors.New("secretbox: key has no entropy; generate a random 32-byte key instead of a placeholder")
)

// Box seals and opens byte slices under one fixed master key.
type Box struct {
	aead cipher.AEAD
}

// New wraps key in an AES-256-GCM AEAD. The key must be exactly KeySize bytes;
// any other length yields ErrInvalidKey.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: building AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: building GCM AEAD: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext with no additional authenticated data.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	return b.SealWithAAD(plaintext, nil)
}

// Open decrypts a value produced by Seal (or by SealWithAAD with a nil aad).
func (b *Box) Open(sealed []byte) ([]byte, error) {
	return b.OpenWithAAD(sealed, nil)
}

// SealWithAAD encrypts plaintext and binds aad into the authentication tag
// without storing it. A fresh random nonce is drawn per call, so the output is
// non-deterministic. The returned slice is one contiguous buffer laid out as
// nonce followed by the ciphertext and tag.
func (b *Box) SealWithAAD(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: drawing nonce: %w", err)
	}
	// Passing nonce as the destination appends the ciphertext+tag right after the
	// nonce bytes, giving the single nonce||ciphertext||tag envelope Open expects.
	return b.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// OpenWithAAD decrypts a value produced by SealWithAAD. The same aad supplied at
// seal time must be supplied here or the tag check fails. A value too small to
// contain a nonce and tag yields ErrCiphertextTooShort; any authentication
// failure (tampering, wrong key, wrong aad) yields the AEAD's own error.
func (b *Box) OpenWithAAD(sealed, aad []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < nonceSize+b.aead.Overhead() {
		return nil, ErrCiphertextTooShort
	}
	nonce, body := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := b.aead.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}

// LoadKey reads a base64 (standard-encoded) master key from the named
// environment variable. It errors if the variable is unset/empty, is not valid
// base64, or does not decode to exactly KeySize bytes, and returns ErrWeakKey
// for a placeholder key whose bytes are all identical.
func LoadKey(envVar string) ([]byte, error) {
	encoded := os.Getenv(envVar)
	if encoded == "" {
		return nil, fmt.Errorf("secretbox: environment variable %s is empty or unset", envVar)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %s could not be base64-decoded: %w", envVar, err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secretbox: %s decoded to %d bytes, want %d", envVar, len(key), KeySize)
	}
	if isUniformBytes(key) {
		return nil, ErrWeakKey
	}
	return key, nil
}

// isUniformBytes reports whether every byte in b equals the first one, which
// flags all-zero and single-repeated-byte placeholder keys. b is never empty at
// the one call site (LoadKey has already checked the length).
func isUniformBytes(b []byte) bool {
	for _, c := range b[1:] {
		if c != b[0] {
			return false
		}
	}
	return true
}
