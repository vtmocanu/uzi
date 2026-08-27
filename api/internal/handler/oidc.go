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

	"github.com/vtmocanu/uzi/api/internal/oidc"
	"github.com/vtmocanu/uzi/api/internal/store"
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
// nonce, the PKCE verifier, and the issue time. None is a long-lived secret, but
// sealing with the master box keeps them opaque + tamper-evident and lets the
// callback treat any decrypt failure as a hard reject. IssuedAt is enforced
// server-side against oidcStateTTL so a replayed cookie cannot outlive the window
// even if the browser ignores the cookie MaxAge (audit Low1).
type oidcStateData struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	IssuedAt int64  `json:"t"` // unix seconds
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
	if err := h.setOIDCStateCookie(w, oidcStateData{State: state, Nonce: nonce, Verifier: verifier, IssuedAt: time.Now().Unix()}); err != nil {
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
		// Cookie + state were valid but the IdP returned neither a code nor an error:
		// a protocol failure, not a state failure (Nit A).
		h.redirectOIDCError(w, r, oidcErrExchange)
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
	// later; a linked user's password-derived vault stays locked here. On the rare
	// mint failure, redirect with an error code rather than emit a raw JSON 500 into
	// a top-level browser navigation (review NB3).
	if err := h.issueSessionCookies(w, user); err != nil {
		h.redirectOIDCError(w, r, oidcErrInternal)
		return
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

	// 0. Allowlist gate (PRD #55 Decision 4.1) — FIRST, before any DB read or write, so
	//    a rejected login never links a row, touches a deactivated row, or JIT-creates.
	//    Fail-safe (Decision 1): reject ONLY when the gate is configured AND the claim
	//    is present AND there is no intersection. An absent/unparseable claim lets an
	//    EXISTING user pass here (they fail safe into their stored role); a brand-new
	//    JIT user is still refused by the JIT-branch guard below. The reject reuses the
	//    generic oidc_forbidden with detail in the server log only (no enumeration).
	if len(h.cfg.OIDCAllowedGroups) > 0 && identity.GroupsClaimPresent &&
		!groupsIntersect(h.cfg.OIDCAllowedGroups, identity.Groups) {
		slog.Warn("oidc resolve: allowlist gate rejected login (no allowed-group membership)", "subject", identity.Subject)
		return store.User{}, oidcErrForbidden
	}

	// Loud misconfig signal (PRD #55 Decision 1): group mapping is configured but the
	// verified ID token carried no usable groups claim (mapper toggled off, renamed,
	// or wrong shape). The login still proceeds under fail-safe semantics — existing
	// users keep their stored role and pass the gate, a gated JIT user is refused
	// below — but this warns per login so the operator notices until the claim is
	// fixed. Only the configured claim NAME is logged (there are no claim contents).
	if (len(h.cfg.OIDCAllowedGroups) > 0 || len(h.cfg.OIDCAdminGroups) > 0) && !identity.GroupsClaimPresent {
		slog.Warn("oidc resolve: groups claim absent/unparseable while group mapping is configured; applying fail-safe (existing users keep role + pass gate; a gated new user is refused)",
			"subject", identity.Subject, "groups_claim", h.cfg.OIDCGroupsClaim)
	}

	// 1. Stable (issuer, subject) identity → log in.
	user, err := h.q.GetUserByOIDCSubject(ctx, store.GetUserByOIDCSubjectParams{
		OidcIssuer:  pgtype.Text{String: identity.Issuer, Valid: true},
		OidcSubject: pgtype.Text{String: identity.Subject, Valid: true},
	})
	if err == nil {
		// Decision 10: warn on IdP email drift, never auto-apply (email is UNIQUE in
		// uzi; a blind rename can collide). No raw addresses in the log (PII rule) —
		// user id + subject is enough to correlate.
		if addr, e := mail.ParseAddress(strings.TrimSpace(strings.ToLower(identity.Email))); e == nil && addr.Address != user.Email {
			slog.Warn("oidc login: idp email drift detected (not auto-applied)", "user", user.ID, "subject", identity.Subject)
		}
		return h.oidcLoginExisting(ctx, user, identity)
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
		// Reject a deactivated account BEFORE mutating it (review NB2): the deactivated
		// check is already in hand from GetUserByEmail, so an unauthenticated callback
		// hit never links (writes to) a disabled row.
		if !existing.IsActive {
			slog.Warn("oidc resolve: link target is deactivated; refusing", "user", existing.ID)
			return store.User{}, oidcErrDeactivated
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
		return h.oidcLoginExisting(ctx, linked, identity)
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
	// JIT allowlist guard (PRD #55 Decision 1): a brand-new user has no established
	// role to fail safe into, so provisioning is refused unless the gate is open —
	// either no allowlist, or the claim is present AND intersects. An absent/
	// unparseable claim with the gate set refuses here (unlike existing users, who
	// pass the top gate). The top gate already rejected present-and-no-intersection;
	// this closes the absent-claim case for new users.
	if len(h.cfg.OIDCAllowedGroups) > 0 &&
		(!identity.GroupsClaimPresent || !groupsIntersect(h.cfg.OIDCAllowedGroups, identity.Groups)) {
		slog.Warn("oidc resolve: JIT blocked, not in an allowed group (or groups claim absent/unparseable)", "subject", identity.Subject)
		return store.User{}, oidcErrForbidden
	}

	// Admin-at-creation (PRD #55 Decision 4/5): when admin groups are configured the
	// group decides (first-OIDC-user-admin is disabled), and an absent claim yields a
	// non-admin (GroupsClaimPresent gates membership). When not configured, the
	// count==0 first-admin rule applies, computed under the advisory lock.
	useGroupAdmin := len(h.cfg.OIDCAdminGroups) > 0
	groupAdmin := useGroupAdmin && identity.GroupsClaimPresent && groupsIntersect(h.cfg.OIDCAdminGroups, identity.Groups)
	created, err := h.createOIDCUserFirstAdmin(r, email, oidcDisplayName(identity.Name), identity.Issuer, identity.Subject, useGroupAdmin, groupAdmin)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Concurrent first-login race on the email or (issuer,subject) unique
			// constraint: "already exists → re-fetch and log in" (audit L6).
			return h.oidcRefetchAfterRace(ctx, identity)
		}
		slog.Error("oidc resolve: create user", "error", err)
		return store.User{}, oidcErrInternal
	}
	// A freshly JIT-created row already has its is_admin set at creation, so the admin
	// sync inside oidcLoginExisting is a guaranteed no-op here (desired == stored):
	// Decision 5 "don't ALSO sync-flip a JIT user" holds by construction, and routing
	// through the same funnel keeps the active-check + SetLastLogin in one place.
	return h.oidcLoginExisting(ctx, created, identity)
}

// oidcLoginExisting applies the deactivated-account check (issueSession doesn't do
// it — review N5), syncs is_admin from group membership (PRD #55), and records the
// login. It never unlocks the vault (Decision 6). Both existing-user paths (subject
// match + email link) and the JIT/refetch paths funnel through here, so the admin
// sync runs exactly once per login regardless of how the user was resolved.
func (h *Handler) oidcLoginExisting(ctx context.Context, user store.User, identity oidc.Identity) (store.User, string) {
	if !user.IsActive {
		slog.Warn("oidc login: account deactivated", "user", user.ID)
		return store.User{}, oidcErrDeactivated
	}
	synced, errCode := h.oidcSyncAdmin(ctx, user, identity)
	if errCode != "" {
		return store.User{}, errCode
	}
	user = synced
	if err := h.q.SetLastLogin(ctx, user.ID); err != nil {
		slog.Warn("oidc login: set last login", "error", err)
	}
	return user, ""
}

// oidcSyncAdmin authoritatively syncs is_admin from OIDC group membership (PRD #55
// Decision 4.3). It runs ONLY when admin groups are configured AND the groups claim
// is present — an absent/unparseable claim keeps the stored role (fail-safe,
// Decision 1). Membership grants; loss of membership demotes. The seed admin
// (cfg.SeedEmail) is exempt from DEMOTION only (break-glass), guarded against an
// empty SeedEmail so disabled seeding never becomes a blanket exemption. On a flip
// the returned row is refreshed so the issued session reflects it. Logs carry the
// user id + direction + configured group names only (never the claimed group list).
func (h *Handler) oidcSyncAdmin(ctx context.Context, user store.User, identity oidc.Identity) (store.User, string) {
	if len(h.cfg.OIDCAdminGroups) == 0 || !identity.GroupsClaimPresent {
		return user, ""
	}
	desired := groupsIntersect(h.cfg.OIDCAdminGroups, identity.Groups)
	if desired == user.IsAdmin {
		return user, ""
	}
	// Seed-admin demotion exemption: compare the RESOLVED, canonical stored email
	// (not identity.Email — a subject-matched user can have un-applied IdP email
	// drift). SeedEmail is already lowercased+trimmed at load; the explicit != ""
	// guard keeps disabled seeding from exempting a user whose email is also "".
	if !desired && h.cfg.SeedEmail != "" && user.Email == h.cfg.SeedEmail {
		slog.Info("oidc login: seed admin exempt from group demotion", "user", user.ID)
		return user, ""
	}
	updated, err := h.q.SetUserAdmin(ctx, store.SetUserAdminParams{ID: user.ID, IsAdmin: desired})
	if err != nil {
		slog.Error("oidc login: sync is_admin from group membership", "user", user.ID, "error", err)
		return store.User{}, oidcErrInternal
	}
	direction := "demote"
	if desired {
		direction = "grant"
	}
	slog.Info("oidc login: synced is_admin from group membership", "user", user.ID, "direction", direction, "admin_groups", h.cfg.OIDCAdminGroups)
	return updated, ""
}

// createOIDCUserFirstAdmin is the passwordless sibling of createUserFirstAdmin
// (review B2): the same advisory-locked check-and-insert, but via CreateUserOIDC
// (NULL password_hash) with the IdP identity attached. The admin decision (PRD #55
// Decision 4): when useGroupAdmin is true (UZI_OIDC_ADMIN_GROUPS configured) the row
// is created with IsAdmin=groupAdmin and the count==0 first-admin rule is disabled;
// otherwise IsAdmin follows count==0, computed under the advisory lock. The lock is
// held in both branches, preserving the concurrent-JIT race handling.
func (h *Handler) createOIDCUserFirstAdmin(r *http.Request, email string, displayName pgtype.Text, issuer, subject string, useGroupAdmin, groupAdmin bool) (store.User, error) {
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
	isAdmin := groupAdmin
	if !useGroupAdmin {
		// Groups not configured: fall back to the first-OIDC-user-admin rule, decided
		// under the lock exactly as password registration does.
		count, err := qtx.CountUsers(ctx)
		if err != nil {
			return store.User{}, err
		}
		isAdmin = count == 0
	}
	user, err := qtx.CreateUserOIDC(ctx, store.CreateUserOIDCParams{
		Email:       email,
		DisplayName: displayName,
		IsAdmin:     isAdmin,
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
func (h *Handler) oidcRefetchAfterRace(ctx context.Context, identity oidc.Identity) (store.User, string) {
	user, err := h.q.GetUserByOIDCSubject(ctx, store.GetUserByOIDCSubjectParams{
		OidcIssuer:  pgtype.Text{String: identity.Issuer, Valid: true},
		OidcSubject: pgtype.Text{String: identity.Subject, Valid: true},
	})
	if err == nil {
		return h.oidcLoginExisting(ctx, user, identity)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("oidc resolve: refetch by subject after race", "error", err)
		return store.User{}, oidcErrInternal
	}
	// The 23505 was an email collision with a different subject; reject, never
	// hijack. Log the subject, not the raw email (PII rule — only operator SeedEmail
	// is ever logged).
	slog.Warn("oidc login: create raced with a different account on this email; refusing", "subject", identity.Subject)
	return store.User{}, oidcErrForbidden
}

// groupsIntersect reports whether any configured group name appears in the claimed
// set. Exact, case-sensitive comparison (PRD #55 Decision 2): config values are
// trimmed at load, claim values come verbatim from the verified ID token, and no
// glob/regex/path-normalization is applied. Either side empty ⇒ no match.
func groupsIntersect(configured, claimed []string) bool {
	if len(configured) == 0 || len(claimed) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(claimed))
	for _, g := range claimed {
		set[g] = struct{}{}
	}
	for _, c := range configured {
		if _, ok := set[c]; ok {
			return true
		}
	}
	return false
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
	// Server-side TTL: reject a cookie older than the window even if the browser
	// kept it past its MaxAge (shares oidcStateTTL with the cookie MaxAge; audit Low1).
	if data.IssuedAt <= 0 || time.Since(time.Unix(data.IssuedAt, 0)) > oidcStateTTL {
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
