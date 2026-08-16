package middleware

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/vtmocanu/uzi/api/internal/auth"
)

func TestShouldRefresh(t *testing.T) {
	const ttl = 168 * time.Hour
	now := time.Now()

	tests := []struct {
		name      string
		expiresAt *jwt.NumericDate
		want      bool
	}{
		{"missing expiry refreshes", nil, true},
		{"fresh token does not refresh", jwt.NewNumericDate(now.Add(ttl)), false},
		{"just over half elapsed refreshes", jwt.NewNumericDate(now.Add(ttl/2 - time.Minute)), true},
		{"just under half elapsed does not refresh", jwt.NewNumericDate(now.Add(ttl/2 + time.Minute)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &auth.Claims{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: tt.expiresAt}}
			if got := shouldRefresh(claims, ttl); got != tt.want {
				t.Errorf("shouldRefresh = %v, want %v", got, tt.want)
			}
		})
	}
}
