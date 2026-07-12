package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/oidc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// OIDC state-cookie parameters (PRD #45, Decision 3). The cookie is HttpOnly,
// Secure per CookieSecure, and SameSite=Lax — the callback is a top-level
// cross-site GET redirect from the IdP, and Strict cookies are dropped on such a
// navigation. It is scoped to the OIDC path so it never rides along on other
// requests, and it is short-lived (10 min covers the IdP login + any MFA prompt).
const (
	oidcStateCookieName = "uzi_oidc_state"
	oidcStateCookiePath = "/api/auth/oidc"
	oidcStateTTL        = 10 * time.Minute
	// oidcMaxDisplayNameLen caps the IdP-provided display name stored at JIT
	// (Decision 10); users edit it themselves afterwards, so this only bounds an
	// oversized claim.
	oidcMaxDisplayNameLen = 100
)

// Enumerated callback error codes. The SPA switches on these known values and
// never renders the raw string; the detail goes to the server log. Redirects use
// /login?error=<code>.
const (
	oidcErrState       = "oidc_state"       // missing/undecryptable cookie or state mismatch
	oidcErrExchange    = "oidc_exchange"    // discovery/token-exchange/verify failure (IdP down or misconfigured)
	oidcErrForbidden   = "oidc_forbidden"   // unverified email, domain rejected, registration off, email bound to another subject, IdP denial
	oidcErrDeactivated = "oidc_deactivated" // matched account is deactivated
	oidcErrInternal    = "oidc_error"       // server-side error (DB, cookie seal)
)

// oidcStateData is sealed into the state cookie: the CSRF state, the ID-token
// nonce, and the PKCE verifier. None is a long-lived secret, but sealing with the
// master box keeps them opaque + tamper-evident and lets the callback treat any
// decrypt failure as a hard reject.
type oidcStateData struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
}

// OIDCLogin starts the Authorization Code + PKCE (S256) flow: it mints
// state/nonce/verifier, seals them into the state cookie, and redirects to the
// IdP. Discovery is lazy, so an IdP that was down at boot is retried here; a
// failure bounces back to the login page with an error code rather than 500ing.
func (h *Handler) OIDCLogin(w http.ResponseWriter, r *http.Request) {
	if h.oidc == nil {
		// OIDC not configured. The SSO button is hidden in this case, so this is only
		// reachable by a hand-crafted request; treat it as a benign non-feature.
		h.redirectOIDCError(w, r, oidcErrExchange)
		return
	}

	state, err1 := randToken()
	nonce, err2 := randToken()
	if err1 != nil || err2 != nil {
		slog.Error("oidc login: generate state/nonce", "state_err", err1, "nonce_err", err2)
		h.redirectOIDCError(w, r, oidcErrInternal)
		return
	}
	verifier := oidc.GenerateVerifier()

	authURL, err := h.oidc.AuthCodeURL(r.Context(), state, nonce, verifier)
	if err != nil {
		// Discovery degraded (IdP unreachable/misconfigured at both boot and now).
		slog.Error("oidc login: build auth url", "error", err)
		h.redirectOIDCError(w, r, oidcErrExchange)
		return
	}
	if err := h.setOIDCStateCookie(w, oidcStateData{State: state, Nonce: nonce, Verifier: verifier}); err != nil {
		slog.Error("oidc login: seal state cookie", "error", err)
		h.redirectOIDCError(w, r, oidcErrInternal)
		return
	}
	http.Redirect(w, r, authURL, http.StatusFound)
}

// OIDCCallback completes the flow. Order is load-bearing (audit M1): the state
// cookie is validated BEFORE any token-endpoint call, so a cookieless / mismatched
// hit is a hard reject that never drives a uzi→IdP exchange. The cookie is deleted
// on every outcome.
func (h *Handler) OIDCCallback(w http.ResponseWriter, r *http.Request) {
	// Always clear the one-shot state cookie, whatever happens next.
	h.clearOIDCStateCookie(w)

	if h.oidc == nil {
		h.redirectOIDCError(w, r, oidcErrExchange)
		return
	}

	// 1. The state cookie must be present and decrypt — a missing cookie is a hard
	//    reject, never a skip (and exchanging first would let cookieless hits drive
	//    uzi→IdP amplification).
	data, ok := h.readOIDCStateCookie(r)
	if !ok {
		h.redirectOIDCError(w, r, oidcErrState)
		return
	}
	// 2. The query state must match the sealed state (constant-time).
	q := r.URL.Query()
	if st := q.Get("state"); subtle.ConstantTimeCompare([]byte(st), []byte(data.State)) != 1 {
		h.redirectOIDCError(w, r, oidcErrState)
		return
	}
	// 3. An IdP-side denial (e.g. access_denied) comes back with an error param and
	//    no code. Reject without an exchange.
	if e := q.Get("error"); e != "" {
		slog.Warn("oidc callback: idp returned error", "idp_error", e)
		h.redirectOIDCError(w, r, oidcErrForbidden)
		return
	}
	code := q.Get("code")
	if code == "" {
		h.redirectOIDCError(w, r, oidcErrState)
		return
	}

	// 4. Only now: exchange the code, verify the ID token (issuer/aud/sig), and check
	//    the nonce against the sealed value.
	identity, err := h.oidc.Exchange(r.Context(), code, data.Verifier, data.Nonce)
	if err != nil {
		slog.Error("oidc callback: code exchange / verify", "error", err)
		h.redirectOIDCError(w, r, oidcErrExchange)
		return
	}

	user, errCode := h.oidcResolveUser(r, identity)
	if errCode != "" {
		h.redirectOIDCError(w, r, errCode)
		return
	}

	// Converge on the existing session chokepoint (Decision 2): same JWT + CSRF
	// cookies, same token_version. The OIDC callback never touches the vault
	// (Decision 6 / audit M4) — a passwordless user creates a vault passphrase
	// later; a linked user's password-derived vault stays locked here.
	if !h.issueSession(w, user) {
		return // issueSession already wrote an error response
	}
	// Fixed server-side relative path — no next/return_to param exists (Decision 3,
	// closes open-redirect).
	http.Redirect(w, r, "/", http.StatusFound)
}

