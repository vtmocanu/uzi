package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

// csrfNonceSize is the number of random bytes drawn for each CSRF token's nonce.
const csrfNonceSize = 18

// csrfSeparator joins the nonce and MAC halves of the encoded CSRF token.
const csrfSeparator = "."

// CookieOptions controls the flags applied to the auth and CSRF cookies.
type CookieOptions struct {
	Secure bool
	TTL    time.Duration
}

// SetAuthCookies writes the session's auth cookie (the JWT, HttpOnly) and a
// matching CSRF cookie (readable by the SPA). Both share Path "/", the TTL-derived
// lifetime, opts.Secure, and SameSite=Strict. It errors only if the CSRF token
// cannot be generated, in which case no cookies are written.
func SetAuthCookies(w http.ResponseWriter, token string, opts CookieOptions) error {
	csrf, err := generateCSRFToken(token)
	if err != nil {
		return err
	}
	expires := time.Now().Add(opts.TTL)
	maxAge := int(opts.TTL.Seconds())

	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  expires,
		Secure:   opts.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrf,
		Path:     "/",
		MaxAge:   maxAge,
		Expires:  expires,
		Secure:   opts.Secure,
		HttpOnly: false, // the SPA must read this to echo it back in CSRFHeaderName
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

// ClearAuthCookies overwrites both cookies with an already-expired instance so a
// compliant browser drops them. Each keeps its original HttpOnly flag and
// SameSite=Strict; Secure follows the caller-supplied flag.
func ClearAuthCookies(w http.ResponseWriter, secure bool) {
	expired := time.Unix(0, 0)
	http.SetCookie(w, &http.Cookie{
		Name:     AuthCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  expired,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  expired,
		Secure:   secure,
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
	})
}

// generateCSRFToken derives a CSRF token bound to the auth JWT. It draws a random
// nonce and tags it with an HMAC-SHA256 keyed by the JWT string, then encodes both
// halves (nonce and MAC) as base64url joined by csrfSeparator. Because the MAC's
// key is the JWT, the token is verifiable by (and only by) a holder of that JWT and
// cannot be forged without it. ValidateCSRF reverses this encoding.
func generateCSRFToken(token string) (string, error) {
	nonce := make([]byte, csrfNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	mac := csrfMAC(nonce, token)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(nonce) + csrfSeparator + enc.EncodeToString(mac), nil
}

// csrfMAC computes HMAC-SHA256 over nonce, keyed by the auth JWT string.
func csrfMAC(nonce []byte, token string) []byte {
	h := hmac.New(sha256.New, []byte(token))
	h.Write(nonce)
	return h.Sum(nil)
}

// ValidateCSRF enforces the double-submit + HMAC check on unsafe requests. Safe
// methods (GET/HEAD/OPTIONS) always pass. Otherwise it reads the token from
// CSRFHeaderName and the JWT from the auth cookie, splits the token into its nonce
// and MAC halves, recomputes HMAC-SHA256(nonce, jwt), and compares in constant
// time. Any missing or malformed part fails closed. It never does a plain
// header==cookie string comparison.
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

	noncePart, macPart, ok := strings.Cut(header, csrfSeparator)
	if !ok {
		return false
	}
	enc := base64.RawURLEncoding
	nonce, err := enc.DecodeString(noncePart)
	if err != nil {
		return false
	}
	presentedMAC, err := enc.DecodeString(macPart)
	if err != nil {
		return false
	}

	return hmac.Equal(presentedMAC, csrfMAC(nonce, authCookie.Value))
}
