package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/clitoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
)

// PRD #112 M1 — the CREDENTIAL LIFECYCLE half of /api/ws's auth boundary.
//
// ws_bearer_livedb_test.go proves the route admits the right credentials and refuses
// the wrong runs. It cannot prove anything about a credential that has DIED, because
// every token it mints is alive: cliMintToken hardcodes a NULL expiry and
// revoked=false. So before this file, no test anywhere asked whether a revoked,
// expired, or disowned token can still open a socket — and M1 is what made that
// question matter, by admitting a long-lived headless credential to a long-lived
// connection for the first time.
//
// Same rig as the bearer suite, and for the same reason: the REAL h.Routes() router
// on a real listener, because RequireUser resolves tokens against cli_tokens + users
// through a concrete *store.Queries (a fake store cannot present a Bearer at all) and
// a WS upgrade needs a hijackable connection (httptest.NewRecorder is not one).
//
// Every refusal is asserted on the STATUS CODE, never on "the dial failed". 401 is
// authN, 403 is websocket.Accept's origin check, 404 is ServeWS's per-run authz —
// three mechanisms a bare error cannot tell apart, and asserting the wrong one is how
// a test goes green against a hole it was written to catch.
//
// POSITIVE CONTROLS — these were run, and a future reader should re-run them before
// trusting this file. Run them over the WHOLE package, not a -run filter: the first
// version of this comment claimed "and nothing else" for mutation A while having only
// ever measured the six tests below, and it was wrong by one. A blast radius measured
// through a filter is not a blast radius (measured 2026-07-25):
//
// Baseline for both, `go test -v -run 'LiveDB$' ./internal/handler/`: 128 PASS, 0 FAIL,
// 0 SKIP.
//
//   - Drop `AND NOT revoked AND (expires_at IS NULL OR expires_at > now())` from
//     getCLITokenByHash → 123 PASS. Reddens the `revoked` and `expired` subtests, the
//     revocation-landed control in (P5) which reads the same predicate, and the
//     pre-existing TestCLIExpiredRevokedReject401LiveDB, which pins the same predicate
//     on the REST path — and nothing else.
//   - Neuter the `!user.IsActive` check in middleware/cli_auth.go → 126 PASS. Reddens
//     exactly the `owner_deactivated` subtest and nothing else.
//
// No mutation smears across unrelated tests: each reddens the assertions that read the
// predicate it broke, and only those.
//
// Mutation B's result is a finding in its own right, and a stronger one than the reason
// this file was written. `owner_deactivated` is the ONLY test in the package that goes
// red when the deactivation check is removed — so before this file, the account
// kill-switch had no gate anywhere, on any route. Not "the WS path lacked coverage
// while REST had it": nothing was watching. That is why the deactivation case below
// carries its own control rather than sharing one.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// probeDial dials /api/ws?run=<runID> with an arbitrary header set and reports the
// handshake status alongside the connection, so a caller can say WHICH gate refused.
func probeDial(ctx context.Context, t *testing.T, url string, hdr http.Header) (*websocket.Conn, int, error) {
	t.Helper()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	return c, wsHandshakeStatus(resp), err
}

// probeRefused asserts the dial was refused with wantStatus, closing the socket if it
// was wrongly accepted so a failing test cannot leak a live connection into the next.
func probeRefused(t *testing.T, c *websocket.Conn, got int, err error, wantStatus int, what string) {
	t.Helper()
	if err == nil {
		wsCloseNow(c)
		t.Fatalf("%s: the upgrade was ACCEPTED — it must be refused with %d", what, wantStatus)
	}
	if got != wantStatus {
		t.Fatalf("%s: refused with status %d, want %d", what, got, wantStatus)
	}
}

