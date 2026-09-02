package handler

// routes_vault.go carries the /api/vault route group split out of Routes()
// (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountVaultRoutes registers the per-user vault (unlock/lock/status/passphrase).
func (h *Handler) mountVaultRoutes(r chi.Router, authLimiter *mw.Limiter) {
	// Per-user vault (PRD #32): unlock/lock/status for the password-wrapped
	// secret DEK. All authenticated (CSRF applies to the POSTs). Unlock is a
	// password-guessing surface, so it rides the auth limiter keyed PER USER
	// (PerUserMiddleware) — not per-IP, so a stolen JWT cannot share a NAT
	// bucket with other users' logins, and not the login route's key space.
	r.Route("/vault", func(r chi.Router) {
		r.Use(mw.RequireAuth(h.q, h.cfg))
		r.With(authLimiter.PerUserMiddleware).Post("/unlock", h.VaultUnlock)
		// Create-only passphrase for passwordless (OIDC) users (PRD #45). Behind the
		// per-user limiter like unlock; refuses when a vault already exists.
		r.With(authLimiter.PerUserMiddleware).Post("/passphrase", h.VaultPassphrase)
		r.Post("/lock", h.VaultLock)
		r.Get("/status", h.VaultStatus)
	})
}
