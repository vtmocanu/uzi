package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/theme"
)

// minPasswordLen is the server-side minimum; the client adds strength
// feedback but this is the authoritative floor. Sourced from the auth package
// so registration and admin seeding share one policy.
const (
	minPasswordLen = auth.MinPasswordLen
	maxPasswordLen = auth.MaxPasswordLen
)

// dummyHash is a valid argon2id hash used to equalize login timing when the
// email is unknown, so an attacker cannot distinguish "no such user" from
// "wrong password" by response time. Computed once at startup.
var dummyHash string

func init() {
	h, err := auth.HashPassword("uzi-timing-equalizer-not-a-real-password")
	if err != nil {
		panic("compute dummy hash: " + err.Error())
	}
	dummyHash = h
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// emailDomain returns the lowercased domain of a parsed addr-spec
// (mail.Address.Address), taken as the substring after its final '@'. It must be
// given the addr-spec, never the raw input: a quoted local part may itself
// contain '@' (e.g. `"a@b"@x.com`), and a final-'@' split of the addr-spec still
// yields the true domain, whereas the raw display-name/comment forms would not.
func emailDomain(addrSpec string) string {
	at := strings.LastIndex(addrSpec, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(addrSpec[at+1:])
}

// emailDomainAllowed reports whether addrSpec's domain is permitted by the
// allowlist. An empty allowlist permits every domain (preserving the open
// default). Matching is exact and case-insensitive — no subdomain wildcards, so
// a.example.com does not match example.com. The allowlist is already
// lowercased by config parsing.
func emailDomainAllowed(addrSpec string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	d := emailDomain(addrSpec)
	for _, a := range allowed {
		if d == a {
			return true
		}
	}
	return false
}

// Register creates a new account. The first account ever created becomes an
// admin; the check-and-insert runs inside a transaction holding
// pg_advisory_xact_lock so two concurrent first-registrations cannot both win
// admin. On success the user is logged in (cookies set).
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	// Registration kill-switch: a well-formed request the policy forbids, so 403
	// (not 400). Checked first, before the body is even inspected. Login is
	// unaffected; the seed admin is created out-of-band (seedAdmin in main.go).
	if !h.cfg.RegistrationEnabled {
		httpx.Error(w, http.StatusForbidden, "registration is disabled")
		return
	}

	var req registerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	addr, err := mail.ParseAddress(strings.TrimSpace(strings.ToLower(req.Email)))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "a valid email is required")
		return
	}
	// Canonicalize to the parsed addr-spec: mail.ParseAddress also accepts
	// display-name/comment forms (e.g. "Alice <alice@x.com>") whose raw string
	// carries junk; addr.Address is just the address, which is also what we match
	// the domain allowlist against.
	email := addr.Address
	if !emailDomainAllowed(email, h.cfg.AllowedEmailDomains) {
		// Domain-list disclosure is acceptable for an internal tool (the register
		// page shows the same hint), and specific beats a generic denial.
		httpx.Error(w, http.StatusForbidden, "registration is restricted to: "+strings.Join(h.cfg.AllowedEmailDomains, ", "))
		return
	}
	if len(req.Password) < minPasswordLen {
		httpx.Error(w, http.StatusBadRequest, "password must be at least 12 characters")
		return
	}
	if len(req.Password) > maxPasswordLen {
		httpx.Error(w, http.StatusBadRequest, "password is too long")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		slog.Error("hash password", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	displayName := pgtype.Text{}
	if dn := strings.TrimSpace(req.DisplayName); dn != "" {
		displayName = pgtype.Text{String: dn, Valid: true}
	}

	user, err := h.createUserFirstAdmin(r, email, hash, displayName)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Duplicate email. Enumeration is an accepted trade-off for this
			// internal MVP (see PRD).
			httpx.Error(w, http.StatusConflict, "an account with this email already exists")
			return
		}
		slog.Error("create user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Create + unlock the user's vault with the registration password (PRD #32).
	// Deliberately AFTER the transaction commits: registration holds a
	// pg_advisory_xact_lock, and the vault's Argon2 KEK derivation inside that lock
	// would serialize all registrations. The vault row is created in its own
	// statement here; a crash between user-create and vault-create is self-healing
	// (the next login's first-unlock creates it). A failure is non-fatal, same as
	// login — the account exists and the SPA will show the unlock banner.
	if h.vault != nil {
		if err := h.vault.Unlock(r.Context(), user.ID, req.Password); err != nil {
			slog.Error("vault unlock at register", "user", user.ID, "error", err)
		}
	}

	if !h.issueSession(w, user) {
		return
	}
	httpx.JSON(w, http.StatusCreated, h.sessionPayload(r.Context(), user))
}

// createUserFirstAdmin performs the advisory-locked first-user-admin
// check-and-insert in a single transaction.
func (h *Handler) createUserFirstAdmin(r *http.Request, email, hash string, displayName pgtype.Text) (store.User, error) {
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	qtx := h.q.WithTx(tx)

	// Serialize registrations: the lock is held until the transaction ends,
	// so the count-then-insert below is atomic across concurrent callers.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.RegistrationLockKey); err != nil {
		return store.User{}, err
	}

	count, err := qtx.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}

	user, err := qtx.CreateUser(ctx, store.CreateUserParams{
		Email:        email,
		PasswordHash: hash,
		DisplayName:  displayName,
		IsAdmin:      count == 0,
	})
	if err != nil {
		return store.User{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return store.User{}, err
	}
	return user, nil
}

