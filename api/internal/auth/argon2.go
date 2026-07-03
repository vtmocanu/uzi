// Package auth holds the password hashing, JWT, cookie and CSRF primitives.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters follow the OWASP Password Storage Cheat Sheet
// recommendation for argon2id: 19 MiB memory, 2 iterations, 1 degree of
// parallelism, 16-byte salt, 32-byte key. Chosen over bcrypt (bottega) per the
// PRD.
const (
	argonMemoryKiB  = 19 * 1024 // 19 MiB
	argonTime       = 2
	argonThreads    = 1
	argonSaltLength = 16
	argonKeyLength  = 32
)

// ErrInvalidHash is returned when a stored hash string is not a well-formed
// PHC argon2id encoding.
var ErrInvalidHash = errors.New("invalid argon2 hash format")

// HashPassword returns a PHC-format argon2id encoding of the password:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLength)

	b64 := base64.RawStdEncoding
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the PHC-encoded argon2id
// hash. The comparison is constant-time. Parameters are read from the encoded
// hash so older hashes with different costs still verify.
func VerifyPassword(password, encoded string) (bool, error) {
	memory, time32, threads, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	computed := argon2.IDKey([]byte(password), salt, time32, memory, threads, uint32(len(key)))
	if subtle.ConstantTimeEq(int32(len(computed)), int32(len(key))) == 0 {
		return false, nil
	}
	return subtle.ConstantTimeCompare(computed, key) == 1, nil
}

func decodeHash(encoded string) (memory, time uint32, threads uint8, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<key>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return 0, 0, 0, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidHash, version)
	}

	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}

	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil {
		return 0, 0, 0, nil, nil, ErrInvalidHash
	}
	return memory, time, threads, salt, key, nil
}
