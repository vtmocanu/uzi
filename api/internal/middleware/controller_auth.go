package middleware

import (
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
)

// RequireController authenticates the hosted-worker controller (PRD #58 Decision
// 2) from its Bearer credential: the token is sha256-hashed and compared, in
// constant time, against the hash the api was configured with
// (WORKER_HOSTING_CONTROLLER_TOKEN_SHA256).
//
// It is a sibling of RequireWorker, not a copy of it, and differs on two axes:
//
//   - No cookies, no CSRF — same reasoning as RequireWorker. The credential is a
//     bearer secret the controller holds, not an ambient cookie a browser sends.
//   - No DB lookup, and no plaintext anywhere in the api. A worker's credential is
//     user data (minted per worker, hashed into the workers table); the
//     controller's is a single static deployment credential, closer to JWT_SECRET.
//     Holding only its hash in config means an api memory or environment
//     disclosure yields nothing that authenticates — the /proc/<pid>/environ leak
//     class that docs/proc-hardening.md closed for the worker and that PRD #58
//     Decision 3 keeps closed by file-mounting the worker's token.
//
// There is no controller identity to put on the context: the credential IS the
// authorization (one controller, whole-fleet scope), so unlike RequireWorker this
// adds nothing for handlers to read.
//
// The unsalted sha256 is sound for the same reason jointoken documents: the
// credential is uniformly random 256-bit data (the chart generates it; the docs
// say `openssl rand -base64 32`), so there is no low-entropy keyspace to
// precompute against. That premise is the operator's to uphold — the api holds
// only a hash and so cannot verify the plaintext's entropy itself.
func RequireController(wantHash []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := jointoken.FromAuthorizationHeader(r.Header.Get("Authorization"))
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, "controller authentication required")
				return
			}
			if !jointoken.Equal(jointoken.Hash(token), wantHash) {
				httpx.Error(w, http.StatusUnauthorized, "invalid controller token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