// oidcResolveUser applies the Decision 5 login order and returns either the user
// to log in or an enumerated error code. The subject match runs first and needs no
// email; linking and JIT both require a verified, canonicalized email.
func (h *Handler) oidcResolveUser(r *http.Request, identity oidc.Identity) (store.User, string) {
	ctx := r.Context()

	// 1. Stable (issuer, subject) identity → log in.
	user, err := h.q.GetUserByOIDCSubject(ctx, store.GetUserByOIDCSubjectParams{
		OidcIssuer:  pgtype.Text{String: identity.Issuer, Valid: true},
		OidcSubject: pgtype.Text{String: identity.Subject, Valid: true},
	})
	if err == nil {
		return h.oidcLoginExisting(ctx, user)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("oidc resolve: get by subject", "error", err)
		return store.User{}, oidcErrInternal
	}

	// Linking and JIT both require a verified email (Decision 5): an unverified
	// email is an account-takeover vector. email_verified must be boolean true.
	if !identity.EmailVerified {
		slog.Warn("oidc resolve: email not verified, refusing link/JIT")
		return store.User{}, oidcErrForbidden
	}
	// Canonicalize exactly like Register so link matching and UNIQUE never diverge
	// from password-registered emails (review N4).
	addr, err := mail.ParseAddress(strings.TrimSpace(strings.ToLower(identity.Email)))
	if err != nil {
		slog.Warn("oidc resolve: missing/unparseable email claim")
		return store.User{}, oidcErrForbidden
	}
	email := addr.Address

	// 2. Verified-email match → link, but only if the row is not already bound to a
	//    subject (audit H1). A row bound to a DIFFERENT subject is rejected + logged,
	//    never overwritten.
	existing, err := h.q.GetUserByEmail(ctx, email)
	if err == nil {
		if existing.OidcSubject.Valid {
			slog.Warn("oidc resolve: email matches an account bound to a different subject; refusing", "user", existing.ID)
			return store.User{}, oidcErrForbidden
		}
		linked, err := h.q.LinkUserOIDC(ctx, store.LinkUserOIDCParams{
			ID:          existing.ID,
			OidcIssuer:  pgtype.Text{String: identity.Issuer, Valid: true},
			OidcSubject: pgtype.Text{String: identity.Subject, Valid: true},
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// A concurrent login bound this row between our read and update. The
				// WHERE oidc_subject IS NULL guard made the update a no-op; reject rather
				// than race a takeover.
				slog.Warn("oidc resolve: link raced with a concurrent bind; refusing", "user", existing.ID)
				return store.User{}, oidcErrForbidden
			}
			slog.Error("oidc resolve: link", "error", err)
			return store.User{}, oidcErrInternal
		}
		return h.oidcLoginExisting(ctx, linked)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("oidc resolve: get by email", "error", err)
		return store.User{}, oidcErrInternal
	}

	// 3. JIT-provision, gated on registration + the domain allowlist. The first-ever
	//    user becomes admin under the same advisory lock as password registration.
	if !h.cfg.RegistrationEnabled {
		slog.Warn("oidc resolve: JIT blocked, registration disabled")
		return store.User{}, oidcErrForbidden
	}
	if !emailDomainAllowed(email, h.cfg.AllowedEmailDomains) {
		slog.Warn("oidc resolve: JIT blocked, domain not allowed")
		return store.User{}, oidcErrForbidden
	}

	created, err := h.createOIDCUserFirstAdmin(r, email, oidcDisplayName(identity.Name), identity.Issuer, identity.Subject)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Concurrent first-login race on the email or (issuer,subject) unique
			// constraint: "already exists → re-fetch and log in" (audit L6).
			return h.oidcRefetchAfterRace(ctx, identity, email)
		}
		slog.Error("oidc resolve: create user", "error", err)
		return store.User{}, oidcErrInternal
	}
	return h.oidcLoginExisting(ctx, created)
}

