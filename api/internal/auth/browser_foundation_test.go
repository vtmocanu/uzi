package auth

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestValidateCSRFTokenAllMethods(t *testing.T) {
	token := "incoming-jwt"
	valid, err := generateCSRFToken(token)
	if err != nil {
		t.Fatal(err)
	}
	tampered := "A" + valid[1:]
	if valid[0] == 'A' {
		tampered = "B" + valid[1:]
	}
	other, err := generateCSRFToken("other-jwt")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, tc := range []struct {
			name, header, cookie string
			want                 bool
		}{
			{"missing header", "", token, false},
			{"missing cookie", valid, "", false},
			{"malformed", "bad.token.extra", token, false},
			{"tampered", tampered, token, false},
			{"other JWT", other, token, false},
			{"valid", valid, token, true},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				r := httptest.NewRequest(method, "/", nil)
				if tc.cookie != "" {
					r.AddCookie(&http.Cookie{Name: AuthCookieName, Value: tc.cookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
				}
				r.Header.Set(CSRFHeaderName, tc.header)
				if got := ValidateCSRFToken(r); got != tc.want {
					t.Fatalf("strict = %v, want %v", got, tc.want)
				}
				safe := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
				if got := ValidateCSRF(r); got != (safe || tc.want) {
					t.Fatalf("ordinary = %v", got)
				}
			})
		}
	}
}

func TestPrepareAndSignExactClaims(t *testing.T) {
	secret := []byte("test-signing-secret")
	for _, slot := range []string{"", "browser-one"} {
		token, claims, err := PrepareToken(secret, "owner", 7, time.Hour, slot)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseToken(secret, token)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(parsed, claims) {
			t.Fatal("prepared claims differ from signed claims")
		}
		if parsed.BrowserSlotID != slot || parsed.TokenVersion != 7 || parsed.Subject != "owner" {
			t.Fatal("lost identity")
		}
		before := *claims
		claims.BrowserSlotID = "legacy-upgrade"
		upgraded, err := SignClaims(secret, *claims)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseToken(secret, upgraded)
		if err != nil {
			t.Fatal(err)
		}
		if got.BrowserSlotID != "legacy-upgrade" || !got.ExpiresAt.Equal(before.ExpiresAt.Time) || !got.IssuedAt.Equal(before.IssuedAt.Time) {
			t.Fatal("upgrade changed lifetime or lost slot")
		}
	}
	token, err := IssueToken(secret, "owner", 7, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ParseToken(secret, token)
	if err != nil || legacy.BrowserSlotID != "" {
		t.Fatalf("legacy issue/parse: %v", err)
	}
}
