package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

// M5's browser-brokered auth-flow LiveDB suite (PRD #64). These exercise the REAL
// h.Routes() router end to end — start (unauth) → approve (cookie+CSRF) → poll
// (unauth) — because the properties under test are transactional: the claim-first
// mint that must fire exactly once even under concurrent polls, the roll-back (never
// deny) on a verifier mismatch, and server-side expiry. A fake store cannot exhibit
// the READ COMMITTED race the claim-first UPDATE exists to close.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// cliPostJSON drives an UNAUTH POST with a JSON body — the start/poll transport (the
// CLI has no credential yet, which is the whole point).
func cliPostJSON(router http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// cliStart begins a login and returns the request_id and the DISPLAY user_code
// (XXXX-XXXX) the CLI would print for the human to type.
func cliStart(t *testing.T, router http.Handler, challenge, desc string) (requestID, userCode string) {
	t.Helper()
	rec := cliPostJSON(router, "/api/auth/cli/start",
		fmt.Sprintf(`{"code_challenge":%q,"client_desc":%q}`, challenge, desc))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d, want 201\nbody: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		RequestID string `json:"request_id"`
		UserCode  string `json:"user_code"`
		ExpiresIn int    `json:"expires_in"`
		Interval  int    `json:"interval"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("start decode: %v (body %s)", err, rec.Body.String())
	}
	if body.RequestID == "" || body.UserCode == "" {
		t.Fatalf("start returned empty request_id/user_code: %s", rec.Body.String())
	}
	// The interval the CLI honours must stay comfortably under the poll limiter budget
	// (they are one decision) — a positive value the client can sleep on.
	if body.Interval <= 0 || body.ExpiresIn <= 0 {
		t.Fatalf("start returned non-positive interval/expires_in: %+v", body)
	}
	return body.RequestID, body.UserCode
}

// cliCountTokens returns how many cli_tokens rows exist for a user — the mint-exactly-
// once invariant is asserted on this count, not just on the HTTP status.
func cliCountTokens(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM cli_tokens WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count cli_tokens: %v", err)
	}
	return n
}

// -------------------------------------------------------------------------
// (1) The happy path proves the whole handshake AND mint-exactly-once: start →
// approve → poll mints a working uzc_, a replayed poll is 410 and mints nothing more.
// -------------------------------------------------------------------------

func TestCLIAuthFlowMintOnceLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)

	verifier := "verifier-" + uuid.NewString()
	challenge := s256Challenge(verifier)
	reqID, userCode := cliStart(t, router, challenge, "laptop (darwin/arm64)")

	// approve marks the row (mints NOTHING); the human typed the code the CLI printed.
	rec := cookieReq(t, router, http.MethodPost, "/api/auth/cli/approve", jwt,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q,"scope":"user"}`, reqID, userCode))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, user); n != 0 {
		t.Fatalf("approve minted %d tokens; approve must mint NOTHING", n)
	}

	// poll mints once, inside the poll tx, and returns the token + session user.
	rec = cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, verifier))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll (mint) = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
		User  struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("poll decode: %v (body %s)", err, rec.Body.String())
	}
	if !strings.HasPrefix(minted.Token, clitoken.PrefixUser) {
		t.Errorf("minted token has prefix %q, want %q", minted.Token[:4], clitoken.PrefixUser)
	}
	if minted.User.ID != user.String() {
		t.Errorf("poll returned user %q, want %q", minted.User.ID, user.String())
	}
	if n := cliCountTokens(t, pool, user); n != 1 {
		t.Fatalf("after mint there are %d tokens, want exactly 1", n)
	}

	// The minted token is a real working Bearer credential.
	if rec := bearerReq(router, http.MethodGet, "/api/auth/me", minted.Token); rec.Code != http.StatusOK {
		t.Errorf("minted uzc_ GET /auth/me = %d, want 200 (the token must actually work)\nbody: %s", rec.Code, rec.Body.String())
	}

	// A replayed poll is 410 consumed and mints NOTHING more — the token is returned
	// once, ever.
	rec = cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, verifier))
	if rec.Code != http.StatusGone {
		t.Errorf("replayed poll = %d, want 410 (mint exactly once)\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, user); n != 1 {
		t.Errorf("replayed poll changed the token count to %d, want 1", n)
	}
}

