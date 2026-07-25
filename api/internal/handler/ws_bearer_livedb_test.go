package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/clitoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
)

// PRD #112 M1: /api/ws moved out of the cookie-only tail into a RequireUser mount,
// so a headless CLI/TUI can subscribe with a uzc_/uza_ Bearer token.
//
// These live as a LiveDB suite against the REAL h.Routes() router for the same
// reason the PRD #64 ceiling tests do, and here the reason is sharper: the property
// under test IS which middleware group handler.go mounted /ws in. RequireUser takes
// a concrete *store.Queries and resolves the token against cli_tokens + users, so a
// fake store cannot present a Bearer credential at all; and a hand-wired middleware
// chain (what runs_test.go's wsTestServer does, deliberately, to pin ServeWS's own
// behaviour) would go green with the route still in the cookie-only tail. The router
// is published on a real listener because a WS upgrade needs a hijackable
// connection, which httptest.NewRecorder is not.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// wsCloseNow drops a socket without a close handshake — the test-teardown path.
func wsCloseNow(c *websocket.Conn) { _ = c.CloseNow() }

// wsLiveDBServer publishes router on a loopback listener, closed with the test.
func wsLiveDBServer(t *testing.T, router http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func wsLiveDBURL(srv *httptest.Server, runID uuid.UUID) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws?run=" + runID.String()
}

// wsHandshakeStatus reports the handshake response's status code, or 0 when the dial
// failed before any response existed. Used to say WHICH gate refused an upgrade:
// 401 is authN, 403 is websocket.Accept's origin check, 404 is ServeWS's per-run
// authz — three different mechanisms a bare "dial failed" cannot tell apart.
func wsHandshakeStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// wsHandshakeBody returns the (bounded) failed-handshake body coder/websocket
// preserves for debugging. On a refusal ServeWS wrote it via httpx.Error, so its
// shape distinguishes OUR pre-upgrade JSON error from Accept's plain-text 403.
func wsHandshakeBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return strings.TrimSpace(string(b))
}

// wsReadMarkedFrame publishes a uniquely-marked message frame on a ticker until the
// socket reads one, then returns the decoded Event.
//
// The repeat is load-bearing, not belt-and-braces: ServeWS calls hub.Subscribe a
// beat AFTER the handshake returns to the client, and the hub is live-only by
// design, so a single publish racing that window is broadcast to an empty
// subscriber set and lost forever.
func wsReadMarkedFrame(ctx context.Context, t *testing.T, h *Handler, c *websocket.Conn, runID uuid.UUID, seq int32, marker string) hub.Event {
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
		t.Fatalf("read from an ACCEPTED subscription: %v — the upgrade succeeded but no published frame arrived, so the hub→client wire is broken (not the auth change)", err)
	}
	var ev hub.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("the frame on the wire is not a hub.Event: %v (raw %q)", err, string(data))
	}
	return ev
}

// wsAssertMarkedFrame checks the frame is the one this test published — an identity
// (the marker uuid), not a pattern. Each condition gets its own message so a red
// names exactly what diverged.
func wsAssertMarkedFrame(t *testing.T, ev hub.Event, wantSeq int32, wantMarker string) {
	t.Helper()
	if ev.Type != "message" {
		t.Errorf("frame type = %q, want \"message\"", ev.Type)
	}
	if ev.Seq != wantSeq {
		t.Errorf("frame seq = %d, want %d", ev.Seq, wantSeq)
	}
	var got struct {
		Marker string `json:"marker"`
	}
	if err := json.Unmarshal(ev.Payload, &got); err != nil {
		t.Errorf("frame payload is not the published JSON object: %v (raw %q)", err, string(ev.Payload))
		return
	}
	if got.Marker != wantMarker {
		t.Errorf("frame payload marker = %q, want %q — the socket delivered a frame this test did not publish", got.Marker, wantMarker)
	}
}

// -------------------------------------------------------------------------
// (1) THE ENABLER: a no-Origin Bearer upgrade is accepted AND delivers a published
// frame. Reaching 101 only proves the route admits the credential; reading the frame
// is what proves the thing the TUI needs, so the assertion is on the frame.
//
// No Origin header is sent, and that IS the mechanism: coder/websocket's Dial never
// sets one, and authenticateOrigin returns nil for an empty Origin (v1.8.14
// accept.go:228-232, reached from accept.go:116-117 while InsecureSkipVerify is
// false). AcceptOptions{} is untouched by M1.
// -------------------------------------------------------------------------

