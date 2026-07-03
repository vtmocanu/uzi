package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Cookie names. The auth cookie is HttpOnly (never readable by JS); the CSRF
// cookie is readable so the SPA can echo it back in a header.
const (
	AuthCookieName = "uzi_auth"
	CSRFCookieName = "uzi_csrf"
	// CSRFHeaderName is the request header the SPA sends the CSRF token in.
	CSRFHeaderName = "X-CSRF-Token"
)

// CookieOptions controls the flags applied to the auth and CSRF cookies.
type CookieOptions struct {
	Secure bool
	TTL    time.Duration
}

// SetAuthCookies writes the HttpOnly auth cookie (the JWT) and the readable,
// HMAC-bound CSRF cookie. Both use SameSite=Strict. Adapted from multica's
// cookie handling; the CSRF token binds to the JWT via HMAC so an attacker who
// can plant a cookie on a sibling subdomain still cannot forge a valid token
// without knowing the JWT.
func SetAuthCookies(w http.ResponseWriter, token string, opts CookieOptions) error {
	csrf, err := generateCSRFToken(token)
	if err != nil {
		return err
	}
	now := time.Now()

	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(opts.TTL.Seconds()),
		Expires:  now.Add(opts.TTL),
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrf,
		Path:     "/",
		MaxAge:   int(opts.TTL.Seconds()),
		Expires:  now.Add(opts.TTL),
		HttpOnly: false,
		Secure:   opts.Secure,
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// ClearAuthCookies expires both cookies.
func ClearAuthCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{AuthCookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0),
			HttpOnly: name == AuthCookieName,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

// generateCSRFToken returns hex(nonce) + "." + hex(HMAC-SHA256(nonce, jwt)).
func generateCSRFToken(token string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(nonce)
	return hex.EncodeToString(nonce) + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

// ValidateCSRF verifies the CSRF header against the auth cookie for
// state-changing methods. Safe methods always pass. The header value is
// verified by recomputing the HMAC rather than a plain cookie==header compare.
func ValidateCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	header := r.Header.Get(CSRFHeaderName)
	if header == "" {
		return false
	}
	authCookie, err := r.Cookie(AuthCookieName)
	if err != nil || authCookie.Value == "" {
		return false
	}

	parts := strings.SplitN(header, ".", 2)
	if len(parts) != 2 {
		return false
	}
	nonce, err := hex.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := hex.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(authCookie.Value))
	mac.Write(nonce)
	return hmac.Equal(mac.Sum(nil), sig)
}
