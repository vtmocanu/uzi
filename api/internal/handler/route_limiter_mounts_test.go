package handler

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// This file guards the rate-limiter MOUNTS on the real route table. It exists
// because nothing else could: measured at ad6c63d9, deleting all 24
// `.With(<limiter>.PerUserMiddleware)` mounts from Routes left `go vet` clean and
// `go test ./...` at zero failures. It compiles without them because each limiter
// stays used as a Routes parameter, so a dropped mount is a live behaviour change
// that no build step and no existing test could see. Three mechanisms explain the
// blindness, and each one dictates a rule this file follows:
//
//  1. Handlers are called as plain functions elsewhere (review_issue_livedb_test.go
//     calls h.FileIssue(rr, req) directly), so no middleware chain executes at all.
//     => Here the router comes from h.Routes, and only from h.Routes.
//  2. The only two prior PerUserMiddleware assertions (chat_test.go, slack_test.go)
//     hand-build a chi.NewRouter() and register the very .With(...) they then observe
//     working — tautologies with respect to the mount. Both their routes are in the
//     24 and both suites stayed green through the deletion.
//     => This file registers no route. If you find yourself adding one, the test has
//     become the third tautology.
//  3. chi.Walk appears nowhere else in api/; the tests that do build the real table
//     pass a limiter that cannot fire (noLimit at 1000/min, or 100000/min).
//     => Here every limiter has a budget small enough to fire, and each probe asserts
//     it actually fired.

// limiterNames maps a limiter's POSITION in the Routes(...) parameter list to its
// parameter name. Order is load-bearing: it is how a probed budget is turned back
// into an identity (see newProbeLimiters). Keep it in sync with Routes' signature —
// a mismatch is caught by TestLimiterNamesCoverEveryRoutesLimiterParameter below.
// An ARRAY, not a slice, so len(limiterNames) is a constant and probeCeiling can
// be one too.
var limiterNames = [...]string{
	limAuth,
	limForge,
	limSlackDM,
	limChat,
	limProposal,
	limJudge,
	limHosted,
	limCLIPoll,
	limBoardOrder,
}

// The limiter names, as constants so a typo in the 143-row table below is a compile
// error rather than a failing row. Spelled `lim*` rather than matching the parameter
// names exactly, so nothing here shadows a parameter inside Routes.
//
// 143 as of this commit; it was 142 until PRD #122 M8 added POST
// /api/worker/runs/{id}/publish, and 141 until `c309e8a0` added GET
// /api/admin/cli-tokens.
// THIS SENTENCE IS DOWNSTREAM OF ANY NEW ROUTE — see the note above wantRouteMounts for
// which lines a mount-adder owns.
//
// noLimiter marks a route that carries NO per-user limiter. Spelling that out
// (rather than omitting the route) is what makes an ADDED-without-a-limiter route
// visible: the table enumerates every route in the API, so a new one fails
// TestEveryRouteCarriesItsExpectedPerUserLimiter until someone writes a row for it
// and thereby decides whether it needs a budget.
const (
	noLimiter   = ""
	limAuth     = "authLimiter"
	limForge    = "forgeLimiter"
	limSlackDM  = "slackDMLimiter"
	limChat     = "chatLimiter"
	limProposal = "proposalLimiter"
	limJudge    = "judgeLimiter"
	limHosted   = "hostedLimiter"
	limCLIPoll  = "cliPollLimiter"
	// PRD #102 M5. Dedicated rather than shared with limForge: a reorder makes zero
	// forge calls, so charging it to the forge budget would let a burst of dragging
	// starve the user's real forge operations.
	limBoardOrder = "boardOrderLimiter"
)

type routeMount struct {
	method  string
	pattern string
	limiter string
}

