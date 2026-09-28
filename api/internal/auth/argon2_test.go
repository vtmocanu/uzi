package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct-horse-battery-staple" {
		t.Fatal("password stored in plaintext")
	}

	ok, err := VerifyPassword("correct-horse-battery-staple", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}

	ok, err = VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password verified")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same-password-value")
	b, _ := HashPassword("same-password-value")
	if a == b {
		t.Fatal("identical hashes for the same password: salt not applied")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", "$argon2id$v=19$m=1$x$y", "$bcrypt$abc"} {
		if _, err := VerifyPassword("pw", bad); err == nil {
			t.Errorf("expected error for malformed hash %q", bad)
		}
	}
}

func TestHashPasswordEncodesOWASPParams(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	const wantPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(hash, wantPrefix) {
		t.Fatalf("hash = %q, want prefix %q", hash, wantPrefix)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Fatalf("hash has %d $-parts, want 6: %q", len(parts), hash)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if len(salt) != 16 || len(key) != 32 {
		t.Errorf("salt/key lengths = %d/%d, want 16/32", len(salt), len(key))
	}
}

// encodeArgon2id builds a PHC argon2id string with explicit cost parameters,
// standing in for a hash stored under an older (different) cost policy.
func encodeArgon2id(password string, salt []byte, memory, time uint32, threads uint8, keyLen uint32) string {
	key := argon2.IDKey([]byte(password), salt, time, memory, threads, keyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time, threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func TestVerifyUsesParamsFromHash(t *testing.T) {
	salt := []byte("0123456789abcdef")
	legacy := encodeArgon2id("legacy-password-value", salt, 8*1024, 1, 2, 32)

	ok, err := VerifyPassword("legacy-password-value", legacy)
	if err != nil {
		t.Fatalf("verify legacy: %v", err)
	}
	if !ok {
		t.Error("password stored under older cost parameters did not verify")
	}

	ok, err = VerifyPassword("wrong-password-value", legacy)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Error("wrong password verified against the legacy hash")
	}
}

func TestVerifyRejectsUnsupportedVersion(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	old := strings.Replace(hash, "$v=19$", "$v=16$", 1)
	ok, err := VerifyPassword("correct-horse-battery-staple", old)
	if !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("err = %v, want ErrInvalidHash", err)
	}
	if ok {
		t.Error("ok = true alongside an unsupported-version error")
	}
}

func TestVerifyDerivesStoredKeyLength(t *testing.T) {
	// Argon2's output length is an input to its final hash, so a 16-byte key is
	// not a prefix of the 32-byte one: this hash verifies only if the verifier
	// derives len(stored key) bytes rather than the current policy's 32.
	short := encodeArgon2id("short-key-password", []byte("fedcba9876543210"), 19*1024, 2, 1, 16)

	ok, err := VerifyPassword("short-key-password", short)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("password did not verify against a hash with a 16-byte key")
	}

	ok, err = VerifyPassword("wrong-password-value", short)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Error("wrong password verified against the 16-byte-key hash")
	}
}
