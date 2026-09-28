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

// cookiesByName indexes the Set-Cookie headers a recorder captured.
func cookiesByName(t *testing.T, rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	t.Helper()
	out := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c
	}
	if out[AuthCookieName] == nil || out[CSRFCookieName] == nil {
		t.Fatalf("missing auth or csrf cookie in %v", out)
	}
	return out
}

func TestValidateCSRFRejectsTokenFromAnotherSession(t *testing.T) {
	recA := httptest.NewRecorder()
	if err := SetAuthCookies(recA, "jwt-A", CookieOptions{TTL: time.Hour}); err != nil {
		t.Fatalf("set cookies A: %v", err)
	}
	recB := httptest.NewRecorder()
	if err := SetAuthCookies(recB, "jwt-B", CookieOptions{TTL: time.Hour}); err != nil {
		t.Fatalf("set cookies B: %v", err)
	}
	a, b := cookiesByName(t, recA), cookiesByName(t, recB)

	// Session B's own token verifies against session B's JWT.
	own := httptest.NewRequest(http.MethodPost, "/", nil)
	own.AddCookie(b[AuthCookieName])
	own.Header.Set(CSRFHeaderName, b[CSRFCookieName].Value)
	if !ValidateCSRF(own) {
		t.Fatal("session B's own CSRF token rejected")
	}

	// A well-formed token minted for session A is not valid for session B:
	// the MAC is keyed by the JWT, so the token does not transfer.
	cross := httptest.NewRequest(http.MethodPost, "/", nil)
	cross.AddCookie(b[AuthCookieName])
	cross.Header.Set(CSRFHeaderName, a[CSRFCookieName].Value)
	if ValidateCSRF(cross) {
		t.Error("session A's CSRF token accepted with session B's auth cookie")
	}
}

func TestValidateCSRFMethodClassification(t *testing.T) {
	// No header and no cookie: only the safe methods may pass.
	cases := []struct {
		method string
		want   bool
	}{
		{http.MethodGet, true},
		{http.MethodHead, true},
		{http.MethodOptions, true},
		{http.MethodPost, false},
		{http.MethodPut, false},
		{http.MethodPatch, false},
		{http.MethodDelete, false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "/", nil)
		if got := ValidateCSRF(r); got != tc.want {
			t.Errorf("%s without CSRF: ValidateCSRF = %v, want %v", tc.method, got, tc.want)
		}
	}
}

func TestValidateCSRFRequiresNonEmptyAuthCookie(t *testing.T) {
	// A header that is a correct HMAC for the empty key: without the non-empty
	// auth-cookie guard, anyone could mint it with no session at all.
	emptyKeyed, err := generateCSRFToken("")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	noCookie := httptest.NewRequest(http.MethodPost, "/", nil)
	noCookie.Header.Set(CSRFHeaderName, emptyKeyed)
	if ValidateCSRF(noCookie) {
		t.Error("CSRF passed with no auth cookie")
	}

	emptyCookie := httptest.NewRequest(http.MethodPost, "/", nil)
	emptyCookie.AddCookie(&http.Cookie{Name: AuthCookieName, Value: ""})
	emptyCookie.Header.Set(CSRFHeaderName, emptyKeyed)
	if ValidateCSRF(emptyCookie) {
		t.Error("CSRF passed with an empty auth cookie and an empty-key token")
	}
}

func TestSetAuthCookiesLifetimeAndPath(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := SetAuthCookies(rec, "the-jwt", CookieOptions{Secure: false, TTL: 90 * time.Minute}); err != nil {
		t.Fatalf("set cookies: %v", err)
	}
	got := cookiesByName(t, rec)
	if v := got[AuthCookieName].Value; v != "the-jwt" {
		t.Errorf("auth cookie value = %q, want the-jwt", v)
	}
	for _, name := range []string{AuthCookieName, CSRFCookieName} {
		c := got[name]
		if c.MaxAge != 5400 {
			t.Errorf("%s MaxAge = %d, want 5400", name, c.MaxAge)
		}
		if c.Path != "/" {
			t.Errorf("%s Path = %q, want /", name, c.Path)
		}
		if c.Secure {
			t.Errorf("%s Secure = true, want false (opts.Secure=false)", name)
		}
	}
}

func TestClearAuthCookies(t *testing.T) {
	for _, secure := range []bool{true, false} {
		rec := httptest.NewRecorder()
		ClearAuthCookies(rec, secure)
		cookies := rec.Result().Cookies()
		if len(cookies) != 2 {
			t.Fatalf("secure=%v: expected 2 cookies, got %d", secure, len(cookies))
		}
		got := cookiesByName(t, rec)
		wantHTTPOnly := map[string]bool{AuthCookieName: true, CSRFCookieName: false}
		for name, httpOnly := range wantHTTPOnly {
			c := got[name]
			if c.Value != "" {
				t.Errorf("secure=%v: %s Value = %q, want empty", secure, name, c.Value)
			}
			// A browser only replaces a cookie whose Path matches the one
			// SetAuthCookies wrote.
			if c.Path != "/" {
				t.Errorf("secure=%v: %s Path = %q, want /", secure, name, c.Path)
			}
			if c.MaxAge != -1 {
				t.Errorf("secure=%v: %s MaxAge = %d, want -1 (Max-Age=0)", secure, name, c.MaxAge)
			}
			if !c.Expires.Equal(time.Unix(0, 0)) {
				t.Errorf("secure=%v: %s Expires = %v, want the Unix epoch", secure, name, c.Expires)
			}
			if c.HttpOnly != httpOnly {
				t.Errorf("secure=%v: %s HttpOnly = %v, want %v", secure, name, c.HttpOnly, httpOnly)
			}
			if c.Secure != secure {
				t.Errorf("secure=%v: %s Secure = %v, want %v", secure, name, c.Secure, secure)
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("secure=%v: %s SameSite = %v, want Strict", secure, name, c.SameSite)
			}
		}
	}
}
