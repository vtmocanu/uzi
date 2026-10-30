package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// The __Host- prefix makes browsers refuse a Domain or non-root Path for this name,
// so a same-site sibling origin cannot plant or shadow the grant (ADR-3508).
const vaultSlotCookieName = "__Host-uzi_vault_slot"

func (h *Handler) requireAuth() func(http.Handler) http.Handler {
	return mw.RequireAuth(h.q, h.cfg, h.browserAuthHook)
}
func (h *Handler) requireUser() func(http.Handler) http.Handler {
	return mw.RequireUser(h.q, h.cfg, h.browserAuthHook)
}

func clearVaultSlot(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: vaultSlotCookieName, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(0, 0)})
}

func (h *Handler) emitVaultSlot(w http.ResponseWriter, key *vault.BrowserKey) {
	if key == nil {
		return
	}
	defer key.Destroy()
	if !h.cfg.CookieSecure {
		return
	}
	value, err := key.CookieValue()
	if err != nil {
		return
	}
	expires := key.ExpiresAt()
	age := int(time.Until(expires).Seconds())
	if age <= 0 {
		clearVaultSlot(w)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: vaultSlotCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: age})
}

// slotCookie scans at most the HTTP server's bounded header bytes without allocating
// a cookie list. Duplicate and invalid presentations consume one failed reservation.
func slotCookie(r *http.Request) (value string, presented, valid bool) {
	count := 0
	valid = true
	for _, header := range r.Header.Values("Cookie") {
		for header != "" {
			part, rest, _ := strings.Cut(header, ";")
			header = rest
			name, v, hasValue := strings.Cut(strings.TrimSpace(part), "=")
			if strings.TrimSpace(name) != vaultSlotCookieName {
				continue
			}
			count++
			presented = true
			if count != 1 || !hasValue || len(v) != 83 {
				valid = false
				continue
			}
			value = v
		}
	}
	return value, presented, valid && count == 1
}

// slotPresented inspects names only. Secret value parsing waits for reservation.
func slotPresented(r *http.Request) bool {
	for _, header := range r.Header.Values("Cookie") {
		for header != "" {
			part, rest, _ := strings.Cut(header, ";")
			header = rest
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if strings.TrimSpace(name) == vaultSlotCookieName {
				return true
			}
		}
	}
	return false
}

func canonicalBrowserID(value string) (uuid.UUID, bool) {
	id, err := uuid.Parse(value)
	return id, err == nil && id != uuid.Nil && id.String() == value
}

// browserAuthHook clears the slot cookie after a definitive rejection so a revoked
// or unusable grant stops reserving the owner's unlock budget on every request.
// The rejected attempt stays charged; transient failures keep the cookie.
func (h *Handler) browserAuthHook(w http.ResponseWriter, r *http.Request, user store.User, claims *auth.Claims) {
	if h.vault == nil || !h.cfg.CookieSecure || h.vault.Unlocked(user.ID) || !auth.ValidateCSRFToken(r) {
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if r.Header.Get("X-Uzi-Passive") == "1" || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || path == "/api/ws" {
		return
	}
	switch path {
	case "/api/vault/unlock", "/api/vault/lock", "/api/vault/enable", "/api/vault/disable", "/api/vault/passphrase", "/api/auth/logout":
		return
	case "/api/me/settings":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return
		}
	}
	id, ok := canonicalBrowserID(claims.BrowserSlotID)
	if !ok {
		return
	}
	if h.vaultUnlockLimiter == nil || !slotPresented(r) {
		return
	}
	reservation, ok := h.vaultUnlockLimiter.Reserve(mw.VaultUnlockKey(user.ID))
	if !ok {
		return
	}
	value, _, valid := slotCookie(r)
	if !valid {
		slog.Warn("vault restore rejected", "phase", "cookie", "owner", user.ID)
		clearVaultSlot(w)
		return
	}
	key, err := vault.DecodeBrowserKey(value)
	if err != nil {
		slog.Warn("vault restore rejected", "phase", "decode", "owner", user.ID)
		clearVaultSlot(w)
		return
	}
	defer key.Destroy()
	if err := h.vault.Restore(r.Context(), vault.VerifiedSession{Auth: vault.Snapshot(user), TokenVersion: claims.TokenVersion, BrowserID: id}, key); err != nil {
		slog.Warn("vault restore rejected", "phase", "restore", "owner", user.ID)
		if errors.Is(err, vault.ErrStaleAuth) && !errors.Is(err, vault.ErrUncertainOutcome) {
			clearVaultSlot(w)
		}
		return
	}
	reservation.Refund()
	h.vaultReady(r.Context(), user.ID)
}

// preparedSession holds cookies privately until the lifecycle transaction acknowledges.
type preparedSession struct {
	claims  *auth.Claims
	cookies []string
}