// Login verifies credentials and starts a session. It returns a generic error
// for both unknown email and wrong password, and equalizes timing by hashing
// against a dummy hash when the email is unknown.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))

	user, err := h.q.GetUserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Burn ONE Argon2 so an unknown email is timing-indistinguishable from a
			// known email with a WRONG password: both fail here having done exactly one
			// VerifyPassword. The vault KEK derivation (a second Argon2) runs only AFTER
			// a correct password, on the success path — which a 200 + Set-Cookie already
			// reveals, so it needs no counterweight here. A second dummy hash on this
			// path would itself leak email existence (unknown=2 vs known-wrong=1), the
			// exact oracle this burn exists to close.
			_, _ = auth.VerifyPassword(req.Password, dummyHash)
			httpx.Error(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		slog.Error("get user by email", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	ok, err := auth.VerifyPassword(req.Password, user.PasswordHash)
	if err != nil {
		slog.Error("verify password", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if !user.IsActive {
		httpx.Error(w, http.StatusForbidden, "account is deactivated")
		return
	}

	if err := h.q.SetLastLogin(r.Context(), user.ID); err != nil {
		slog.Warn("set last login", "error", err)
	}

	// Unlock the vault with the just-verified password so the user's agents can
	// work this session — including overnight, since the DEK lives in the API, not
	// the browser (PRD #32). First-ever unlock creates the vault. A failure here
	// must NOT block login (the session is valid); it can only be a corrupted row,
	// so log loudly and return the session with the vault left locked (the SPA then
	// shows the unlock banner). The error never carries the password or a secret.
	if h.vault != nil {
		if err := h.vault.Unlock(r.Context(), user.ID, req.Password); err != nil {
			slog.Error("vault unlock at login", "user", user.ID, "error", err)
		}
	}

	if !h.issueSession(w, user) {
		return
	}
	httpx.JSON(w, http.StatusOK, h.sessionPayload(r.Context(), user))
}

// Logout bumps the user's token_version (revoking every issued token) and
// clears the cookies.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if _, err := h.q.BumpTokenVersion(r.Context(), user.ID); err != nil {
		slog.Error("bump token version", "error", err)
	}
	auth.ClearAuthCookies(w, h.cfg.CookieSecure)
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// Me returns the authenticated user.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	httpx.JSON(w, http.StatusOK, h.sessionPayload(r.Context(), user))
}

// sessionPayload builds the auth/session bootstrap body the SPA reads on
// login, register, and the initial me() probe: the user plus the instance
// forge labels the board and issue-creation UI need before their first API call
// (PRD #19 M2 — delivered on the existing response, no new endpoint). Labels are
// best-effort: a cold settings read yields the compiled-in defaults, never an
// error, so a session response is never blocked on settings.
func (h *Handler) sessionPayload(ctx context.Context, user store.User) map[string]any {
	prdLabel, _ := h.settings.PRDLabel(ctx)
	autopilotLabel, _ := h.settings.AutopilotLabel(ctx)
	// Theme resolution (PRD #21 Decision 2): the SPA needs three values, not just
	// the resolved theme. With an override active, the Appearance picker also has
	// to render "Use default (<name>)" and set its selected state, and the default
	// lives in an admin-only endpoint — so carry the resolved theme, the user's
	// raw override (nullable), and the instance default here. Best-effort like the
	// labels: a cold settings read yields the registry default, never an error.
	defaultTheme, _ := h.settings.DefaultTheme(ctx)
	override := ""
	if user.Theme.Valid {
		override = user.Theme.String
	}
	// PRDLESS bootstrap (PRD #22 M3): the SPA gates the label toggle and the
	// PRDLESS badge on prdless_enabled and needs the label name. prdless_enabled is
	// this payload's first bool field (the labels are strings); the typed accessor
	// backs it. Best-effort like the rest: a cold settings read yields the
	// compiled-in defaults (enabled, "PRDLESS"), never an error.
	prdlessLabel, _ := h.settings.PrdlessLabel(ctx)
	prdlessEnabled, _ := h.settings.PrdlessEnabled(ctx)
	return map[string]any{
		"user":            toDTO(user),
		"prd_label":       prdLabel,
		"autopilot_label": autopilotLabel,
		"theme":           theme.Resolve(override, defaultTheme),
		"theme_override":  textPtrValue(override != "", override),
		"default_theme":   defaultTheme,
		"prdless_label":   prdlessLabel,
		"prdless_enabled": prdlessEnabled,
		// Vault status (PRD #32): the SPA shows a 🔒 badge + unlock banner and marks
		// own queued runs "waiting for vault unlock" when locked. Delivered on the
		// session payload so the shell needs no extra round-trip; the SPA refreshes
		// it via AuthContext (window focus, after unlock/lock, on any 409 vault_locked).
		"vault": map[string]any{"unlocked": h.vaultUnlocked(user.ID)},
	}
}

// vaultUnlocked reports whether the user's vault DEK is cached. A nil vault (only
// in tests; main always wires one) means "no gate", reported as unlocked so the
// SPA shows no banner and legacy behavior is preserved.
func (h *Handler) vaultUnlocked(userID uuid.UUID) bool {
	return h.vault == nil || h.vault.Unlocked(userID)
}

// AuthConfig returns the operator-set registration policy the register page
// needs to hide itself or hint the allowed domains before submit. It is uzi's
// first unauthenticated JSON surface besides /health, so it is a security
// boundary: it must expose ONLY operator-set, user-visible policy — never user
// data, secrets, or anything derived from a request — and it sits outside
// RequireAuth, behind the auth rate limiter like register/login.
func (h *Handler) AuthConfig(w http.ResponseWriter, r *http.Request) {
	// Always emit a JSON array (never null) so the SPA can index it directly.
	domains := h.cfg.AllowedEmailDomains
	if domains == nil {
		domains = []string{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"registration_enabled":  h.cfg.RegistrationEnabled,
		"allowed_email_domains": domains,
	})
}

// issueSession mints a JWT at the user's current token_version and sets the
// auth + CSRF cookies. It writes an error response and returns false on
// failure.
func (h *Handler) issueSession(w http.ResponseWriter, user store.User) bool {
	token, err := auth.IssueToken(h.cfg.JWTSecret, user.ID.String(), user.TokenVersion, h.cfg.AuthTokenTTL)
	if err != nil {
		slog.Error("issue token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if err := auth.SetAuthCookies(w, token, auth.CookieOptions{Secure: h.cfg.CookieSecure, TTL: h.cfg.AuthTokenTTL}); err != nil {
		slog.Error("set auth cookies", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}
