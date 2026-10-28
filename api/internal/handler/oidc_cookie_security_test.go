package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// HTTPS deployments must keep Secure on both creation and deletion. The
// configured HTTP development origin intentionally uses a non-Secure cookie.
func TestOIDCStateCookieConfiguredSecurity(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("secure=%t", secure), func(t *testing.T) {
			h := oidcTestHandler(t, nil)
			h.cfg.CookieSecure = secure
			for _, action := range []string{"set", "clear"} {
				t.Run(action, func(t *testing.T) {
					rec := httptest.NewRecorder()
					if action == "set" {
						if err := h.setOIDCStateCookie(rec, oidcStateData{State: "security-fixture"}); err != nil {
							t.Fatal(err)
						}
					} else {
						h.clearOIDCStateCookie(rec)
					}
					cookies := rec.Result().Cookies()
					if len(cookies) != 1 {
						t.Fatalf("state cookie count=%d", len(cookies))
					}
					cookie := cookies[0]
					if cookie.Name != oidcStateCookieName || cookie.Path != oidcStateCookiePath ||
						cookie.Secure != secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
						t.Fatalf("state cookie flags: name=%q path=%q Secure=%v HttpOnly=%v SameSite=%v", cookie.Name, cookie.Path, cookie.Secure, cookie.HttpOnly, cookie.SameSite)
					}
					if action == "clear" && cookie.MaxAge >= 0 {
						t.Fatal("state cookie deletion did not expire the cookie")
					}
					if action == "set" && cookie.MaxAge <= 0 {
						t.Fatal("state cookie creation has no bounded lifetime")
					}
				})
			}
		})
	}
}
