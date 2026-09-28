package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testSecret = []byte("test-secret-value-32-bytes-long!!")

func TestIssueParseRoundTrip(t *testing.T) {
	token, err := IssueToken(testSecret, "user-123", 7, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := ParseToken(testSecret, token)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("user id = %q, want user-123", claims.UserID)
	}
	if claims.TokenVersion != 7 {
		t.Errorf("token version = %d, want 7", claims.TokenVersion)
	}
	if claims.Subject != "user-123" {
		t.Errorf("subject = %q, want user-123", claims.Subject)
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	token, _ := IssueToken(testSecret, "u", 1, time.Hour)
	if _, err := ParseToken([]byte("a-different-secret-value-32-byte!"), token); err == nil {
		t.Fatal("token verified under the wrong secret")
	}
}

func TestParseRejectsExpired(t *testing.T) {
	token, _ := IssueToken(testSecret, "u", 1, -time.Minute)
	if _, err := ParseToken(testSecret, token); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestParseRejectsAlgNone(t *testing.T) {
	// Forge an unsigned token (alg=none) — a classic JWT downgrade attack.
	claims := Claims{UserID: "attacker", TokenVersion: 1}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build none token: %v", err)
	}
	if _, err := ParseToken(testSecret, raw); err == nil {
		t.Fatal("alg=none token accepted")
	}
}

func TestParseRejectsOtherHMACAlg(t *testing.T) {
	// HS384 under the same secret passes the *jwt.SigningMethodHMAC type check;
	// only the HS256 pin rejects it.
	claims := Claims{
		UserID:       "user-123",
		TokenVersion: 7,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-123",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	hs256, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	got, err := ParseToken(testSecret, hs256)
	if err != nil {
		t.Fatalf("HS256 token rejected: %v", err)
	}
	if got.UserID != "user-123" {
		t.Errorf("HS256 user id = %q, want user-123", got.UserID)
	}

	hs384, err := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString(testSecret)
	if err != nil {
		t.Fatalf("sign HS384: %v", err)
	}
	if _, err := ParseToken(testSecret, hs384); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Errorf("HS384 token: err = %v, want ErrTokenSignatureInvalid", err)
	}
}
