package handler

// routes_vault.go carries the /api/vault route group split out of Routes()
// (PRD #1008, epic #915 Batch 3).

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountVaultRoutes registers the per-user vault (unlock/lock/status/passphrase).
func (h *Handler) mountVaultRoutes(r chi.Router, authLimiter *mw.Limiter) {
	r.Route("/vault", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			// Session or CLI token; cookie POSTs still require CSRF. Authenticate
			// before limiting so cookies and every token share the user's budget.
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.With(authLimiter.PerUserMiddleware).Post("/unlock", h.VaultUnlock)
			r.Get("/status", h.VaultStatus)
		})
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			// Create-only passphrase for passwordless (OIDC) users (PRD #45).
			// Lock and passphrase creation remain cookie-only.
			r.With(authLimiter.PerUserMiddleware).Post("/passphrase", h.VaultPassphrase)
			r.Post("/lock", h.VaultLock)
		})
	})
}
