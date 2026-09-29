package middleware

import (
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// RequireFetcher authenticates uzi-fetcher (PRD #1906 M3) from its service Bearer: the
// token is sha256-hashed and compared in constant time against the hash the api was
// configured with (UZI_FETCHER_TOKEN_SHA256). It is RequireController's sibling with the
// same shape and the same reasons (a static deployment credential, only its hash in the
// api, no cookies and so no CSRF, no identity to put on the context), and a separate
// function so the two credentials can never be confused: neither hash authorizes the
// other's routes.
//
// A missing, non-Bearer or empty Bearer is refused before any hashing. The run
// credential the fetcher forwards in the body is a different secret and is checked by
// the handler; this only says the caller is the fetcher.
func RequireFetcher(wantHash []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := jointoken.FromAuthorizationHeader(r.Header.Get("Authorization"))
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, "fetcher authentication required")
				return
			}
			if len(wantHash) == 0 || !jointoken.Equal(jointoken.Hash(token), wantHash) {
				httpx.Error(w, http.StatusUnauthorized, "invalid fetcher token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