// wantRouteMounts is the complete route table of h.Routes(...) with hosted-worker
// support ON, each row naming the per-user limiter mounted on it. 141 rows / 24
// limiter mounts as of ad6c63d9 — both figures are that tree's inventory and will
// drift; the durable claim is that the list is COMPLETE, which is what
// TestEveryRouteCarriesItsExpectedPerUserLimiter enforces in both directions.
//
// The two rows worth naming, because they are brute-force controls on a passphrase
// endpoint: POST /api/vault/unlock and POST /api/vault/passphrase.
//
// NOTE on authLimiter: it is mounted BOTH ways. Its per-IP Middleware sits on
// /register, /login, /config, the OIDC pair and /cli/start; FOUR routes take its
// PerUserMiddleware — /cli/approve, /vault/unlock, /vault/passphrase and
// /admin/cli-tokens. This table covers the per-user mounts ONLY — the per-IP ones read
// as noLimiter here and are not guarded by this file. e2e/run-e2e.sh asserts a 429 on
// /api/auth/login, which is the per-IP mount and closes none of the 25.
//
// 🔴 WHICH LINES A MOUNT-ADDER OWNS, because two numerals in this paragraph and one at
// the `lim*` constants above were rotted by a single commit — `c309e8a0`, which added
// GET /api/admin/cli-tokens as the FOURTH auth per-user mount and the 142nd table row:
//
//	any new ROUTE                  -> the "143-row table" sentence at the lim* constants
//	a new AUTH per-user mount      -> this paragraph: the four-route list AND the "25"
//
// Both figures are this commit's inventory. THE TABLE BELOW IS THE MECHANISM THAT FAILS
// WHEN THE TREE MOVES; this prose only has to stop lying, which is why it is bound to a
// tip rather than derived — deriving it would put a second parser in the file and give it
// a way to be self-consistently wrong.
//
// The pattern is measured rather than argued, and this file ran the experiment on itself:
// across `c309e8a0`, every SHA-BOUND claim here survived (the `ad6c63d9` figures at :27
// and below) and every UNBOUND one rotted (three of three, each way).
var wantRouteMounts = []routeMount{
	{"DELETE", "/api/agent-templates/{id}", noLimiter},
	{"DELETE", "/api/forge/connections/{id}", noLimiter},
	{"DELETE", "/api/me/cli-tokens/{id}", noLimiter},
	{"DELETE", "/api/me/memory/{id}", noLimiter},
	{"DELETE", "/api/me/secrets/anthropic_token", noLimiter},
	{"DELETE", "/api/me/secrets/anthropic_token/{id}", noLimiter},
	{"DELETE", "/api/runs/{id}/review/recommendations/{recID}/disposition", noLimiter},
	// Schedule delete (PRD #241 M4): owner-scoped DB delete, no forge read → noLimiter.
	{"DELETE", "/api/schedules/{id}", noLimiter},
	{"DELETE", "/api/skills/{id}", noLimiter},
	{"DELETE", "/api/tool-allowlist/{id}", noLimiter},
	{"DELETE", "/api/workers/{id}", limHosted},
	// The one admin read that carries a per-user budget: it enumerates standing
	// credentials, so it rides the credential-surface limiter. Its bucket is keyed by
	// (pattern, user) and is therefore disjoint from the other authLimiter mounts.
	{"GET", "/api/admin/cli-tokens", limAuth},
	{"GET", "/api/admin/rate-limits", noLimiter},
	{"GET", "/api/admin/runs", noLimiter},
	{"GET", "/api/admin/selfimprove", noLimiter},
	{"GET", "/api/admin/settings", noLimiter},
	{"GET", "/api/admin/slack/status", noLimiter},
	{"GET", "/api/admin/usage", noLimiter},
	{"GET", "/api/admin/users", noLimiter},
	{"GET", "/api/admin/vault-migration", noLimiter},
	{"GET", "/api/admin/workers", noLimiter},
	{"GET", "/api/agent-templates/", noLimiter},
	{"GET", "/api/agent-templates/allocations", noLimiter},
	{"GET", "/api/agent-templates/{id}", noLimiter},
	// The shipped builtin definition (issue #201 M4a). noLimiter, like every
	// sibling here: it is a read of data embedded in the binary — no database
	// query beyond the row fetch the neighbours already do, no external call, and
	// it is gated to callers who pass the template WRITE authz.
	{"GET", "/api/agent-templates/{id}/builtin", noLimiter},
	{"GET", "/api/agent-templates/{id}/rendered", noLimiter},
	{"GET", "/api/agent-templates/{id}/skills", noLimiter},
	{"GET", "/api/auth/cli/request/{id}", noLimiter},
	{"GET", "/api/auth/config", noLimiter},
	{"GET", "/api/auth/me", noLimiter},
	{"GET", "/api/auth/oidc/callback", noLimiter},
	{"GET", "/api/auth/oidc/login", noLimiter},
	{"GET", "/api/chats/", noLimiter},
	{"GET", "/api/controller/poll", noLimiter},
	{"GET", "/api/forge/config", noLimiter},
	{"GET", "/api/forge/connections/", noLimiter},
	{"GET", "/api/forge/connections/{id}/projects", limForge},
	{"GET", "/api/health", noLimiter},
	{"GET", "/api/me/cli-tokens/", noLimiter},
	{"GET", "/api/me/judge/recommendations", noLimiter},
	{"GET", "/api/me/judge/stats", noLimiter},
	// noLimiter, same reasoning as /me/judge/stats beside it: an authenticated read that
	// AppShell polls on a fixed interval for every logged-in user. A per-user budget here
	// would throttle the app's own shell — the badge would silently stop updating for a
	// user doing nothing wrong — and the endpoint spends nothing: one indexed
	// user-scoped query, no forge call, no model call (PRD #113 M6).
	{"GET", "/api/me/workers/upgrade-summary", noLimiter},
	// noLimiter, same class and reasoning as the two badge-count reads above
	// (/me/judge/stats, /me/workers/upgrade-summary): AppShell polls it for every
	// logged-in user to feed the Runs nav badge (PRD #239), so a per-user budget would
	// throttle the app's own shell; it spends nothing — one indexed user-scoped
	// count(*), no forge or model call.
	{"GET", "/api/me/runs/in-progress-count", noLimiter},
	// Schedule list/get (PRD #241 M4): owner-scoped reads, no forge → noLimiter.
	{"GET", "/api/me/schedules/", noLimiter},
	{"GET", "/api/schedules/{id}", noLimiter},
	{"GET", "/api/me/memory/", noLimiter},
	{"GET", "/api/me/rate-limits", noLimiter},
	{"GET", "/api/me/secrets/", noLimiter},
	{"GET", "/api/me/settings/", noLimiter},
	{"GET", "/api/me/slack/", noLimiter},
	{"GET", "/api/notifications/", noLimiter},
	{"GET", "/api/notifications/unread_count", noLimiter},
	{"GET", "/api/repos/", noLimiter},
	{"GET", "/api/repos/{id}/board", noLimiter},
	{"GET", "/api/repos/{id}/board/prefs", noLimiter},
	{"GET", "/api/repos/{id}/issues/{iid}", limForge},
	{"GET", "/api/repos/{id}/tool-profile", noLimiter},
	{"GET", "/api/runs/", noLimiter},
	{"GET", "/api/runs/{id}", noLimiter},
	{"GET", "/api/runs/{id}/inputs", noLimiter},
	{"GET", "/api/runs/{id}/messages", noLimiter},
	{"GET", "/api/runs/{id}/review", noLimiter},
	{"GET", "/api/runs/{id}/review/recommendations/{recID}/issue-draft", noLimiter},
	{"GET", "/api/skills/", noLimiter},
	{"GET", "/api/skills/{id}", noLimiter},
	{"GET", "/api/tool-allowlist/", noLimiter},
	{"GET", "/api/usage", noLimiter},
	{"GET", "/api/vault/status", noLimiter},
	{"GET", "/api/version", noLimiter},
	{"GET", "/api/worker/chat/runs", noLimiter},
	{"GET", "/api/worker/chat/runs/{id}", noLimiter},
	{"GET", "/api/worker/chat/runs/{id}/messages", noLimiter},
	{"GET", "/api/worker/runs/{id}/inputs", noLimiter},
	{"GET", "/api/worker/runs/{id}/memory", noLimiter},
	{"GET", "/api/worker/runs/{id}/trace", noLimiter},
	{"GET", "/api/workers/", noLimiter},
	{"GET", "/api/workers/hosted/config", noLimiter},
	{"GET", "/api/ws", noLimiter},
	{"PATCH", "/api/admin/users/{id}", noLimiter},
	{"PATCH", "/api/me/secrets/anthropic_token/{id}", noLimiter},
	// The auto-selection pool toggle (PRD #111 M2, D13). noLimiter, and the choice
	// is deliberate rather than inherited: the credential-surface limiter (limAuth)
	// exists for routes that MINT, REVEAL or ENUMERATE standing credentials, and
	// this does none of those — it flips a boolean on a row the caller already owns.
	// Abusing it costs the attacker a write and gains them nothing they could not
	// already do with the cookie-only PATCH beside it. It sits under RequireUser
	// like PATCH /api/workers/{id} below, which is the same class and is likewise
	// unlimited.
	{"PATCH", "/api/me/secrets/anthropic_token/{id}/auto-eligible", noLimiter},
	{"PATCH", "/api/repos/{id}", noLimiter},
	// Schedule edit (PRD #241 M4): owner-scoped DB update, no forge → noLimiter.
	{"PATCH", "/api/schedules/{id}", noLimiter},
	{"PATCH", "/api/workers/{id}", noLimiter},
	{"POST", "/api/agent-templates/", noLimiter},
	{"POST", "/api/agent-templates/{id}/reset", noLimiter},
	{"POST", "/api/auth/cli/approve", limAuth},
	{"POST", "/api/auth/cli/deny", noLimiter},
	{"POST", "/api/auth/cli/poll", noLimiter},
	{"POST", "/api/auth/cli/start", noLimiter},
	{"POST", "/api/auth/login", noLimiter},
	{"POST", "/api/auth/logout", noLimiter},
	{"POST", "/api/auth/register", noLimiter},
	{"POST", "/api/chats/", limChat},
	{"POST", "/api/chats/{id}/continue", limChat},
	{"POST", "/api/chats/{id}/end", noLimiter},
	{"POST", "/api/chats/{id}/messages", limChat},
	// noLimiter, and this is a reasoned choice rather than the default (PRD #113 M4).
	// The per-user limiters key on the authenticated USER, and a controller request has
	// no user in context — it carries the fleet-scoped bearer credential, so
	// PerUserMiddleware cannot apply at all rather than being merely unhelpful. What is
	// left would be a per-IP budget, which is meaningless here: there is exactly one
	// controller principal, on a fixed ~10s cadence, dialing from a pod CIDR that is a
	// trusted proxy by construction. The real bounds on this endpoint are the ones that
	// fit it — a 1 MiB body cap and an explicit 512-entry cap — not a request rate.
	// Same reasoning, same answer as GET /api/controller/poll above.
	{"POST", "/api/controller/status", noLimiter},
	{"POST", "/api/chats/{id}/proposals/{pid}/confirm", limForge},
	{"POST", "/api/chats/{id}/proposals/{pid}/dismiss", noLimiter},
	{"POST", "/api/forge/connections/", noLimiter},
	{"POST", "/api/forge/connections/{id}/privilege-check", limForge},
	{"POST", "/api/forge/connections/{id}/verify", limForge},
	{"POST", "/api/me/cli-tokens/", noLimiter},
	{"POST", "/api/me/cli-tokens/revoke-all", noLimiter},
	{"POST", "/api/me/secrets/anthropic_token", noLimiter},
	{"POST", "/api/me/slack/test-dm", limSlackDM},
	{"POST", "/api/notifications/{id}/read", noLimiter},
	{"POST", "/api/repos/{id}/ci-fix-runs", limForge},
	{"POST", "/api/repos/{id}/issues", limForge},
	{"POST", "/api/repos/{id}/issues/{iid}/move", limForge},
	{"POST", "/api/repos/{id}/issues/{iid}/prdless", limForge},
	// Promote (PRD #102 Decision 15) writes one label to the forge, the same shape
	// and the same cost as the prdless toggle above it.
	{"POST", "/api/repos/{id}/issues/{iid}/promote", limForge},
	{"POST", "/api/repos/{id}/runs", limForge},
	// Schedule create (PRD #241 M4): validates config + computes next_fire, no forge
	// read → noLimiter (unlike run-now, which fires through the seam).
	{"POST", "/api/repos/{id}/schedules", noLimiter},
	// Schedule preview (PRD #241 M4): pure next-fire computation, no forge → noLimiter.
	{"POST", "/api/schedules/preview", noLimiter},
	// Run-now (PRD #241 M4): fires through the seam (forge GetIssue), so it carries the
	// per-user forge limiter, matching CreateRun's posture.
	{"POST", "/api/schedules/{id}/run-now", limForge},
	{"POST", "/api/repos/{id}/sync", limForge},
	{"POST", "/api/runs/{id}/inputs", noLimiter},
	{"POST", "/api/runs/{id}/rejudge", limJudge},
	{"POST", "/api/runs/{id}/review/recommendations/{recID}/issue", limForge},
	{"POST", "/api/skills/", noLimiter},
	{"POST", "/api/skills/{id}/reset", noLimiter},
	{"POST", "/api/tool-allowlist/", noLimiter},
	{"POST", "/api/vault/lock", noLimiter},
	{"POST", "/api/vault/passphrase", limAuth},
	{"POST", "/api/vault/unlock", limAuth},
	{"POST", "/api/worker/heartbeat", noLimiter},
	{"POST", "/api/worker/register", noLimiter},
	{"POST", "/api/worker/runs/claim", noLimiter},
	{"POST", "/api/worker/runs/{id}/memory", noLimiter},
	{"POST", "/api/worker/runs/{id}/messages", noLimiter},
	{"POST", "/api/worker/runs/{id}/proposals", noLimiter},
	{"POST", "/api/worker/runs/{id}/publish", noLimiter},
	{"POST", "/api/worker/runs/{id}/review", noLimiter},
	{"POST", "/api/worker/runs/{id}/state", noLimiter},
	{"POST", "/api/workers/", noLimiter},
	{"POST", "/api/workers/hosted", limHosted},
	{"PUT", "/api/admin/selfimprove", noLimiter},
	{"PUT", "/api/admin/settings", noLimiter},
	{"PUT", "/api/admin/users/{id}/judge", noLimiter},
	{"PUT", "/api/agent-templates/allocations", noLimiter},
	{"PUT", "/api/agent-templates/{id}", noLimiter},
	{"PUT", "/api/agent-templates/{id}/skills", noLimiter},
	{"PUT", "/api/forge/connections/{id}", limForge},
	{"PUT", "/api/me/autopilot", noLimiter},
	// PRD #35 Decision 7. noLimiter, and the choice is deliberate rather than
	// inherited from the neighbours: this is a single boolean UPDATE on the caller's
	// own users row. It spends no Anthropic token, makes no forge call, sends no DM,
	// and mints nothing — none of the three things the per-user limiters exist to
	// bound. Cookie+CSRF already gates it, matching /me/autopilot beside it.
	{"PUT", "/api/me/wait-on-limit", noLimiter},
	{"PUT", "/api/me/judge/recommendations/disposition", noLimiter},
	{"PUT", "/api/me/secrets/anthropic_token", noLimiter},
	{"PUT", "/api/me/settings/", noLimiter},
	{"PUT", "/api/me/slack/notify", noLimiter},
	{"PUT", "/api/me/slack/override", limSlackDM},
	{"PUT", "/api/repos/{id}", noLimiter},
	{"PUT", "/api/repos/{id}/board/columns", noLimiter},
	// Its OWN limiter (PRD #102 M5), not limForge and not none. The route makes zero
	// forge calls, so the forge budget would be the wrong pocket; but it renumbers a
	// whole board in a transaction and rebuilds the board on every request, so bare
	// was the wrong answer too — and "give it its own" is what this codebase has
	// already decided five times over.
	//
	// An earlier version of this comment justified it with "every other board WRITE
	// route carries limForge". That is false in both directions and was struck: the row
	// directly above is PUT board/columns with noLimiter, and ConfigureColumns DOES make
	// forge calls (ForgeForConnection then EnsureLabels). The decision stands on its own
	// merits above; it never needed that claim.
	{"PUT", "/api/repos/{id}/board/order", limBoardOrder},
	{"PUT", "/api/repos/{id}/board/prefs", noLimiter},
	{"PUT", "/api/repos/{id}/tool-profile", noLimiter},
	// PRD #35 Decision 7, the per-run toggle. noLimiter for the same reason as
	// /me/wait-on-limit: one owner-scoped boolean UPDATE, no spend, no forge write,
	// and it cannot even change the run's status. Contrast /runs/{id}/rejudge above,
	// which carries limJudge precisely because it MINTS a token-spending run.
	{"PUT", "/api/runs/{id}/wait-on-limit", noLimiter},
	{"PUT", "/api/runs/{id}/review/recommendations/{recID}/disposition", noLimiter},
	{"PUT", "/api/skills/{id}", noLimiter},
	{"PUT", "/api/tool-allowlist/{id}", noLimiter},
}