// oidcLoginExisting applies the deactivated-account check (issueSession doesn't do
// it — review N5) and records the login. It never unlocks the vault (Decision 6).
func (h *Handler) oidcLoginExisting(ctx context.Context, user store.User) (store.User, string) {
	if !user.IsActive {
		slog.Warn("oidc login: account deactivated", "user", user.ID)
		return store.User{}, oidcErrDeactivated
	}
	if err := h.q.SetLastLogin(ctx, user.ID); err != nil {
		slog.Warn("oidc login: set last login", "error", err)
	}
	return user, ""
}

// createOIDCUserFirstAdmin is the passwordless sibling of createUserFirstAdmin
// (review B2): the same advisory-locked first-user-admin check-and-insert, but via
// CreateUserOIDC (NULL password_hash) with the IdP identity attached.
func (h *Handler) createOIDCUserFirstAdmin(r *http.Request, email string, displayName pgtype.Text, issuer, subject string) (store.User, error) {
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return store.User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit

	qtx := h.q.WithTx(tx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.RegistrationLockKey); err != nil {
		return store.User{}, err
	}
	count, err := qtx.CountUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	user, err := qtx.CreateUserOIDC(ctx, store.CreateUserOIDCParams{
		Email:       email,
		DisplayName: displayName,
		IsAdmin:     count == 0,
		OidcIssuer:  pgtype.Text{String: issuer, Valid: true},
		OidcSubject: pgtype.Text{String: subject, Valid: true},
	})
	if err != nil {
		return store.User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.User{}, err
	}
	return user, nil
}

// oidcRefetchAfterRace recovers from a JIT 23505: the winning creator's row is
// re-read by (issuer, subject) and logged in. A 23505 that was actually an email
// collision with a different subject is treated like the "email bound to a
// different subject" case — rejected, never hijacked.
func (h *Handler) oidcRefetchAfterRace(ctx context.Context, identity oidc.Identity, email string) (store.User, string) {
	user, err := h.q.GetUserByOIDCSubject(ctx, store.GetUserByOIDCSubjectParams{
		OidcIssuer:  pgtype.Text{String: identity.Issuer, Valid: true},
		OidcSubject: pgtype.Text{String: identity.Subject, Valid: true},
	})
	if err == nil {
		return h.oidcLoginExisting(ctx, user)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("oidc resolve: refetch by subject after race", "error", err)
		return store.User{}, oidcErrInternal
	}
	slog.Warn("oidc login: create raced with a different account on this email; refusing", "email", email)
	return store.User{}, oidcErrForbidden
}

// oidcDisplayName trims and rune-caps an IdP-provided name for JIT storage.
func oidcDisplayName(name string) pgtype.Text {
	dn := strings.TrimSpace(name)
	if dn == "" {
		return pgtype.Text{}
	}
	if r := []rune(dn); len(r) > oidcMaxDisplayNameLen {
		dn = string(r[:oidcMaxDisplayNameLen])
	}
	return pgtype.Text{String: dn, Valid: true}
}

// --- state cookie seal/open ------------------------------------------------

func (h *Handler) setOIDCStateCookie(w http.ResponseWriter, data oidcStateData) error {
	plain, err := json.Marshal(data)
	if err != nil {
		return err
	}
	sealed, err := h.box.Seal(plain)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    base64.RawURLEncoding.EncodeToString(sealed),
		Path:     oidcStateCookiePath,
		MaxAge:   int(oidcStateTTL.Seconds()),
		Expires:  time.Now().Add(oidcStateTTL),
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (h *Handler) clearOIDCStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    "",
		Path:     oidcStateCookiePath,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// readOIDCStateCookie returns the sealed state, or ok=false when the cookie is
// absent, malformed, won't decrypt, or is missing a field. Every false path is a
// generic reject — the caller never distinguishes them to the client.
func (h *Handler) readOIDCStateCookie(r *http.Request) (oidcStateData, bool) {
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil || c.Value == "" {
		return oidcStateData{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return oidcStateData{}, false
	}
	plain, err := h.box.Open(sealed)
	if err != nil {
		return oidcStateData{}, false
	}
	var data oidcStateData
	if err := json.Unmarshal(plain, &data); err != nil {
		return oidcStateData{}, false
	}
	if data.State == "" || data.Nonce == "" || data.Verifier == "" {
		return oidcStateData{}, false
	}
	return data, true
}

// redirectOIDCError sends the browser back to the SPA login page with an
// enumerated error code (never a raw message). The code is a compiled-in constant,
// so QueryEscape here is belt-and-suspenders.
func (h *Handler) redirectOIDCError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+url.QueryEscape(code), http.StatusFound)
}

// randToken returns a 32-byte base64url random string for state/nonce.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