// -------------------------------------------------------------------------
// (2) A wrong verifier never mints AND rolls back (does NOT mark the row denied): a
// junk poll from someone who learned the non-secret request_id must not kill a live
// login, so a subsequent CORRECT poll still mints. This is the "roll back, never
// deny" ruling, proven by the fact that the legitimate poll afterwards succeeds.
// -------------------------------------------------------------------------

func TestCLIAuthWrongVerifierRollsBackLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)

	verifier := "verifier-" + uuid.NewString()
	challenge := s256Challenge(verifier)
	reqID, userCode := cliStart(t, router, challenge, "race")
	rec := cookieReq(t, router, http.MethodPost, "/api/auth/cli/approve", jwt,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q,"scope":"user"}`, reqID, userCode))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	// A poll with the WRONG verifier is rejected and mints nothing.
	rec = cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, "not-the-verifier"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-verifier poll = %d, want 401\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, user); n != 0 {
		t.Fatalf("wrong-verifier poll minted %d tokens, want 0", n)
	}

	// The row was rolled back to 'approved' (NOT denied), so the CORRECT poll still
	// mints — proving the junk poll did not kill the login.
	rec = cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, verifier))
	if rec.Code != http.StatusOK {
		t.Fatalf("correct poll after a junk poll = %d, want 200 (roll-back must keep the login alive)\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, user); n != 1 {
		t.Errorf("after the correct poll there are %d tokens, want 1", n)
	}
}

// -------------------------------------------------------------------------
// (3) Expiry is enforced SERVER-SIDE, at both gates the client cannot influence: an
// approved-but-expired request cannot be claimed/minted, and a pending-but-expired
// request cannot be approved.
// -------------------------------------------------------------------------

func TestCLIAuthExpiryServerSideLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	verifier := "verifier-" + uuid.NewString()
	challenge := s256Challenge(verifier)
	past := time.Now().Add(-time.Minute)

	// (a) An APPROVED but EXPIRED request cannot mint — the claim guard's expires_at >
	// now() fails, so the poll is 410 and no token appears.
	codeA, err := generateUserCode()
	if err != nil {
		t.Fatalf("generateUserCode: %v", err)
	}
	var reqA uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO cli_auth_requests (code_challenge, client_desc, user_code, status, user_id, scope, expires_at)
		 VALUES ($1, 'expired-approved', $2, 'approved', $3, 'user', $4) RETURNING id`,
		challenge, codeA, user, past).Scan(&reqA); err != nil {
		t.Fatalf("insert expired approved request: %v", err)
	}
	rec := cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqA.String(), verifier))
	if rec.Code != http.StatusGone {
		t.Errorf("poll of an approved-but-expired request = %d, want 410\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, user); n != 0 {
		t.Errorf("an expired request minted %d tokens, want 0", n)
	}

	// (b) A PENDING but EXPIRED request cannot be approved.
	codeB, err := generateUserCode()
	if err != nil {
		t.Fatalf("generateUserCode: %v", err)
	}
	var reqB uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO cli_auth_requests (code_challenge, client_desc, user_code, status, scope, expires_at)
		 VALUES ($1, 'expired-pending', $2, 'pending', 'user', $3) RETURNING id`,
		challenge, codeB, past).Scan(&reqB); err != nil {
		t.Fatalf("insert expired pending request: %v", err)
	}
	rec = cookieReq(t, router, http.MethodPost, "/api/auth/cli/approve", jwt,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q,"scope":"user"}`, reqB.String(), displayUserCode(codeB)))
	if rec.Code != http.StatusGone {
		t.Errorf("approve of an expired pending request = %d, want 410\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (4) The load-bearing one: N CONCURRENT polls of a single approved request mint
// EXACTLY ONE token. Claim-first (the atomic approved→consumed UPDATE...RETURNING) is
// what makes this hold under READ COMMITTED; "inside a transaction" alone would let
// two polls both SELECT approved and both mint. A fake store cannot exhibit this.
// -------------------------------------------------------------------------

func TestCLIAuthConcurrentPollMintsOnceLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	verifier := "verifier-" + uuid.NewString()
	challenge := s256Challenge(verifier)
	reqID, userCode := cliStart(t, router, challenge, "concurrent")
	rec := cookieReq(t, router, http.MethodPost, "/api/auth/cli/approve", jwt,
		fmt.Sprintf(`{"request_id":%q,"user_code":%q,"scope":"user"}`, reqID, userCode))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	body := fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, verifier)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			codes[i] = cliPostJSON(router, "/api/auth/cli/poll", body).Code
		}(i)
	}
	wg.Wait()

	got200 := 0
	for _, c := range codes {
		if c == http.StatusOK {
			got200++
		} else if c != http.StatusGone && c != http.StatusAccepted {
			t.Errorf("concurrent poll returned %d, want one of {200, 410, 202}", c)
		}
	}
	if got200 != 1 {
		t.Errorf("%d concurrent polls returned %d 200s, want exactly 1", n, got200)
	}
	if tokens := cliCountTokens(t, pool, user); tokens != 1 {
		t.Errorf("%d concurrent polls minted %d tokens, want exactly 1 (claim-first must hold under READ COMMITTED)", n, tokens)
	}
}