// newProbeLimiters builds one limiter per Routes parameter, each with a DISTINCT
// budget equal to its 1-based position. The budget is the identity channel: a probe
// that gets exactly k requests through before a 429 has found limiter k, so this
// test reddens when a route carries the WRONG limiter, not merely when it carries
// none.
//
// Function identity could not do that, and the precise boundary is measured rather
// than assumed: a reflect pointer compare sees the METHOD but not the RECEIVER.
// Measured — PerUserMiddleware's pointer differs from both PerWorkerMiddleware's and
// the IP-keyed Middleware's, so reflect can tell those three apart; but two limiters
// with different receivers share one PerUserMiddleware pointer, and comparing every
// walked middleware against reflect.ValueOf(ls[0].PerUserMiddleware).Pointer() tagged
// all 24 mounts equal though they carry SIX different instances. A bound-method
// value's code pointer is the receiver-independent wrapper and the receiver is not
// reachable through the reflect API. So reflect could have named the method; only
// driving the middleware names the instance.
func newProbeLimiters() []*mw.Limiter {
	ls := make([]*mw.Limiter, len(limiterNames))
	for i := range ls {
		// An hour-long window: no bucket may reset in the middle of a probe.
		ls[i] = mw.NewLimiter(i+1, time.Hour, nil)
	}
	return ls
}

