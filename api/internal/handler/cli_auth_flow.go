package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// The browser-brokered, poll-based `uzi login` flow (PRD #64 M5). The CLI mints a
// PKCE verifier locally, sends only its S256 challenge to start, and polls; the
// human approves in an already-authenticated browser tab, where password OR OIDC
// login happens. No loopback listener, no session JWT in a URL — so it works over
// SSH and in containers. No plaintext token is ever stored, not even sealed: approve
// only marks the row, and the token is minted claim-first INSIDE the poll tx.

const (
	// cliAuthRequestTTL is how long a start'd request lives before the human must
	// have approved and the CLI claimed it. Short by design (~5 min): a login the
	// human walked away from should not leave an approvable request lingering.
	cliAuthRequestTTL = 5 * time.Minute
	// cliAuthPollInterval is the poll cadence the SERVER returns to the CLI (seconds).
	// The CLI honours it, and cliPollLimiter is sized to comfortably exceed the
	// resulting rate (12/min at 5s) — the two are one decision (config.go).
	cliAuthPollInterval = 5
	// maxUserCodeAttempts bounds the retry loop on a user_code UNIQUE collision at
	// start. ~40 bits over a ≤5-min window makes a collision negligible-but-not-never,
	// so a handful of fresh-code retries turns the loud insert failure into a
	// non-event rather than a 500.
	maxUserCodeAttempts = 5
	// maxCodeChallengeBytes / maxClientDescRunes bound the two client-supplied fields
	// on start so a caller cannot store oversized blobs on an UNAUTHENTICATED endpoint.
	// The challenge is base64url(S256(verifier)) = 43 chars; the desc is display text.
	// The desc is bounded by RUNE count (not bytes) and REJECTED when over-long: a byte
	// cap that sliced the string could split a multibyte rune into invalid UTF-8, which
	// the INSERT rejects → a 500 on an unauthenticated endpoint (mirrors handler/forge.go).
	maxCodeChallengeBytes = 256
	maxClientDescRunes    = 200
)

// crockfordAlphabet is Crockford base32 (0-9 A-Z minus I, L, O, U): unambiguous when
// read aloud or retyped. user_code is 8 of these (~40 bits), rendered XXXX-XXXX.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// userCodeLen is the number of Crockford characters in a user_code (before the
// display hyphen). 8 ≈ 40 bits, comfortably above RFC 8628's own example.
const userCodeLen = 8