// -------------------------------------------------------------------------
// (5) Deny is terminal: after the human clicks Deny, the poll reports denied (410)
// and no token is ever minted, even with the correct verifier.
// -------------------------------------------------------------------------

func TestCLIAuthDenyLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	verifier := "verifier-" + uuid.NewString()
	challenge := s256Challenge(verifier)
	reqID, _ := cliStart(t, router, challenge, "denied")

	rec := cookieReq(t, router, http.MethodPost, "/api/auth/cli/deny", jwt,
		fmt.Sprintf(`{"request_id":%q}`, reqID))
	if rec.Code != http.StatusOK {
		t.Fatalf("deny = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	rec = cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, reqID, verifier))
	if rec.Code != http.StatusGone {
		t.Errorf("poll after deny = %d, want 410\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, user); n != 0 {
		t.Errorf("a denied request minted %d tokens, want 0", n)
	}
}

// -------------------------------------------------------------------------
// (5b) The admin_ro approve gate mirrors the static mint gate: a NON-admin approving
// with scope=admin_ro is 403 (and mints nothing on the subsequent poll), while an
// ADMIN's admin_ro approval mints a working uza_. Resolved live from is_admin, never
// from the presenting credential.
// -------------------------------------------------------------------------

func TestCLIAuthApproveAdminROGateLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)

	// A non-admin cannot approve an admin_ro login.
	nonAdmin := cliSeedUser(t, pool, false)
	v1 := "verifier-" + uuid.NewString()
	req1, code1 := cliStart(t, router, s256Challenge(v1), "non-admin box")
	rec := cookieReq(t, router, http.MethodPost, "/api/auth/cli/approve", cliMintJWT(t, pool, nonAdmin),
		fmt.Sprintf(`{"request_id":%q,"user_code":%q,"scope":"admin_ro"}`, req1, code1))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin approve admin_ro = %d, want 403\nbody: %s", rec.Code, rec.Body.String())
	}
	if rec := cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, req1, v1)); rec.Code != http.StatusAccepted {
		t.Errorf("poll after a refused approve = %d, want 202 pending (nothing was approved)\nbody: %s", rec.Code, rec.Body.String())
	}
	if n := cliCountTokens(t, pool, nonAdmin); n != 0 {
		t.Errorf("a refused admin_ro approve minted %d tokens, want 0", n)
	}

	// An admin's admin_ro approval mints a working uza_.
	admin := cliSeedUser(t, pool, true)
	v2 := "verifier-" + uuid.NewString()
	req2, code2 := cliStart(t, router, s256Challenge(v2), "admin box")
	if rec := cookieReq(t, router, http.MethodPost, "/api/auth/cli/approve", cliMintJWT(t, pool, admin),
		fmt.Sprintf(`{"request_id":%q,"user_code":%q,"scope":"admin_ro"}`, req2, code2)); rec.Code != http.StatusOK {
		t.Fatalf("admin approve admin_ro = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	rec = cliPostJSON(router, "/api/auth/cli/poll",
		fmt.Sprintf(`{"request_id":%q,"verifier":%q}`, req2, v2))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll (admin_ro mint) = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("poll decode: %v", err)
	}
	if !strings.HasPrefix(minted.Token, clitoken.PrefixAdmin) {
		t.Errorf("admin_ro mint returned prefix %q, want %q", minted.Token[:4], clitoken.PrefixAdmin)
	}
	// The uza_ actually reads an admin surface.
	if rec := bearerReq(router, http.MethodGet, "/api/admin/users", minted.Token); rec.Code != http.StatusOK {
		t.Errorf("minted uza_ GET /api/admin/users = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (6) The consent-screen GET (RequireAuth) exposes client_desc + status but NEVER the
// code_challenge or the user_code — withholding user_code is what forces the human to
// TYPE the code from their terminal (the anti-race/anti-async-phishing property). It
// is also cookie-only: a Bearer CLI token cannot read it.
// -------------------------------------------------------------------------

func TestCLIAuthGetRequestMetadataLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)
	verifier := "verifier-" + uuid.NewString()
	challenge := s256Challenge(verifier)
	reqID, userCode := cliStart(t, router, challenge, "my-host (linux)")

	rec := cookieReq(t, router, http.MethodGet, "/api/auth/cli/request/"+reqID, jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get request = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "my-host (linux)") {
		t.Errorf("consent metadata missing client_desc: %s", body)
	}
	// The user_code (canonical or display form) and the code_challenge must NEVER be in
	// the consent payload.
	canonical := strings.ReplaceAll(userCode, "-", "")
	for _, leaked := range []string{canonical, userCode, challenge} {
		if strings.Contains(body, leaked) {
			t.Errorf("consent metadata leaked a secret/type-me value %q: %s", leaked, body)
		}
	}

	// Cookie-only: a valid Bearer CLI token is rejected on this route (it takes the
	// bearer path, and the request endpoint is not RequireUser).
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	if rec := bearerReq(router, http.MethodGet, "/api/auth/cli/request/"+reqID, uzc); rec.Code != http.StatusUnauthorized {
		t.Errorf("Bearer GET consent metadata = %d, want 401 (RequireAuth, cookie-only)\nbody: %s", rec.Code, rec.Body.String())
	}
}