// probeSentinel is the status the probe's innermost handler writes. It must be a
// code no middleware in this router produces, so "the request reached the end of
// the chain" is never confused with "a middleware answered it".
const probeSentinel = http.StatusTeapot

// probeCeiling bounds the requests one probe sends. Above the largest budget
// newProbeLimiters hands out, so every one of our limiters fires inside it.
const probeCeiling = len(limiterNames) + 2

// prober numbers probes so each gets a private limiter bucket. Not safe for
// concurrent use; the tests here are sequential.
type prober struct{ n int }

// probe drives ONE middleware in isolation and reports the budget it enforces
// (0 = not a rate limiter) and whether it keys per-user.
//
// Isolation is deliberate: driving the whole chain would need a valid session
// cookie, a CSRF token and a live store for every route in the table, and a route
// that 401s or 404s BEFORE reaching the limiter would then hand back a green that
// proves nothing. Driving the middleware alone means a probe that fails to reach
// the limiter cannot be mistaken for a probe that reached it and passed: every
// classification here is backed by an observed 429, or by no 429 at all.
func (p *prober) probe(m func(http.Handler) http.Handler) (budget int, perUser bool) {
	p.n++
	// Unique to this probe, so no two probes can share a bucket in any keying.
	path := fmt.Sprintf("/__limiter-probe__/%d", p.n)
	first := store.User{ID: uuid.New()}
	second := store.User{ID: uuid.New()}

	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(probeSentinel)
	})
	chain := m(reached)

	// The probe request carries no cookie and no Authorization header, so every auth
	// middleware in this router rejects it on its first branch, before it touches the
	// (nil) store — which is what makes it safe to drive them all. A middleware that
	// grew a nil-dereference on that path would panic here rather than fail quietly.
	call := func(user store.User) int {
		rctx := chi.NewRouteContext()
		rctx.RoutePatterns = []string{path}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req.WithContext(mw.ContextWithUser(ctx, user)))
		return rec.Code
	}

	for i := 0; i < probeCeiling; i++ {
		switch call(first) {
		case probeSentinel:
			budget++
		case http.StatusTooManyRequests:
			if budget == 0 {
				return 0, false // rejected before it ever passed: not one of ours
			}
			// The discriminator between the keyings, and the reason a mount can be
			// named rather than merely detected. PerUserMiddleware keys on (route
			// pattern, user id), so a second user on the same pattern, path and IP gets
			// a fresh bucket and is let through. Middleware keys on (path, client IP),
			// and PerWorkerMiddleware falls back to the client IP when no worker is in
			// context — under both, the second user is still over budget.
			return budget, call(second) == probeSentinel
		default:
			return 0, false // an auth or CSRF middleware: it answers every probe request
		}
	}
	return 0, false // never rejected inside the ceiling: not one of our limiters
}