func (h *Handler) prepareSession(token string, claims *auth.Claims) (*preparedSession, error) {
	if claims.ExpiresAt == nil || !claims.ExpiresAt.After(time.Now()) {
		return nil, vault.ErrStaleAuth
	}
	recorder := &sessionCookieBuffer{header: make(http.Header)}
	if err := auth.SetAuthCookies(recorder, token, auth.CookieOptions{Secure: h.cfg.CookieSecure, TTL: time.Until(claims.ExpiresAt.Time)}); err != nil {
		return nil, err
	}
	// Cookie expiry follows the signed JWT, including for legacy identity upgrades.
	cookies := (&http.Response{Header: recorder.header}).Cookies()
	result := &preparedSession{claims: claims}
	for _, cookie := range cookies {
		cookie.Expires = claims.ExpiresAt.Time
		result.cookies = append(result.cookies, cookie.String())
	}
	return result, nil
}
func (p *preparedSession) emit(w http.ResponseWriter) {
	retained := make([]string, 0, len(w.Header().Values("Set-Cookie")))
	for _, value := range w.Header().Values("Set-Cookie") {
		name, _, _ := strings.Cut(value, "=")
		if name != auth.AuthCookieName && name != auth.CSRFCookieName {
			retained = append(retained, value)
		}
	}
	w.Header().Del("Set-Cookie")
	for _, value := range retained {
		w.Header().Add("Set-Cookie", value)
	}
	for _, cookie := range p.cookies {
		w.Header().Add("Set-Cookie", cookie)
	}
}
func (h *Handler) freshSession(user store.User) (*preparedSession, error) {
	var browserID uuid.UUID
	var err error
	if h.browserSessionRandom != nil {
		browserID, err = uuid.NewRandomFromReader(h.browserSessionRandom)
	} else {
		browserID, err = uuid.NewRandom()
	}
	if err != nil {
		return nil, err
	}
	token, claims, err := auth.PrepareToken(h.cfg.JWTSecret, user.ID.String(), user.TokenVersion, h.cfg.AuthTokenTTL, browserID.String())
	if err != nil {
		return nil, err
	}
	return h.prepareSession(token, claims)
}
func (h *Handler) mintRequest(claims *auth.Claims) *vault.BrowserMintRequest {
	if !h.cfg.CookieSecure || claims == nil || claims.ExpiresAt == nil {
		return nil
	}
	id, ok := canonicalBrowserID(claims.BrowserSlotID)
	if !ok {
		return nil
	}
	return &vault.BrowserMintRequest{HTTPS: true, BrowserID: id, SessionExpiresAt: claims.ExpiresAt.Time}
}

func (h *Handler) passwordSession(w http.ResponseWriter, r *http.Request, user store.User, snapshot vault.AuthSnapshot, password string) bool {
	session, err := h.freshSession(user)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return false
	}
	var key *vault.BrowserKey
	if h.vault != nil && user.VaultMode == store.VaultModeProtected {
		key, err = h.vault.UnlockAndMint(r.Context(), snapshot, password, vault.InteractiveCreate, h.mintRequest(session.claims))
		if err != nil {
			slog.Warn("vault login failed", "phase", "unlock", "owner", user.ID)
			if errors.Is(err, vault.ErrStaleAuth) {
				httpx.Error(w, http.StatusConflict, "authentication changed; retry login")
				return false
			}
			current, readErr := h.q.GetUserByID(r.Context(), user.ID)
			if readErr != nil || !current.IsActive || current.TokenVersion != user.TokenVersion || current.PasswordHash != user.PasswordHash {
				httpx.Error(w, http.StatusServiceUnavailable, "authentication changed; retry login")
				return false
			}
		} else {
			h.vaultReady(r.Context(), user.ID)
		}
	}
	session.emit(w)
	h.emitVaultSlot(w, key)
	if !user.VaultAutoReunlock {
		clearVaultSlot(w)
	}
	return true
}

// interactiveVault retains the context-only and CLI compatibility paths. A real
// cookie session always uses the immutable auth tuple and combined lifecycle.
func (h *Handler) interactiveVault(w http.ResponseWriter, r *http.Request, user store.User, password string, operation vault.InteractiveOperation) error {
	incoming, browser := mw.ClaimsFromContext(r.Context())
	if !browser {
		if operation == vault.InteractiveExisting {
			return h.vault.UnlockExisting(r.Context(), user.ID, password)
		}
		return h.vault.Enable(r.Context(), user.ID, password)
	}
	claims := *incoming
	var upgrade *preparedSession
	if h.cfg.CookieSecure && claims.BrowserSlotID == "" {
		browserID, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		claims.BrowserSlotID = browserID.String()
		// Identity upgrade preserves the incoming JWT's expiry. Replace any rolling
		// refresh pair with this exact-expiry pair only after successful unlock.
		token, err := auth.SignClaims(h.cfg.JWTSecret, claims)
		if err != nil {
			return err
		}
		upgrade, err = h.prepareSession(token, &claims)
		if err != nil {
			return err
		}
	}
	key, err := h.vault.UnlockAndMint(r.Context(), vault.Snapshot(user), password, operation, h.mintRequest(&claims))
	if err != nil {
		return err
	}
	if upgrade != nil {
		upgrade.emit(w)
	}
	h.emitVaultSlot(w, key)
	if !user.VaultAutoReunlock {
		clearVaultSlot(w)
	}
	return nil
}

// sessionCookieBuffer is never connected to a client.
type sessionCookieBuffer struct{ header http.Header }

func (b *sessionCookieBuffer) Header() http.Header         { return b.header }
func (b *sessionCookieBuffer) Write(p []byte) (int, error) { return len(p), nil }
func (b *sessionCookieBuffer) WriteHeader(int)             {}