// -------------------------------------------------------------------------
// (P1) A uzc_ that is revoked, expired, or whose OWNER was deactivated must not open a
// NEW socket. Revocation and expiry are enforced in getCLITokenByHash's WHERE clause;
// deactivation is a separate check in the middleware, and it is the one that matters
// most here because it neuters a token that is itself still perfectly valid.
//
// The live token on the same run is the control in each case, and it runs FIRST: it
// proves the fixture can open a socket at all, so each 401 below is the credential
// rather than a broken rig.
// -------------------------------------------------------------------------

func TestWSDeadCredentialsCannotOpenLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	srv := wsLiveDBServer(t, router)
	url := wsLiveDBURL(srv, runID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	live := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	c, got, err := probeDial(ctx, t, url, http.Header{"Authorization": []string{"Bearer " + live}})
	if err != nil {
		t.Fatalf("CONTROL: a live uzc_ was refused with status %d: %v — every refusal below would then prove nothing", got, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	past := time.Now().Add(-time.Hour)
	revoked, _ := cliInsertToken(t, pool, owner, clitoken.ScopeUser, nil, true)
	expired, _ := cliInsertToken(t, pool, owner, clitoken.ScopeUser, &past, false)

	t.Run("revoked", func(t *testing.T) {
		c, got, err := probeDial(ctx, t, url, http.Header{"Authorization": []string{"Bearer " + revoked}})
		probeRefused(t, c, got, err, http.StatusUnauthorized,
			"a REVOKED uzc_ opened a socket on its owner's own run")
	})

	t.Run("expired", func(t *testing.T) {
		c, got, err := probeDial(ctx, t, url, http.Header{"Authorization": []string{"Bearer " + expired}})
		probeRefused(t, c, got, err, http.StatusUnauthorized,
			"an EXPIRED uzc_ opened a socket on its owner's own run")
	})

	// The account kill-switch has to reach the socket, not just the REST reads: the
	// token here is untouched and still live, and deactivating its owner is the only
	// thing that changes between the control above and this refusal.
	t.Run("owner_deactivated", func(t *testing.T) {
		cliMustExec(t, pool, `UPDATE users SET is_active = false WHERE id = $1`, owner)
		defer cliMustExec(t, pool, `UPDATE users SET is_active = true WHERE id = $1`, owner)
		c, got, err := probeDial(ctx, t, url, http.Header{"Authorization": []string{"Bearer " + live}})
		probeRefused(t, c, got, err, http.StatusUnauthorized,
			"a DEACTIVATED user's still-live uzc_ opened a socket")
	})
}

// -------------------------------------------------------------------------
// (P2) The uncredentialed upgrade is 401, asserted on the STATUS.
//
// e2e leg 1 asserts only that the dial is "rejected": a Node WebSocket error event
// carries no status, so that probe cannot tell a 401 from a 404, a 403, or a
// connection refused. This pins the code.
//
// The malformed-Authorization cases pin that garbage does not somehow open the socket.
// They do NOT pin the absence of a cookie fallback — with no cookie present, both
// paths answer 401 and the status cannot discriminate. That property is pinned on the
// query trace instead, in ws_route_guard_test.go's bogus-Bearer test.
// -------------------------------------------------------------------------

func TestWSNoCredentialIs401LiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	srv := wsLiveDBServer(t, router)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, got, err := probeDial(ctx, t, wsLiveDBURL(srv, runID), nil)
	probeRefused(t, c, got, err, http.StatusUnauthorized,
		"an upgrade carrying neither a cookie nor a Bearer opened a socket")

	for _, h := range []string{"Bearer", "Bearer ", "Basic dXNlcjpwYXNz", "uzc_bare-no-scheme"} {
		c, got, err := probeDial(ctx, t, wsLiveDBURL(srv, runID),
			http.Header{"Authorization": []string{h}})
		probeRefused(t, c, got, err, http.StatusUnauthorized,
			"an upgrade carrying Authorization: "+h+" opened a socket")
	}
}

// -------------------------------------------------------------------------
// (P3) The origin rule is UNIFORM across credential classes.
//
// M1's security argument is "a cross-site browser page cannot attach an Authorization
// header" — which nothing enforces server-side. What makes that argument safe is that
// websocket.Accept is reached identically on both paths, so a Bearer upgrade carrying
// a foreign Origin is refused exactly like a cookie one. Without this test the shipped
// suite would be consistent with a much weaker property ("Bearer skips the origin
// check"), which is the hole PRD #112 R1 exists to forbid.
//
// The no-Origin dial above is the control: it proves the same token opens the same url
// when the only difference is the header under test.
// -------------------------------------------------------------------------

func TestWSBearerForeignOriginStillRejectedLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	tok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	srv := wsLiveDBServer(t, router)
	url := wsLiveDBURL(srv, runID)
	bearer := []string{"Bearer " + tok}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, got, err := probeDial(ctx, t, url, http.Header{"Authorization": bearer})
	if err != nil {
		t.Fatalf("CONTROL: the no-Origin Bearer dial was refused with status %d: %v — the refusals below would then prove nothing", got, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	// "null" is a sandboxed iframe or a data: page; the third is same-host-wrong-port,
	// which a naive hostname-only comparison would wave through.
	for _, origin := range []string{"http://evil.example", "null", "http://127.0.0.1:1"} {
		c, got, err := probeDial(ctx, t, url,
			http.Header{"Authorization": bearer, "Origin": []string{origin}})
		probeRefused(t, c, got, err, http.StatusForbidden,
			"a Bearer upgrade carrying Origin: "+origin+" was not refused by the origin check")
	}
}

// -------------------------------------------------------------------------
// (P4) Credential-class confusion: a WORKER join token is also presented as
// `Authorization: Bearer`, but it lives in `workers` (jointoken), not `cli_tokens`.
// RequireUser must not resolve it. /api/ws is the one RequireUser route a worker could
// plausibly want — it follows runs — so this is where the classes are closest.
// -------------------------------------------------------------------------

func TestWSWorkerJoinTokenCannotOpenLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	srv := wsLiveDBServer(t, router)

	jt, jhash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("jointoken.Generate: %v", err)
	}
	cliMustExec(t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'probe-worker', $3)`,
		uuid.New(), owner, jhash)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, got, err := probeDial(ctx, t, wsLiveDBURL(srv, runID),
		http.Header{"Authorization": []string{"Bearer " + jt}})
	probeRefused(t, c, got, err, http.StatusUnauthorized,
		"a VALID worker join token (uzw_) opened a user /api/ws socket")
}

// -------------------------------------------------------------------------
// (P5) Revocation does NOT reach an already-open socket. This RECORDS behaviour; it is
// not a defect claim. AuthN runs once, at the handshake, and ServeWS's fan-out loop
// re-checks nothing — which was equally true of the cookie before M1. It is written
// down because the TUI is the first long-lived HEADLESS consumer, so the threat model
// should state the bound rather than assume it, and because a future change that adds
// mid-socket revocation should have a test that notices it changed.
//
// The re-dial is load-bearing: without it, "frames still arrive" is equally explained
// by a revocation that never landed.
//
// The whole value of a change detector is in the going-red, so it does NOT use the
// shared wsReadMarkedFrame: that helper's read diagnostic blames the hub→client wire
// and says "(not the auth change)", which is precisely backwards here. If this read
// ever fails it is most likely because mid-socket revocation was implemented — the one
// cause the shared message rules out — and the next reader would spend an hour in a hub
// that is working fine. wsReadFrameOrExplainRevocation says so instead.
// -------------------------------------------------------------------------

// wsReadFrameOrExplainRevocation is wsReadMarkedFrame with the diagnostic (P5) needs:
// it names the design change that would cause the read to fail, rather than the wire.
// The republish ticker is load-bearing for the same reason it is in the shared helper —
// ServeWS subscribes a beat AFTER the handshake returns, and the hub is live-only, so a
// single publish racing that window is broadcast to nobody and lost.
func wsReadFrameOrExplainRevocation(ctx context.Context, t *testing.T, h *Handler, c *websocket.Conn, runID uuid.UUID, seq int32, marker string) hub.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"marker": marker})
	if err != nil {
		t.Fatalf("marshal marker payload: %v", err)
	}
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.hub.PublishMessage(runID, seq, "text", "coder", "", "", payload, time.Now())
			}
		}
	}()
	defer close(stop)

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("the socket stopped delivering after its uzc_ was revoked: %v\n"+
			"This test RECORDS that revocation does not reach an open socket. If it now does, "+
			"that is very likely a deliberate change (mid-socket revocation, a periodic "+
			"re-auth in ServeWS's loop, or a hub eviction on token revoke) — update PRD #112's "+
			"threat model and rewrite this test to assert the new bound. DO NOT debug the "+
			"hub→client wire: the three tests in ws_bearer_livedb_test.go read frames over "+
			"the same wire and would be red too if it were broken.", err)
	}
	var ev hub.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("the frame on the wire is not a hub.Event: %v (raw %q)", err, string(data))
	}
	return ev
}

func TestWSRevocationDoesNotCloseAnOpenSocketLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	tok, tokID := cliInsertToken(t, pool, owner, clitoken.ScopeUser, nil, false)
	srv := wsLiveDBServer(t, router)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, got, err := probeDial(ctx, t, wsLiveDBURL(srv, runID),
		http.Header{"Authorization": []string{"Bearer " + tok}})
	if err != nil {
		t.Fatalf("the live token was refused with status %d: %v", got, err)
	}
	defer wsCloseNow(c)

	cliMustExec(t, pool, `UPDATE cli_tokens SET revoked = true WHERE id = $1`, tokID)

	c2, got2, err2 := probeDial(ctx, t, wsLiveDBURL(srv, runID),
		http.Header{"Authorization": []string{"Bearer " + tok}})
	probeRefused(t, c2, got2, err2, http.StatusUnauthorized,
		"the revocation did not land (a NEW dial with the revoked token still opened)")

	marker := uuid.NewString()
	ev := wsReadFrameOrExplainRevocation(ctx, t, h, c, runID, 5, marker)
	wsAssertMarkedFrame(t, ev, 5, marker)
	t.Logf("RECORDED BEHAVIOUR: an already-open /api/ws socket keeps delivering frames "+
		"after its uzc_ is revoked (read seq=%d after revocation). AuthN is handshake-only; "+
		"revocation bounds NEW subscriptions, not live ones.", ev.Seq)
}

// -------------------------------------------------------------------------
// (P6) The cookie half of P1: a session invalidated by a token_version bump — the
// "log out everywhere" kill-switch — must not open a new socket either. M1 left the
// cookie path byte-identical, and this is the assertion that says so for the
// credential-lifecycle question rather than the origin one.
// -------------------------------------------------------------------------

func TestWSStaleCookieVersionCannotOpenLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	jwt := cliMintJWT(t, pool, owner)
	srv := wsLiveDBServer(t, router)
	url := wsLiveDBURL(srv, runID)
	cookie := auth.AuthCookieName + "=" + jwt

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, got, err := probeDial(ctx, t, url,
		http.Header{"Cookie": []string{cookie}, "Origin": []string{srv.URL}})
	if err != nil {
		t.Fatalf("CONTROL: a current session cookie was refused with status %d: %v", got, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	cliMustExec(t, pool, `UPDATE users SET token_version = token_version + 1 WHERE id = $1`, owner)

	c2, got2, err2 := probeDial(ctx, t, url,
		http.Header{"Cookie": []string{cookie}, "Origin": []string{srv.URL}})
	probeRefused(t, c2, got2, err2, http.StatusUnauthorized,
		"a session cookie invalidated by a token_version bump still opened a socket")
}