// perUserLimiterOn names the per-user limiter mounted on one route's middleware
// chain, or noLimiter if there is none.
func (p *prober) perUserLimiterOn(mws []func(http.Handler) http.Handler) (string, error) {
	found := noLimiter
	for _, m := range mws {
		budget, perUser := p.probe(m)
		if budget == 0 || !perUser {
			continue
		}
		if budget > len(limiterNames) {
			return "", fmt.Errorf("probed a per-user budget of %d, above the %d limiters "+
				"newProbeLimiters hands out — the probe has lost track of identity", budget, len(limiterNames))
		}
		name := limiterNames[budget-1]
		if found != noLimiter {
			return "", fmt.Errorf("two per-user limiters mounted (%s and %s); this test assumes at most one", found, name)
		}
		found = name
	}
	return found, nil
}

func describeLimiter(name string) string {
	if name == noLimiter {
		return "noLimiter"
	}
	return name
}

// TestEveryRouteCarriesItsExpectedPerUserLimiter walks the route table built by the
// real Routes constructor and pins, for every route, which per-user limiter is
// mounted on it.
//
// It guards BOTH directions, which is the difference between catching regressions
// and catching omissions:
//   - a route that LOSES its .With(<limiter>.PerUserMiddleware) reddens by name;
//   - a route that carries the WRONG limiter reddens with both names;
//   - a route ADDED without a limiter reddens as "not in wantRouteMounts", because
//     the table enumerates the whole API rather than only the limited routes;
//   - a route removed or renamed reddens as listed-but-absent.
//
// What it does NOT pin: the per-IP authLimiter.Middleware mounts (see the note on
// wantRouteMounts), and the limiters' real production budgets (this test substitutes
// its own).
//
// It DOES catch the most plausible maintenance error of all, which is worth naming
// because it looks like a fix rather than a regression: swapping a route's
// PerUserMiddleware for the same limiter's IP-keyed Middleware. Measured on
// /repos/{id}/sync — `forgeLimiter.PerUserMiddleware` -> `forgeLimiter.Middleware`
// compiles, keeps a limiter on the route, and still reddens here with "per-user
// limiter is noLimiter, want forgeLimiter", because the probe's second-user
// discriminator refuses to credit an IP-keyed bucket as per-user protection. A
// reader who assumed "some limiter is present" was the property would have called
// that a false positive; it is the point.
//
// On REORDERS, measured rather than reasoned — an earlier version of this comment
// claimed this test was blind to all of them, and that was wrong:
//   - reordering Routes' PARAMETERS does redden it (16 routes report the wrong
//     limiter), because this test binds budget to name positionally while the body
//     binds name to behaviour, so the two disagree. It reads as sixteen broken
//     mounts rather than one renamed parameter, which is why
//     TestRoutesLimiterParametersAreNamedInLimiterNamesOrder also exists.
//   - reordering main's ARGUMENTS is genuinely invisible here, because this test
//     builds its own call with its own ordering and never reads main's.
//     TestRoutesCallSitePassesLimitersInLimiterNamesOrder covers that one.
func TestEveryRouteCarriesItsExpectedPerUserLimiter(t *testing.T) {
	limiters := newProbeLimiters()
	// Hosting on, so the controller routes exist and the table is unconditional.
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}
	router := h.Routes(limiters[0], limiters[1], limiters[2], limiters[3],
		limiters[4], limiters[5], limiters[6], limiters[7], limiters[8])

	routes, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("Routes returned %T, which does not implement chi.Routes — the walk cannot run", router)
	}

	want := make(map[string]string, len(wantRouteMounts))
	for _, m := range wantRouteMounts {
		key := m.method + " " + m.pattern
		if _, dup := want[key]; dup {
			t.Fatalf("wantRouteMounts lists %s twice", key)
		}
		want[key] = m.limiter
	}

	p := &prober{}
	seen := make(map[string]bool, len(want))
	if err := chi.Walk(routes, func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := method + " " + pattern
		seen[key] = true

		got, err := p.perUserLimiterOn(mws)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			return nil
		}
		expected, listed := want[key]
		if !listed {
			t.Errorf("route %s is not listed in wantRouteMounts (it currently mounts %s). "+
				"Add a row for it, choosing the per-user limiter it needs or noLimiter if it "+
				"deliberately needs none — that decision is the point of this test.",
				key, describeLimiter(got))
			return nil
		}
		if got != expected {
			t.Errorf("route %s: per-user limiter is %s, want %s",
				key, describeLimiter(got), describeLimiter(expected))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk the route table: %v", err)
	}

	missing := make([]string, 0)
	for key := range want {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		t.Errorf("route %s is listed in wantRouteMounts but is not in the router — "+
			"it was removed or renamed; update the table.", key)
	}
}

