package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the JWT payload. UserID identifies the subject; TokenVersion is
// compared against the DB on every request so a bump (logout, password change,
// admin deactivation) instantly invalidates every token previously issued to
// that user.
type Claims struct {
	BrowserSlotID string `json:"browser_slot_id,omitempty"`
	UserID        string `json:"user_id"`
	TokenVersion  int32  `json:"token_version"`
	jwt.RegisteredClaims
}

// IssueToken mints an HS256 JWT for the user with the given token version and
// TTL.
func IssueToken(secret []byte, userID string, tokenVersion int32, ttl time.Duration) (string, error) {
	token, _, err := PrepareToken(secret, userID, tokenVersion, ttl, "")
	return token, err
}

// PrepareToken returns a signed token and its exact claims, including expiry.
// An empty browserSlotID retains the legacy session payload.
func PrepareToken(secret []byte, userID string, tokenVersion int32, ttl time.Duration, browserSlotID string) (string, *Claims, error) {
	now := time.Now()
	claims := Claims{
		BrowserSlotID: browserSlotID,
		UserID:        userID,
		TokenVersion:  tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	signed, err := SignClaims(secret, claims)
	if err != nil {
		return "", nil, err
	}
	return signed, &claims, nil
}

// SignClaims signs claims exactly as supplied, without changing expiry or issued-at.
// A legacy upgrade can copy incoming validated claims, set BrowserSlotID, and sign.
func SignClaims(secret []byte, claims Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// ParseToken validates the signature and expiry and returns the claims. The
// signing method is pinned to HMAC to prevent algorithm-confusion attacks
// (e.g. a token forged with alg=none or an RS256/HS256 key confusion).
func ParseToken(secret []byte, tokenString string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	return claims, nil
}