func TestWSBearerNoOriginSubscribeReceivesFrameLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	srv := wsLiveDBServer(t, router)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsLiveDBURL(srv, runID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + uzc}},
	})
	if err != nil {
		t.Fatalf("no-Origin Bearer upgrade to the caller's OWN run was refused with handshake status %d: %v (body %q) — status 401 means /ws is still mounted in the cookie-only RequireAuth tail",
			wsHandshakeStatus(resp), err, wsHandshakeBody(resp))
	}
	defer wsCloseNow(c)

	marker := uuid.NewString()
	wsAssertMarkedFrame(t, wsReadMarkedFrame(ctx, t, h, c, runID, 7, marker), 7, marker)
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// -------------------------------------------------------------------------
// (2) The cookie CSWSH defense survives the move: a foreign-Origin cookie upgrade is
// still refused, and refused BY THE ORIGIN CHECK (403) rather than by authN or authz.
//
// The same-origin dial above it is a positive control on the same request shape: it
// proves the cookie path still opens a socket at all after the move, so the 403 below
// is the Origin and not a cookie the router stopped accepting.
// -------------------------------------------------------------------------

func TestWSCookieForeignOriginStillRejectedLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	jwt := cliMintJWT(t, pool, owner)
	srv := wsLiveDBServer(t, router)
	cookie := auth.AuthCookieName + "=" + jwt

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	same, resp, err := websocket.Dial(ctx, wsLiveDBURL(srv, runID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{cookie}, "Origin": []string{srv.URL}},
	})
	if err != nil {
		t.Fatalf("SAME-origin cookie upgrade was refused with handshake status %d: %v (body %q) — the control must pass, else the foreign-Origin refusal below proves nothing",
			wsHandshakeStatus(resp), err, wsHandshakeBody(resp))
	}
	_ = same.Close(websocket.StatusNormalClosure, "")

	// Byte-identical request except the Origin. The value is a variable and the
	// diagnostic PRINTS it rather than repeating it as a literal, so the message cannot
	// outlive the fixture: aiming this dial elsewhere (as a mutation check does) changes
	// what the failure claims instead of leaving it asserting an origin never sent.
	const foreignOrigin = "http://evil.example"
	evil, resp, err := websocket.Dial(ctx, wsLiveDBURL(srv, runID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{cookie}, "Origin": []string{foreignOrigin}},
	})
	if err == nil {
		wsCloseNow(evil)
		t.Fatalf("a cookie upgrade carrying Origin: %s (Host: %s) was ACCEPTED — the same-origin CSWSH defense is gone (InsecureSkipVerify set, or OriginPatterns widened)",
			foreignOrigin, srv.Listener.Addr())
	}
	if got := wsHandshakeStatus(resp); got != http.StatusForbidden {
		t.Fatalf("foreign-Origin cookie upgrade was refused with status %d, want 403 — the refusal must come from websocket.Accept's origin check, not from authN (401) or per-run authz (404)", got)
	}

	// "Origin: null" — a sandboxed iframe or a data: page — must NOT be mistaken for
	// the absent-Origin case the CLI relies on. It parses to a URL with no host and is
	// rejected at accept.go:256-258; if it were treated as empty, the whole CSWSH
	// defense would be one sandbox attribute away from bypass.
	nul, resp, err := websocket.Dial(ctx, wsLiveDBURL(srv, runID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{cookie}, "Origin": []string{"null"}},
	})
	if err == nil {
		wsCloseNow(nul)
		t.Fatal("a cookie upgrade carrying Origin: null was ACCEPTED — a sandboxed iframe would reach the socket; \"null\" must not be treated as the absent-Origin exemption the browser-less client uses")
	}
	if got := wsHandshakeStatus(resp); got != http.StatusForbidden {
		t.Errorf("Origin: null cookie upgrade was refused with status %d, want 403 from the origin check", got)
	}
}