// generateUserCode returns the canonical 8-char user_code (no hyphen) stored in the
// DB. 256 is an exact multiple of 32, so masking a random byte with 0x1f is unbiased.
func generateUserCode() (string, error) {
	buf := make([]byte, userCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(userCodeLen)
	for _, x := range buf {
		b.WriteByte(crockfordAlphabet[x&0x1f])
	}
	return b.String(), nil
}

// displayUserCode renders the stored canonical code as XXXX-XXXX for the CLI to
// print and the human to type. The hyphen is cosmetic — normalizeUserCode strips it.
func displayUserCode(code string) string {
	if len(code) == userCodeLen {
		return code[:4] + "-" + code[4:]
	}
	return code
}

// normalizeUserCode folds a human-typed code back to the canonical stored form:
// uppercase, hyphens/spaces dropped, and the Crockford digit-misreads (O→0, I/L→1)
// substituted so a careful-but-imperfect read still matches. Anything outside the
// alphabet is dropped, so a garbage input simply fails the exact compare in approve.
func normalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case 'O':
			b.WriteByte('0')
		case 'I', 'L':
			b.WriteByte('1')
		default:
			if strings.ContainsRune(crockfordAlphabet, r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// s256Challenge is the server side of PKCE S256: base64url(sha256(verifier)),
// RawURLEncoding to match the client. The poll compares this against the stored
// code_challenge in constant time.
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// cliRequestExpired reports whether a fetched request row is past its expiry.
// expires_at is NOT NULL on this table, so there is no NULL trap here.
func cliRequestExpired(row store.CliAuthRequest) bool {
	return !row.ExpiresAt.Time.After(time.Now())
}

// CLIAuthStart begins a browser-brokered login (POST /api/auth/cli/start). UNAUTH by
// design — the CLI has no credential yet; that is the whole point — and behind the
// auth limiter. It stores the client's S256 challenge, assigns a unique user_code +
// ~5-min expiry, opportunistically sweeps expired rows, and returns the request_id,
// the display code, and the poll cadence the CLI must honour.
func (h *Handler) CLIAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodeChallenge string `json:"code_challenge"`
		ClientDesc    string `json:"client_desc"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	challenge := strings.TrimSpace(req.CodeChallenge)
	if challenge == "" || len(challenge) > maxCodeChallengeBytes {
		httpx.Error(w, http.StatusBadRequest, "code_challenge is required")
		return
	}
	desc := strings.TrimSpace(req.ClientDesc)
	if desc == "" {
		desc = "unknown client"
	}
	if utf8.RuneCountInString(desc) > maxClientDescRunes {
		// Reject rather than byte-slice: slicing at a byte offset can split a multibyte
		// rune into invalid UTF-8, which the INSERT rejects → a 500 on this UNAUTH
		// endpoint. A client_desc is auto-derived hostname/os, so a clear 400 is fine.
		httpx.Error(w, http.StatusBadRequest, "client_desc is too long")
		return
	}
	// #169: this string becomes a cli_tokens.name via browserTokenName, and that name is
	// a NAME column in `uzi admin cli-tokens` beside another user's owner_email. Same rule
	// as the static mint path in cli_tokens.go, and rejecting rather than stripping is the
	// idiom the length check directly above already set on this same field.
	if err := termsafe.Validate("client_desc", desc); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	// Opportunistic sweep — the start handler is the repo's precedent for stale-row
	// cleanup (best-effort; a failure must not fail the login).
	if _, err := h.q.DeleteExpiredCLIAuthRequests(ctx); err != nil {
		slog.Warn("cli auth: sweep expired requests", "error", err)
	}

	expiresAt := pgtype.Timestamptz{Time: time.Now().Add(cliAuthRequestTTL), Valid: true}
	for attempt := 0; attempt < maxUserCodeAttempts; attempt++ {
		code, err := generateUserCode()
		if err != nil {
			slog.Error("cli auth: generate user_code", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		row, err := h.q.CreateCLIAuthRequest(ctx, store.CreateCLIAuthRequestParams{
			CodeChallenge: challenge,
			ClientDesc:    desc,
			UserCode:      code,
			ExpiresAt:     expiresAt,
		})
		if err != nil {
			// A user_code collision (~40 bits, ≤5-min window) is a loud UNIQUE failure,
			// not a silent cross-wire: retry with a fresh code rather than 500.
			if isUniqueViolation(err) {
				continue
			}
			slog.Error("cli auth: create request", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{
			"request_id": row.ID.String(),
			"user_code":  displayUserCode(code),
			"expires_in": int(cliAuthRequestTTL.Seconds()),
			"interval":   cliAuthPollInterval,
		})
		return
	}
	slog.Error("cli auth: user_code collisions exhausted", "attempts", maxUserCodeAttempts)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// CLIAuthPoll is the CLI's poll (POST /api/auth/cli/poll). UNAUTH, behind the
// dedicated cliPollLimiter. POST not GET so the verifier never lands in an access
// log. The mint is CLAIM-FIRST inside a single transaction: an atomic
// approved→consumed UPDATE...RETURNING is the guard, so two concurrent polls cannot
// both mint. On a verifier/challenge mismatch the whole tx rolls back (row stays
// 'approved', never 'denied') and slog.Warn records the signal — the request_id is
// not a secret, so rolling back keeps a live login alive against a junk poller.
func (h *Handler) CLIAuthPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"request_id"`
		Verifier  string `json:"verifier"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(req.RequestID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request_id")
		return
	}
	verifier := strings.TrimSpace(req.Verifier)
	if verifier == "" {
		httpx.Error(w, http.StatusBadRequest, "verifier is required")
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("cli auth poll: begin tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit; the roll-back-on-mismatch path relies on it
	qtx := h.q.WithTx(tx)

	claimed, err := qtx.ClaimCLIAuthRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		// No approved-and-unexpired row: report the poll status (pending → keep polling,
		// anything terminal → stop). The claim matched 0 rows, so nothing is pending in
		// this tx — end it NOW to release its connection before the status read (which
		// uses the pool). Otherwise a pending poll (the hot path) would hold two
		// connections at once. The deferred Rollback then no-ops (ErrTxClosed).
		_ = tx.Rollback(ctx)
		h.writeCLIPollStatus(w, ctx, id)
		return
	}
	if err != nil {
		slog.Error("cli auth poll: claim", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// A row was claimed (flipped approved→consumed in-tx). Verify the verifier binds to
	// the stored challenge; on mismatch return without committing so the defer's
	// Rollback restores the row to 'approved'. Do NOT mark denied (PRD): a junk poll
	// from someone who learned the non-secret request_id must not kill a live login.
	if subtle.ConstantTimeCompare([]byte(s256Challenge(verifier)), []byte(claimed.CodeChallenge)) != 1 {
		slog.Warn("cli auth poll: verifier does not match challenge; rolling back (row stays approved)", "request_id", id)
		httpx.Error(w, http.StatusUnauthorized, "verifier does not match")
		return
	}

	if !claimed.UserID.Valid {
		// An approved row always carries a user_id; a NULL here is a bug, not a client
		// condition. Roll back and 500 rather than mint an ownerless token.
		slog.Error("cli auth poll: approved request has no user_id", "request_id", id)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	userID := uuid.UUID(claimed.UserID.Bytes)
	user, err := qtx.GetUserByID(ctx, userID)
	if err != nil {
		slog.Error("cli auth poll: load user", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !user.IsActive {
		httpx.Error(w, http.StatusForbidden, "account is deactivated")
		return
	}

	token, hash, prefix, err := clitoken.Generate(claimed.Scope)
	if err != nil {
		slog.Error("cli auth poll: generate token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Expiry matrix at mint, browser path: a user token is 90 days (human-held,
	// re-running uzi login is trivial) and an admin_ro token is 90 days (scope wins) —
	// so the browser path is always bounded, never NULL. The server sets it; the client
	// never proposes a lifetime.
	expires := pgtype.Timestamptz{Time: time.Now().Add(cliTokenTTL), Valid: true}
	row, err := qtx.CreateCLIToken(ctx, store.CreateCLITokenParams{
		UserID:      userID,
		Name:        browserTokenName(claimed.ClientDesc),
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scope:       claimed.Scope,
		ExpiresAt:   expires,
	})
	if err != nil {
		slog.Error("cli auth poll: mint token", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("cli auth poll: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if claimed.Scope == clitoken.ScopeAdminRO {
		// The only detection breadcrumb for a factory-wide-read credential's mint (Risk
		// 8), mirroring the static-mint path.
		slog.Info("admin_ro cli token minted via browser login", "user_id", userID, "token_id", row.ID, "token_prefix", prefix)
	}

	// Returned ONCE, ever. `user` is the real session user (the login result); the scope
	// ceiling is applied per-request by RequireUser, not baked into this response.
	httpx.JSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user":  toDTO(user),
	})
}

// writeCLIPollStatus maps a not-yet-mintable request to the poll response the CLI
// branches on: 202 pending (keep polling), or 410 with a terminal reason (stop). A
// missing row (swept or bad id) reads as expired — the request_id is not a secret,
// so this does not leak existence.
func (h *Handler) writeCLIPollStatus(w http.ResponseWriter, ctx context.Context, id uuid.UUID) {
	row, err := h.q.GetCLIAuthRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.JSON(w, http.StatusGone, map[string]any{"status": "expired"})
		return
	}
	if err != nil {
		slog.Error("cli auth poll: status lookup", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	switch row.Status {
	case "pending", "approved":
		// 'approved' can appear here only if it just raced past the claim's expiry check
		// or was approved between the claim and this read; either way, if it is still
		// live the next poll claims it, so report pending. If expired, it is terminal.
		if cliRequestExpired(row) {
			httpx.JSON(w, http.StatusGone, map[string]any{"status": "expired"})
			return
		}
		httpx.JSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
	case "denied":
		httpx.JSON(w, http.StatusGone, map[string]any{"status": "denied"})
	case "consumed":
		// A replayed poll after a successful mint: the token was returned once, and this
		// second poll must not re-mint. 410 is the "mint exactly once" backstop.
		httpx.JSON(w, http.StatusGone, map[string]any{"status": "consumed"})
	default:
		httpx.JSON(w, http.StatusGone, map[string]any{"status": "expired"})
	}
}

// CLIAuthGetRequest returns consent-screen metadata (GET /api/auth/cli/request/{id},
// RequireAuth — this is where the human's password/OIDC login happens). It exposes
// ONLY client_desc + status + expires_at: never the code_challenge, and never the
// user_code. Withholding user_code is deliberate — the human must TYPE the code shown
// in THEIR terminal, which interrupts the approve-the-tab reflex and is what buys the
// anti-race/anti-async-phishing property. M6's consent page must therefore render a
// code input, not auto-fill.
func (h *Handler) CLIAuthGetRequest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request id")
		return
	}
	row, err := h.q.GetCLIAuthRequest(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		slog.Error("cli auth: get request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	status := row.Status
	if status == "pending" && cliRequestExpired(row) {
		status = "expired"
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"client_desc": row.ClientDesc,
		"status":      status,
		"expires_at":  row.ExpiresAt.Time,
	})
}

// CLIAuthApprove marks a request approved (POST /api/auth/cli/approve, RequireAuth +
// CSRF + per-user limiter). It MINTS NOTHING — approve only binds the row to the
// session user and the consent-screen scope; the token is minted claim-first in the
// poll tx. The human must have TYPED the user_code from their terminal, and the server
// validates it here (making this a credential-checking endpoint, which is why it rides
// the per-user auth limiter — exactly as vault unlock does). admin_ro is gated on live
// is_admin, mirroring the static mint.
func (h *Handler) CLIAuthApprove(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
		UserCode  string `json:"user_code"`
		Scope     string `json:"scope"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(req.RequestID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request_id")
		return
	}
	scope := req.Scope
	if scope == "" {
		scope = clitoken.ScopeUser
	}
	switch scope {
	case clitoken.ScopeUser:
		// default; capped to the owner's own authority by the RequireUser masking.
	case clitoken.ScopeAdminRO:
		// admin_ro reads the whole factory, so only an admin may approve one — resolved
		// live from the row, never the credential, exactly like the static mint gate.
		if !user.IsAdmin {
			httpx.Error(w, http.StatusForbidden, "admin access required to approve an admin-scoped login")
			return
		}
	default:
		httpx.Error(w, http.StatusBadRequest, "scope must be 'user' or 'admin_ro'")
		return
	}

	ctx := r.Context()
	row, err := h.q.GetCLIAuthRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, http.StatusNotFound, "request not found")
		return
	}
	if err != nil {
		slog.Error("cli auth approve: get request", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if cliRequestExpired(row) {
		httpx.Error(w, http.StatusGone, "request expired")
		return
	}
	if row.Status != "pending" {
		httpx.Error(w, http.StatusConflict, "request is no longer pending")
		return
	}
	// The typed code must match the code the CLI printed. Constant-time compare of the
	// normalized forms — it is a low-entropy human-typed confirmation, but there is no
	// reason to leak timing on it.
	if subtle.ConstantTimeCompare([]byte(normalizeUserCode(req.UserCode)), []byte(row.UserCode)) != 1 {
		httpx.Error(w, http.StatusBadRequest, "the code you entered does not match")
		return
	}

	n, err := h.q.ApproveCLIAuthRequest(ctx, store.ApproveCLIAuthRequestParams{
		ID:     id,
		UserID: pgtype.UUID{Bytes: user.ID, Valid: true},
		Scope:  scope,
	})
	if err != nil {
		slog.Error("cli auth approve", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		// Raced to non-pending/expired between the read above and the guarded update.
		httpx.Error(w, http.StatusConflict, "request is no longer pending")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "approved"})
}

// CLIAuthDeny marks a pending request denied (POST /api/auth/cli/deny, RequireAuth +
// CSRF) — the consent screen's Deny button. A non-pending or expired request is a 409
// (nothing to deny), never a false success.
func (h *Handler) CLIAuthDeny(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		RequestID string `json:"request_id"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(req.RequestID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request_id")
		return
	}
	n, err := h.q.DenyCLIAuthRequest(r.Context(), id)
	if err != nil {
		slog.Error("cli auth deny", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusConflict, "request is no longer pending")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "denied"})
}

// browserTokenName is the default name for a browser-minted CLI token: the client's
// self-description (hostname/os), bounded to the same cap as a static token's name. It
// is what M6's token list renders so a human can tell their laptops apart.
func browserTokenName(clientDesc string) string {
	name := strings.TrimSpace(clientDesc)
	if name == "" {
		name = "uzi login"
	}
	if utf8.RuneCountInString(name) > maxCLITokenNameBytes {
		// Rune-safe truncate (mirrors deriveChatTitle): a byte-slice here could split a
		// multibyte rune into invalid UTF-8 and 500 the mint. Effectively dead because
		// CLIAuthStart already rejects an over-cap client_desc, but hardened regardless.
		name = string([]rune(name)[:maxCLITokenNameBytes])
	}
	return name
}
