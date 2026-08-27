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

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/theme"
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
	// Password login off (SSO-only) disables registration too: a password account
	// minted here could never log in (PRD #45, Decision 8 / audit M3). SSO users are
	// provisioned via JIT or linking, never this endpoint.
	if !h.cfg.PasswordLoginEnabled {
		httpx.Error(w, http.StatusForbidden, "password login is disabled")
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
		PasswordHash: pgtype.Text{String: hash, Valid: true},
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
	// SSO-only mode: with password login disabled, refuse before touching the body
	// or the DB (PRD #45, Decision 8 / fact-check R1). A uniform 403 regardless of
	// whether the account exists — no enumeration surface, and no Argon2 on this
	// path. This is the whole point of the kill-switch: an SSO-only shop must not
	// leave a password backdoor that bypasses the IdP's offboarding. Login is the
	// only VerifyPassword caller; worker join tokens and the JWT-cookie paths are
	// separate. Break-glass stays as documented: flip UZI_PASSWORD_LOGIN_ENABLED back
	// to true and restart (the seed admin keeps its password_hash).
	if !h.cfg.PasswordLoginEnabled {
		httpx.Error(w, http.StatusForbidden, "password login is disabled")
		return
	}

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

	// OIDC-only accounts have no password_hash (PRD #45, Decision 7). Branch on that
	// FIRST and treat it exactly like a wrong password: burn one Argon2 against the
	// dummy hash to hold timing, then return the same generic 401. Never let a
	// NULL/invalid hash reach VerifyPassword and surface its ErrInvalidHash as a 500 —
	// that 500-vs-401 difference is an oracle distinguishing OIDC-only accounts from
	// wrong passwords (audit H2).
	if !user.PasswordHash.Valid {
		_, _ = auth.VerifyPassword(req.Password, dummyHash)
		httpx.Error(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	ok, err := auth.VerifyPassword(req.Password, user.PasswordHash.String)
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
//
// The PRD #196 eligible set and PRD-link waiver ride this payload (not just the
// board payload) because the issue view is the board's second consumer of the
// eligibility predicate and has no board payload — it reads these from useAuth().
// They also feed the card's Start/Promote eligibility affordance, which has no
// board payload of its own to carry them.
func (h *Handler) sessionPayload(ctx context.Context, user store.User) map[string]any {
	prdLabel, _ := h.settings.PRDLabel(ctx)
	autopilotLabel, _ := h.settings.AutopilotLabel(ctx)
	runEligible, _ := h.settings.RunEligibleLabels(ctx)
	waiver, _ := h.settings.EligibleLabelWaivesPRDLink(ctx)
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
	// Judge consent surface (PRD #69 M4): a non-admin cannot read
	// /api/admin/settings, so the two facts the user needs to consent to their own
	// token being spent ride the session payload. Both are resolved server-side and
	// best-effort — a cold/failed settings read degrades to the SAFE reading (not
	// enforced; the instance/default model), never a 500 on /me.
	//
	// judge_enforced_by_admin mirrors the Gate-2-wins semantics of the enqueue path
	// (workersvc): the kill-switch (judge_enabled) dominates enforce_all, so the
	// judge is only "enforced" when BOTH are on. With the kill-switch off, an
	// enforce_all=true row is inert and reported as not enforced.
	judgeEnabled, _ := h.settings.JudgeEnabled(ctx)
	judgeEnforceAll, _ := h.settings.JudgeEnforceAll(ctx)
	judgeEnforced := judgeEnabled && judgeEnforceAll
	// effective_judge_model is the model THIS user's judge actually runs on, resolved
	// exactly the way assembleJudgeClaim resolves it (user-value-wins): the user's own
	// non-blank judge_model, else the instance judge_model, which itself falls back to
	// DefaultJudgeModel ("opus") inside JudgeModel. So the consent copy names the real
	// model, not a guess.
	effectiveJudgeModel := ""
	if user.JudgeModel.Valid && strings.TrimSpace(user.JudgeModel.String) != "" {
		effectiveJudgeModel = user.JudgeModel.String
	} else if m, err := h.settings.JudgeModel(ctx); err == nil && strings.TrimSpace(m) != "" {
		effectiveJudgeModel = m
	} else {
		effectiveJudgeModel = settings.DefaultJudgeModel
	}
	return map[string]any{
		"user":            toDTO(user),
		"prd_label":       prdLabel,
		"autopilot_label": autopilotLabel,
		"theme":           theme.Resolve(override, defaultTheme),
		"theme_override":  textPtrValue(override != "", override),
		"default_theme":   defaultTheme,
		"prdless_label":   prdlessLabel,
		"prdless_enabled": prdlessEnabled,
		// PRD #196: the admin-configured run-eligible label set and the PRD-link
		// waiver bool. The SPA renders the card/issue-view Start affordance from the
		// eligible set and the waiver.
		"run_eligible_labels":            runEligible,
		"eligible_label_waives_prd_link": waiver,
		// Vault status (PRD #32): the SPA shows a 🔒 badge + unlock banner and marks
		// own queued runs "waiting for vault unlock" when locked. Delivered on the
		// session payload so the shell needs no extra round-trip; the SPA refreshes
		// it via AuthContext (window focus, after unlock/lock, on any 409 vault_locked).
		// `exists` (PRD #45, review N1) lets a passwordless user's SPA pick the
		// create-passphrase dialog (exists=false) vs the unlock banner (exists=true)
		// deterministically, without probing for a 409.
		"vault": map[string]any{
			"unlocked": h.vaultUnlocked(user.ID),
			"exists":   h.vaultExists(ctx, user.ID),
		},
		// has_password is false for OIDC-only users (NULL password_hash); the SPA uses
		// it with vault.exists to choose passphrase-create vs unlock wording (PRD #45).
		"has_password": user.PasswordHash.Valid,
		// Judge consent (PRD #69 M4). judge_enforced_by_admin drives the RunDefaults
		// ENFORCED banner; effective_judge_model names the model the user's own token
		// will run on. The user's RAW per-user judge_model override is read/written
		// through PUT /me/settings (userSettingsDTO), so it is not duplicated here.
		"judge_enforced_by_admin": judgeEnforced,
		"effective_judge_model":   effectiveJudgeModel,
	}
}

// vaultUnlocked reports whether the user's vault DEK is cached. A nil vault (only
// in tests; main always wires one) means "no gate", reported as unlocked so the
// SPA shows no banner and legacy behavior is preserved.
func (h *Handler) vaultUnlocked(userID uuid.UUID) bool {
	return h.vault == nil || h.vault.Unlocked(userID)
}

// vaultExists reports whether the user has a vault row, for the session payload's
// `exists` bit (PRD #45). A nil vault (tests) reports true ("no gate", consistent
// with vaultUnlocked) so the SPA offers no create dialog. A DB error is treated as
// "exists" (conservative: never invite a passphrase-create over a vault we could
// not confirm is absent — the user can retry, and /api/vault/passphrase is itself
// create-only as the real backstop).
func (h *Handler) vaultExists(ctx context.Context, userID uuid.UUID) bool {
	if h.vault == nil {
		return true
	}
	exists, err := h.vault.Exists(ctx, userID)
	if err != nil {
		slog.Error("vault exists for session payload", "user", userID, "error", err)
		return true
	}
	return exists
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
		// OIDC SSO surface (PRD #45, Decision 9). oidc_enabled is gated on the feature
		// being CONFIGURED, not on discovery having succeeded, so the SSO button stays
		// visible (and the lazy discovery-retry reachable) even if the IdP was down at
		// boot. password_login_enabled lets the SPA hide the password form for SSO-only
		// deployments.
		"oidc_enabled":           h.cfg.OIDCEnabled(),
		"oidc_provider_name":     h.cfg.OIDCProviderName,
		"password_login_enabled": h.cfg.PasswordLoginEnabled,
	})
}

// issueSessionCookies mints the JWT and sets the auth + CSRF cookies. It logs and
// returns an error WITHOUT writing a response body, so callers choose how to
// surface failure: a JSON 500 for the password endpoints (issueSession), a
// redirect for the OIDC callback (review NB3).
func (h *Handler) issueSessionCookies(w http.ResponseWriter, user store.User) error {
	token, err := auth.IssueToken(h.cfg.JWTSecret, user.ID.String(), user.TokenVersion, h.cfg.AuthTokenTTL)
	if err != nil {
		slog.Error("issue token", "error", err)
		return err
	}
	if err := auth.SetAuthCookies(w, token, auth.CookieOptions{Secure: h.cfg.CookieSecure, TTL: h.cfg.AuthTokenTTL}); err != nil {
		slog.Error("set auth cookies", "error", err)
		return err
	}
	return nil
}

// issueSession mints a JWT at the user's current token_version and sets the
// auth + CSRF cookies. It writes an error response and returns false on
// failure.
func (h *Handler) issueSession(w http.ResponseWriter, user store.User) bool {
	if err := h.issueSessionCookies(w, user); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}