// TestLimiterProbeTellsTheMountKindsApart is the positive control on the
// instrument. Without it, the file above could go green for the wrong reason: a
// probe that classified everything as noLimiter would still redden 24 rows, but a
// probe whose per-user discriminator was inverted, or which mistook the IP-keyed
// Middleware for the per-user one, would pass quietly on a tree where the mounts
// had been swapped. These cases pin exactly what probe can distinguish.
func TestLimiterProbeTellsTheMountKindsApart(t *testing.T) {
	const budget = 3
	l := mw.NewLimiter(budget, time.Hour, nil)
	passThrough := func(next http.Handler) http.Handler { return next }
	rejectAll := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
	}

	// One prober for the whole test: it is what keeps each subtest on its own
	// bucket in the shared limiter.
	p := &prober{}
	for _, tc := range []struct {
		name        string
		mw          func(http.Handler) http.Handler
		wantBudget  int
		wantPerUser bool
	}{
		{"PerUserMiddleware is keyed per user", l.PerUserMiddleware, budget, true},
		{"Middleware is keyed per IP", l.Middleware, budget, false},
		{"PerWorkerMiddleware falls back to the IP with no worker in context", l.PerWorkerMiddleware, budget, false},
		{"a pass-through middleware enforces nothing", passThrough, 0, false},
		{"a middleware that answers every request is not a limiter", rejectAll, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotBudget, gotPerUser := p.probe(tc.mw)
			if gotBudget != tc.wantBudget || gotPerUser != tc.wantPerUser {
				t.Errorf("probe = (budget %d, perUser %v), want (budget %d, perUser %v)",
					gotBudget, gotPerUser, tc.wantBudget, tc.wantPerUser)
			}
		})
	}
}

// TestLimiterNamesCoverEveryRoutesLimiterParameter guards the position-to-name
// mapping that makes a probed budget readable as an identity, at RUNTIME: adding or
// dropping a limiter parameter shifts every position after it, silently relabelling
// mounts, and this fails instead.
//
// reflect cannot see a REORDER of two existing parameters: it knows their types, not
// their names. A source parse can, so the two order checks live next door
// (TestRoutesLimiterParametersAreNamedInLimiterNamesOrder for the signature,
// TestRoutesCallSitePassesLimitersInLimiterNamesOrder for main's arguments) rather
// than being left as a declared gap.
//
// This one is still worth keeping alongside them: it pins count and type as the
// RUNNING program sees them, where the parses read source text. A disagreement
// between the two would mean a parse had matched something other than the Routes
// this package actually calls — which is the failure mode a source-reading test has
// and a reflect-reading test does not.
func TestLimiterNamesCoverEveryRoutesLimiterParameter(t *testing.T) {
	sig := reflect.TypeOf((*Handler).Routes)
	gotParams := sig.NumIn() - 1 // less the receiver
	if gotParams != len(limiterNames) {
		t.Fatalf("Routes takes %d limiters but limiterNames has %d entries; the "+
			"position-to-name mapping is wrong and every probed identity with it", gotParams, len(limiterNames))
	}
	limiterType := reflect.TypeOf((*mw.Limiter)(nil))
	for i := 1; i < sig.NumIn(); i++ {
		if sig.In(i) != limiterType {
			t.Errorf("Routes parameter %d is %s, want %s", i, sig.In(i), limiterType)
		}
	}
}

