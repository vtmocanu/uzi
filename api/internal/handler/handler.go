// Package handler implements the HTTP API: auth, current-user and admin
// endpoints, plus the router wiring.
package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Handler bundles the dependencies shared by every HTTP handler.
type Handler struct {
	pool *pgxpool.Pool
	q    *store.Queries
	cfg  config.Config
	// box is the generic secret cipher used by the per-user secret endpoints
	// (Anthropic token). svc owns the forge-specific machinery (which also holds
	// its own box for PAT sealing); the two share the same key material.
	box *secretbox.Box
	svc *forgesvc.Service
}

// New constructs a Handler.
func New(pool *pgxpool.Pool, q *store.Queries, cfg config.Config, box *secretbox.Box, svc *forgesvc.Service) *Handler {
	return &Handler{pool: pool, q: q, cfg: cfg, box: box, svc: svc}
}

// userDTO is the safe, JSON-serializable view of a user. It never exposes the
// password hash or token_version.
type userDTO struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName *string    `json:"display_name"`
	IsAdmin     bool       `json:"is_admin"`
	IsActive    bool       `json:"is_active"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLogin   *time.Time `json:"last_login"`
}

func toDTO(u store.User) userDTO {
	dto := userDTO{
		ID:        u.ID.String(),
		Email:     u.Email,
		IsAdmin:   u.IsAdmin,
		IsActive:  u.IsActive,
		CreatedAt: u.CreatedAt.Time,
	}
	if u.DisplayName.Valid {
		dto.DisplayName = &u.DisplayName.String
	}
	if u.LastLogin.Valid {
		t := u.LastLogin.Time
		dto.LastLogin = &t
	}
	return dto
}

// Health reports API and DB liveness for the compose healthcheck chain.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Routes builds the API router. authLimiter is applied per-route to the
// register and login endpoints; forgeLimiter is a per-user budget on the
// forge-proxying endpoints (verify/projects/sync/move) so one user cannot
// hammer the upstream forge.
func (h *Handler) Routes(authLimiter, forgeLimiter *mw.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", h.Health)

		r.Route("/auth", func(r chi.Router) {
			r.With(authLimiter.Middleware).Post("/register", h.Register)
			r.With(authLimiter.Middleware).Post("/login", h.Login)

			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAuth(h.q, h.cfg))
				r.Post("/logout", h.Logout)
				r.Get("/me", h.Me)
			})
		})

		// Current-user secrets (per-user, encrypted at rest). No admin read
		// path to other users' secret values by design.
		r.Route("/me/secrets", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/", h.ListMySecrets)
			r.Put("/anthropic_token", h.PutAnthropicToken)
			r.Delete("/anthropic_token", h.DeleteAnthropicToken)
		})

		// Agent templates: all authenticated users can read and preview; only
		// admins can create, edit, delete, or reset (closes bottega's hole
		// where any user rewrites the shared prompts everyone's agents run).
		r.Route("/agent-templates", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Get("/", h.ListAgentTemplates)
			r.Get("/{id}", h.GetAgentTemplate)
			r.Get("/{id}/rendered", h.GetRenderedAgentTemplate)

			r.Group(func(r chi.Router) {
				r.Use(mw.RequireAdmin)
				r.Post("/", h.CreateAgentTemplate)
				r.Put("/{id}", h.UpdateAgentTemplate)
				r.Delete("/{id}", h.DeleteAgentTemplate)
				r.Post("/{id}/reset", h.ResetAgentTemplate)
			})
		})

		r.Route("/admin", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Use(mw.RequireAdmin)
			r.Get("/users", h.ListUsers)
			r.Patch("/users/{id}", h.PatchUser)
		})

		// Forge integration: connections, repo discovery, and the label-synced
		// kanban board. Every route is per-user authorized through the owning
		// connection's user_id (see the handlers).
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))

			r.Get("/forge/config", h.ForgeConfig)

			r.Route("/forge/connections", func(r chi.Router) {
				r.Post("/", h.CreateConnection)
				r.Get("/", h.ListConnections)
				// verify + projects hit the upstream forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/verify", h.VerifyConnection)
				r.Delete("/{id}", h.DeleteConnection)
				r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/projects", h.ListProjects)
			})

			r.Route("/repos", func(r chi.Router) {
				r.Get("/", h.ListRepos)
				r.Put("/{id}", h.SetRepoEnabled)
				r.Get("/{id}/board", h.GetBoard)
				r.Put("/{id}/board/columns", h.ConfigureColumns)
				// move + sync write/read through to the forge → per-user budget.
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/move", h.MoveIssue)
				r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/sync", h.SyncRepo)
			})
		})
	})

	return r
}
