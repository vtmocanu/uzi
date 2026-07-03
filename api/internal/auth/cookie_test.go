package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetAuthCookiesFlags(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := SetAuthCookies(rec, "the-jwt", CookieOptions{Secure: true, TTL: time.Hour}); err != nil {
		t.Fatalf("set cookies: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("expected 2 cookies, got %d", len(cookies))
	}
	var auth, csrf *http.Cookie
	for _, c := range cookies {
		switch c.Name {
		case AuthCookieName:
			auth = c
		case CSRFCookieName:
			csrf = c
		}
	}
	if auth == nil || csrf == nil {
		t.Fatal("missing auth or csrf cookie")
	}
	if !auth.HttpOnly {
		t.Error("auth cookie must be HttpOnly")
	}
	if csrf.HttpOnly {
		t.Error("csrf cookie must be readable (not HttpOnly)")
	}
	for _, c := range cookies {
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("%s SameSite = %v, want Strict", c.Name, c.SameSite)
		}
		if !c.Secure {
			t.Errorf("%s Secure = false, want true", c.Name)
		}
	}
}

func TestValidateCSRF(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := SetAuthCookies(rec, "the-jwt", CookieOptions{TTL: time.Hour}); err != nil {
		t.Fatalf("set cookies: %v", err)
	}
	var authCookie, csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case AuthCookieName:
			authCookie = c
		case CSRFCookieName:
			csrfCookie = c
		}
	}

	// Safe method: always passes.
	get := httptest.NewRequest(http.MethodGet, "/", nil)
	if !ValidateCSRF(get) {
		t.Error("GET should not require CSRF")
	}

	// POST with matching header + cookie passes.
	post := httptest.NewRequest(http.MethodPost, "/", nil)
	post.AddCookie(authCookie)
	post.Header.Set(CSRFHeaderName, csrfCookie.Value)
	if !ValidateCSRF(post) {
		t.Error("valid CSRF token rejected")
	}

	// POST without header fails.
	noHeader := httptest.NewRequest(http.MethodPost, "/", nil)
	noHeader.AddCookie(authCookie)
	if ValidateCSRF(noHeader) {
		t.Error("missing CSRF header accepted")
	}

	// POST with tampered token fails.
	tampered := httptest.NewRequest(http.MethodPost, "/", nil)
	tampered.AddCookie(authCookie)
	tampered.Header.Set(CSRFHeaderName, "deadbeef.deadbeef")
	if ValidateCSRF(tampered) {
		t.Error("tampered CSRF token accepted")
	}
}
