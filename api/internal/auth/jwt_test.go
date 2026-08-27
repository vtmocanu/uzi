package auth

import (
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