// routesLimiterParamNames parses this package's non-test source and returns the
// names of Routes' *mw.Limiter parameters in declaration order.
//
// It scans the directory rather than naming handler.go, so moving Routes to another
// file in the package does not redden this test for the wrong reason. It fails when
// it finds no Routes at all: a parse that matched nothing would otherwise return an
// empty list and compare equal to nothing, which is the shape of a check that cannot
// fail.
func routesLimiterParamNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".") // go test runs with the package dir as cwd
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var decl *ast.FuncDecl
	var declIn string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Routes" {
				continue
			}
			if decl != nil {
				t.Fatalf("found a Routes method in both %s and %s; this test does not know which one runs", declIn, name)
			}
			decl, declIn = fn, name
		}
	}
	if decl == nil {
		t.Fatal("parsed this package's source and found no Routes method — the check matched nothing, which is a failure and not a pass")
	}

	var names []string
	for _, field := range decl.Type.Params.List {
		// Grouped parameters (`a, b, c *mw.Limiter`) are ONE Field carrying several
		// Names, which is exactly how Routes declares its limiters — so the names must
		// come from Field.Names, not from one name per Field.
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Limiter" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "mw" {
			continue
		}
		for _, n := range field.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// TestRoutesLimiterParametersAreNamedInLimiterNamesOrder pins the SIGNATURE half of
// the position-to-name mapping: Routes' limiter parameters, by name, in order.
//
// Swapping two of them COMPILES — both names stay declared and the body still
// references both — so no build step sees it. The walk test does redden (measured:
// 16 routes report the wrong limiter), because it binds budget to name positionally
// while the body binds name to behaviour, and the two then disagree. But it reddens
// as a spray of route mismatches that reads like sixteen broken mounts. This test
// names the actual cause in two lines, which is the difference between a failure you
// can act on and one you have to diagnose.
//
// It is one of a PAIR. See TestRoutesCallSitePassesLimitersInLimiterNamesOrder for
// the half that the walk test genuinely cannot see.
func TestRoutesLimiterParametersAreNamedInLimiterNamesOrder(t *testing.T) {
	got := routesLimiterParamNames(t)
	want := limiterNames[:]
	if len(got) != len(want) {
		t.Fatalf("Routes declares %d *mw.Limiter parameters %v, limiterNames has %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Routes parameter %d is named %q but limiterNames[%d] is %q — "+
				"a probed budget of %d would be reported as %q while the mount actually "+
				"uses %q. Reorder limiterNames to match the signature, or undo the signature change.",
				i+1, got[i], i, want[i], i+1, want[i], got[i])
		}
	}
}

// routesCallSiteArgNames parses the server's main package and returns the
// identifiers passed to h.Routes(...), in argument order.
//
// It fails rather than returning nothing when the call cannot be found or when an
// argument is not a plain identifier: a parse that silently matched nothing would
// compare equal to nothing and pass.
func routesCallSiteArgNames(t *testing.T) []string {
	t.Helper()
	// Relative to this package's directory, which is go test's cwd. If main moves,
	// this fails loudly and someone repoints it — the alternative is a check that
	// quietly stops checking.
	const mainPath = "../../cmd/server/main.go"
	file, err := parser.ParseFile(token.NewFileSet(), mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}

	var calls []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Routes" {
			calls = append(calls, call)
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("found %d .Routes(...) calls in %s, want exactly 1 — this test no longer knows which call wires the server", len(calls), mainPath)
	}

	names := make([]string, 0, len(calls[0].Args))
	for i, arg := range calls[0].Args {
		id, ok := arg.(*ast.Ident)
		if !ok {
			t.Fatalf("%s: argument %d to Routes is not a plain identifier (%T); this test can only read named variables", mainPath, i+1, arg)
		}
		names = append(names, id.Name)
	}
	return names
}

// TestRoutesCallSitePassesLimitersInLimiterNamesOrder pins the half that nothing
// else reaches: the order main passes its limiter variables in.
//
// This is the hole worth having. MEASURED: swapping the first two arguments at
// main.go's call site, leaving the signature untouched, compiles and leaves the
// ENTIRE api suite green — exit 0, 41 packages ok, zero failures — while forge
// routes run on the auth budget and vice versa in production. Every other check here
// is blind to it by construction: the walk test builds its own call with its own
// ordering, reflect sees the signature, and the signature parse sees the signature.
// Only the call site says which real limiter lands in which slot.
//
// Note what this pins and what it does not: that the ARGUMENT NAMES are in
// limiterNames order. It does not verify that the variable called forgeLimiter was
// constructed with the forge budget — that binding is main's own, a few lines up,
// and no test here reads it.
func TestRoutesCallSitePassesLimitersInLimiterNamesOrder(t *testing.T) {
	got := routesCallSiteArgNames(t)
	want := limiterNames[:]
	if len(got) != len(want) {
		t.Fatalf("main passes %d arguments to Routes %v, limiterNames has %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("main passes %q as Routes argument %d, but limiterNames[%d] is %q — "+
				"the running server mounts %q where every mount in this package's table says %q.",
				got[i], i+1, i, want[i], got[i], want[i])
		}
	}
}

// limiterConfigFields declares which config field each limiter is CONSTRUCTED from.
// It is the third and last link in the chain, and the one that reaches furthest from
// this package: limiterNames pins the parameter and argument NAMES, and this pins
// what the name was built out of.
//
// AN EXPLICIT TABLE, NOT A NAME-STEM CONVENTION, and that choice was measured rather
// than assumed — see TestEachLimiterIsBuiltFromItsOwnConfigField for the experiment
// and the numbers. Seven of the eight do follow the stem convention
// (forgeLimiter <- ForgeRateLimitMax), authLimiter is the standing exception
// (RateLimitMax, no stem), and a convention with a permanent exception is one rename
// away from needing a second.
var limiterConfigFields = map[string]string{
	limAuth:       "RateLimitMax",
	limForge:      "ForgeRateLimitMax",
	limSlackDM:    "SlackDMRateLimitMax",
	limChat:       "ChatRateLimitMax",
	limProposal:   "ProposalRateLimitMax",
	limJudge:      "JudgeRateLimitMax",
	limHosted:     "HostedRateLimitMax",
	limCLIPoll:    "CLIPollRateLimitMax",
	limBoardOrder: "BoardOrderRateLimitMax",
}