// -------------------------------------------------------------------------
// (7) N2 regression: a client_desc whose 200-byte truncation boundary lands
// MID-RUNE must not 500 on this UNAUTH endpoint. The old byte-slice
// desc[:maxClientDescBytes] could split a multibyte rune into invalid UTF-8,
// which the INSERT rejected → a 500 at start. The rune-aware handling either
// stores the whole value (rune count within the cap) or rejects it with a 400 —
// never a 500, and never an invalid-UTF-8 row.
// -------------------------------------------------------------------------

func TestCLIAuthStartClientDescRuneSafeLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	jwt := cliMintJWT(t, pool, user)

	// 150 three-byte runes = 450 bytes; byte index 200 (the old truncation boundary)
	// falls INSIDE a rune, so desc[:200] would have produced invalid UTF-8. 150 runes
	// is within the cap, so the value must be stored WHOLE (not sliced), and start
	// must return 201 rather than 500.
	desc := strings.Repeat("中", 150)
	verifier := "verifier-" + uuid.NewString()
	reqID, _ := cliStart(t, router, s256Challenge(verifier), desc)

	// The stored client_desc round-trips intact (valid UTF-8, never byte-sliced).
	rec := cookieReq(t, router, http.MethodGet, "/api/auth/cli/request/"+reqID, jwt, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get request = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var meta struct {
		ClientDesc string `json:"client_desc"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("consent decode: %v (body %s)", err, rec.Body.String())
	}
	if meta.ClientDesc != desc {
		t.Errorf("client_desc round-trip mismatch: stored %d-rune value, want the full %d-rune input intact",
			utf8.RuneCountInString(meta.ClientDesc), utf8.RuneCountInString(desc))
	}

	// A client_desc BEYOND the cap (201 runes) is rejected with a 400 — never a 500,
	// and never a byte-sliced (invalid-UTF-8) row.
	overLong := strings.Repeat("中", maxClientDescRunes+1)
	rec = cliPostJSON(router, "/api/auth/cli/start",
		fmt.Sprintf(`{"code_challenge":%q,"client_desc":%q}`, s256Challenge("v2-"+uuid.NewString()), overLong))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-long client_desc start = %d, want 400 (must not 500 on an unauth endpoint)\nbody: %s",
			rec.Code, rec.Body.String())
	}
}
