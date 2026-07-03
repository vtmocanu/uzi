package middleware

import (
	"context"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

const workerKey ctxKey = iota + 1

// WorkerStore is the narrow store dependency for worker authentication.
// *store.Queries satisfies it.
type WorkerStore interface {
	GetWorkerByTokenHash(ctx context.Context, tokenHash []byte) (store.Worker, error)
}

// WorkerFromContext returns the authenticated worker set by RequireWorker.
func WorkerFromContext(ctx context.Context) (store.Worker, bool) {
	wkr, ok := ctx.Value(workerKey).(store.Worker)
	return wkr, ok
}

// ContextWithWorker carries an authenticated worker (used by RequireWorker and
// by handler tests that exercise a worker endpoint without a real token).
func ContextWithWorker(ctx context.Context, wkr store.Worker) context.Context {
	return context.WithValue(ctx, workerKey, wkr)
}

// RequireWorker authenticates a worker from its Bearer join token: the token is
// sha256-hashed and looked up against workers.token_hash. Unlike RequireAuth
// this is not cookie-based, so there is no CSRF step — the credential is a
// bearer secret the worker holds, not an ambient cookie a browser auto-sends.
func RequireWorker(q WorkerStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := jointoken.FromAuthorizationHeader(r.Header.Get("Authorization"))
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
				return
			}
			hash := jointoken.Hash(token)
			wkr, err := q.GetWorkerByTokenHash(r.Context(), hash)
			if err != nil {
				// Do not distinguish "no such token" from other lookup failures —
				// a probing worker learns nothing about which tokens exist.
				httpx.Error(w, http.StatusUnauthorized, "invalid worker token")
				return
			}
			// Belt-and-suspenders constant-time credential check. The row was
			// found by an indexed equality on the sha256 of a 256-bit random
			// token (no exploitable timing channel on its own), so this compare is
			// always true here; it makes the constant-time guarantee explicit and
			// keeps holding if the lookup is ever refactored to fetch-then-compare.
			if !jointoken.Equal(hash, wkr.TokenHash) {
				httpx.Error(w, http.StatusUnauthorized, "invalid worker token")
				return
			}
			ctx := ContextWithWorker(r.Context(), wkr)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