// limiterConstruction is one `x := mw.NewLimiter(cfg.Y, …)` found in main.
type limiterConstruction struct {
	variable    string
	configField string
}

// parseLimiterConstructions reads main's limiter constructions: the variable name on
// the left and the cfg field supplying its BUDGET (the first argument) on the right.
//
// A construction whose budget argument is not a plain cfg.Field is reported rather
// than skipped — skipping is how a parse quietly stops covering the thing it names.
func parseLimiterConstructions(t *testing.T) []limiterConstruction {
	t.Helper()
	const mainPath = "../../cmd/server/main.go"
	file, err := parser.ParseFile(token.NewFileSet(), mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}

	var out []limiterConstruction
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fn.Sel.Name != "NewLimiter" {
			return true
		}
		if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "mw" {
			return true
		}
		lhs, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			t.Errorf("%s: an mw.NewLimiter result is assigned to something other than a plain identifier (%T)", mainPath, assign.Lhs[0])
			return true
		}
		if len(call.Args) == 0 {
			t.Errorf("%s: %s is built by an mw.NewLimiter call with no arguments", mainPath, lhs.Name)
			return true
		}
		sel, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			t.Errorf("%s: %s's budget argument is %T, not a cfg field; this test can only read cfg.Field", mainPath, lhs.Name, call.Args[0])
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "cfg" {
			t.Errorf("%s: %s's budget argument does not come from cfg", mainPath, lhs.Name)
			return true
		}
		out = append(out, limiterConstruction{variable: lhs.Name, configField: sel.Sel.Name})
		return true
	})
	return out
}

// TestEachLimiterIsBuiltFromItsOwnConfigField closes the last link: that the variable
// NAMED forgeLimiter is CONSTRUCTED from the forge budget. Without it,
// `forgeLimiter := mw.NewLimiter(cfg.ChatRateLimitMax, …)` — an ordinary copy-paste
// slip — is invisible everywhere: it compiles, every mount test stays green because
// the name and the position are both still right, and forge routes silently run on
// the chat budget.
//
// WHY A DECLARED TABLE RATHER THAN A NAME-STEM RULE. Both were built and run; this
// is the measurement, not a preference.
//
// A stem rule ("variable fooLimiter must take a cfg field starting Foo") needs a
// standing exception for authLimiter, which takes RateLimitMax and has no stem. Two
// things were then measured against it:
//   - Case-SENSITIVE, it needs a SECOND exception on the unmodified tree:
//     cliPollLimiter takes cfg.CLIPollRateLimitMax, and a naive capitalisation of the
//     variable stem yields "CliPoll", which is not a prefix of "CLIPoll". Go
//     initialisms break the transform before any refactor happens.
//   - Case-INSENSITIVE — the strongest form, and green on the unmodified tree — it
//     still reddens on a LEGITIMATE rename. Renaming forgeLimiter to
//     forgeProxyLimiter keeps the binding perfectly correct, and the rule reports
//     "cfg.ForgeRateLimitMax does not start with forgeproxy". A correct refactor
//     would have to buy an exception.
//
// The same rename under THIS table is green, and costs zero edits here — the map is
// keyed by the limForge CONSTANT, so a rename flows through the one constant edit
// that limiterNames already requires, while the config field it declares is
// unchanged because the config field genuinely did not change. That is the whole
// argument: an exact declaration tracks what actually moved, and never argues with a
// name.
//
// WHAT THIS CHAIN DOES AND DOES NOT PIN, stated because "the budget is verified" is
// the wrong conclusion to draw from three green tests:
//   - PINNED: the parameter names (signature parse), the argument names at main's
//     call site, and the cfg field each variable's budget is read from.
//   - NOT PINNED, and this is the honest end of the chain: that the forge budget is
//     the RIGHT budget for forge routes. cfg.ForgeRateLimitMax could be set to 1 or
//     to 10000 and every test here stays green. Nothing mechanical can answer that
//     question; it is a product judgement, and the chain stops at the name.
func TestEachLimiterIsBuiltFromItsOwnConfigField(t *testing.T) {
	got := parseLimiterConstructions(t)

	byVar := make(map[string]string, len(got))
	for _, c := range got {
		if prev, dup := byVar[c.variable]; dup {
			t.Errorf("main builds %s twice (from %s and %s)", c.variable, prev, c.configField)
			continue
		}
		byVar[c.variable] = c.configField
	}

	// The declaration must cover exactly the limiters the rest of this file knows
	// about, so the table cannot drift away from limiterNames unnoticed.
	if len(limiterConfigFields) != len(limiterNames) {
		t.Fatalf("limiterConfigFields declares %d limiters, limiterNames has %d", len(limiterConfigFields), len(limiterNames))
	}
	for _, name := range limiterNames {
		want, declared := limiterConfigFields[name]
		if !declared {
			t.Errorf("limiterConfigFields has no entry for %s", name)
			continue
		}
		field, built := byVar[name]
		if !built {
			t.Errorf("main does not build %s with mw.NewLimiter — either it moved, or this parse "+
				"has stopped seeing it; a missing construction is a failure here, not a skip", name)
			continue
		}
		if field != want {
			t.Errorf("main builds %s from cfg.%s, but limiterConfigFields declares cfg.%s — "+
				"the routes that mount %s would run on the wrong budget",
				name, field, want, name)
		}
	}
}