// -------------------------------------------------------------------------
// (3) Owner-or-admin parity with the REST reads: a uzc_ for a run its holder cannot
// see gets the pre-upgrade 404, and the OWNER's token on the SAME run succeeds — so
// the 404 is the authz check, not a missing or unroutable run.
//
// "Pre-upgrade" is asserted on evidence, not on wording: the handshake carries
// ServeWS's own httpx.Error JSON body rather than a 101, and websocket.Accept is only
// reached after GetRunForViewer returns.
// -------------------------------------------------------------------------

func TestWSBearerForeignRunIs404BeforeUpgradeLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	stranger := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	ownerTok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	strangerTok := cliMintToken(t, pool, stranger, clitoken.ScopeUser)
	srv := wsLiveDBServer(t, router)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dial := func(tok string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, wsLiveDBURL(srv, runID), &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
		})
	}

	c, resp, err := dial(strangerTok)
	if err == nil {
		wsCloseNow(c)
		t.Fatal("a uzc_ token subscribed to a run its holder does not own — /api/ws must apply the same owner-or-admin rule the REST reads do")
	}
	if got := wsHandshakeStatus(resp); got != http.StatusNotFound {
		t.Fatalf("non-owner Bearer upgrade was refused with status %d, want 404 — the run's existence must not be distinguishable, and a 403 here would be the origin check refusing instead of authz", got)
	}
	if body := wsHandshakeBody(resp); body != `{"error":"run not found"}` {
		t.Errorf("non-owner refusal body = %q, want %q — the refusal must be ServeWS's own pre-upgrade httpx.Error, written before websocket.Accept ran", body, `{"error":"run not found"}`)
	}

	// Control: the owner's token opens the SAME url and reads a live frame.
	own, resp, err := dial(ownerTok)
	if err != nil {
		t.Fatalf("the OWNER's uzc_ was refused with handshake status %d on the same url: %v (body %q) — then the 404 above is a broken fixture, not the authz check",
			wsHandshakeStatus(resp), err, wsHandshakeBody(resp))
	}
	defer wsCloseNow(own)
	marker := uuid.NewString()
	wsAssertMarkedFrame(t, wsReadMarkedFrame(ctx, t, h, own, runID, 3, marker), 3, marker)
	_ = own.Close(websocket.StatusNormalClosure, "")
}

// -------------------------------------------------------------------------
// (4) Admin read parity with AdminListRuns: a uza_ subscribes to ANOTHER user's run
// and reads a live frame. The same admin's uzc_ is 404 on that run, which is what
// makes the success above specifically the admin_ro scope rather than "any Bearer
// sees any run" — the F1 masking is the difference between the two credentials, and
// it is the only difference here (same user row, same url, same fixture).
// -------------------------------------------------------------------------

func TestWSAdminTokenSubscribesToAnotherUsersRunLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	admin := cliSeedUser(t, pool, true)
	owner := cliSeedUser(t, pool, false)
	runID := cliSeedJudgedRun(t, pool, owner)
	uza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	uzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)
	srv := wsLiveDBServer(t, router)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dial := func(tok string) (*websocket.Conn, *http.Response, error) {
		return websocket.Dial(ctx, wsLiveDBURL(srv, runID), &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + tok}},
		})
	}

	c, resp, err := dial(uza)
	if err != nil {
		t.Fatalf("uza_ upgrade to another user's run was refused with handshake status %d: %v (body %q) — an admin_ro token reads any run over REST and must reach its live channel too",
			wsHandshakeStatus(resp), err, wsHandshakeBody(resp))
	}
	defer wsCloseNow(c)
	marker := uuid.NewString()
	wsAssertMarkedFrame(t, wsReadMarkedFrame(ctx, t, h, c, runID, 11, marker), 11, marker)
	_ = c.Close(websocket.StatusNormalClosure, "")

	masked, resp, err := dial(uzc)
	if err == nil {
		wsCloseNow(masked)
		t.Fatal("the SAME admin's default-scope uzc_ also subscribed to another user's run — the RequireUser scope ceiling (IsAdmin cleared for a non-admin_ro token) is not reaching /api/ws")
	}
	if got := wsHandshakeStatus(resp); got != http.StatusNotFound {
		t.Fatalf("admin's uzc_ upgrade to another user's run was refused with status %d, want 404 (the masking makes it owner-only)", got)
	}
}
