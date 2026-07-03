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
	box  *secretbox.Box
}

// New constructs a Handler.
func New(pool *pgxpool.Pool, q *store.Queries, cfg config.Config, box *secretbox.Box) *Handler {
	return &Handler{pool: pool, q: q, cfg: cfg, box: box}
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

// Routes builds the API router. The limiter is applied per-route to the
// register and login endpoints.
func (h *Handler) Routes(limiter *mw.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)

	r.Route("/api", func(r chi.Router) {
		r.Get("/health", h.Health)

		r.Route("/auth", func(r chi.Router) {
			r.With(limiter.Middleware).Post("/register", h.Register)
			r.With(limiter.Middleware).Post("/login", h.Login)

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

		r.Route("/admin", func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Use(mw.RequireAdmin)
			r.Get("/users", h.ListUsers)
			r.Patch("/users/{id}", h.PatchUser)
		})
	})

	return r
}
