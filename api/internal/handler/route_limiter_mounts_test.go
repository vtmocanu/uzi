package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
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
}

// The limiter names, as constants so a typo in the 141-row table below is a compile
// error rather than a failing row. Spelled `lim*` rather than matching the parameter
// names exactly, so nothing here shadows a parameter inside Routes.
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
// /register, /login, /config, the OIDC pair and /cli/start; only /cli/approve,
// /vault/unlock and /vault/passphrase take its PerUserMiddleware. This table covers
// the per-user mounts ONLY — the per-IP ones read as noLimiter here and are not
// guarded by this file. e2e/run-e2e.sh asserts a 429 on /api/auth/login, which is
// the per-IP mount and closes none of the 24.
var wantRouteMounts = []routeMount{
	{"DELETE", "/api/agent-templates/{id}", noLimiter},
	{"DELETE", "/api/forge/connections/{id}", noLimiter},
	{"DELETE", "/api/me/cli-tokens/{id}", noLimiter},
	{"DELETE", "/api/me/memory/{id}", noLimiter},
	{"DELETE", "/api/me/secrets/anthropic_token", noLimiter},
	{"DELETE", "/api/me/secrets/anthropic_token/{id}", noLimiter},
	{"DELETE", "/api/runs/{id}/review/recommendations/{recID}/disposition", noLimiter},
	{"DELETE", "/api/skills/{id}", noLimiter},
	{"DELETE", "/api/tool-allowlist/{id}", noLimiter},
	{"DELETE", "/api/workers/{id}", limHosted},
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
	{"GET", "/api/me/memory/", noLimiter},
	{"GET", "/api/me/rate-limits", noLimiter},
	{"GET", "/api/me/secrets/", noLimiter},
	{"GET", "/api/me/settings/", noLimiter},
	{"GET", "/api/me/slack/", noLimiter},
	{"GET", "/api/notifications/", noLimiter},
	{"GET", "/api/notifications/unread_count", noLimiter},
	{"GET", "/api/repos/", noLimiter},
	{"GET", "/api/repos/{id}/board", noLimiter},
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
	{"PATCH", "/api/repos/{id}", noLimiter},
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
	{"POST", "/api/repos/{id}/runs", limForge},
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
	{"PUT", "/api/me/judge/recommendations/disposition", noLimiter},
	{"PUT", "/api/me/secrets/anthropic_token", noLimiter},
	{"PUT", "/api/me/settings/", noLimiter},
	{"PUT", "/api/me/slack/notify", noLimiter},
	{"PUT", "/api/me/slack/override", limSlackDM},
	{"PUT", "/api/repos/{id}", noLimiter},
	{"PUT", "/api/repos/{id}/board/columns", noLimiter},
	{"PUT", "/api/repos/{id}/tool-profile", noLimiter},
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
// Function identity could not do that, and that is measured rather than assumed:
// comparing every walked middleware against reflect.ValueOf(ls[0].PerUserMiddleware)
// .Pointer() tagged all 24 mounts as equal, though those 24 carry SIX different
// limiter instances. A bound-method value's code pointer is the receiver-independent
// wrapper, and the receiver itself is not reachable through the reflect API — so a
// reflect compare can only see THAT some per-user middleware is mounted. Driving the
// middleware is what turns "some limiter" into a named one.
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
// wantRouteMounts), the limiters' real production budgets (this test substitutes
// its own), and the order of the Routes parameters — the limiters go in
// positionally here exactly as they do in main, so swapping two names in the
// signature would change behaviour invisibly to this file.
func TestEveryRouteCarriesItsExpectedPerUserLimiter(t *testing.T) {
	limiters := newProbeLimiters()
	// Hosting on, so the controller routes exist and the table is unconditional.
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}
	router := h.Routes(limiters[0], limiters[1], limiters[2], limiters[3],
		limiters[4], limiters[5], limiters[6], limiters[7])

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
// mapping that makes a probed budget readable as an identity. Adding or dropping a
// limiter parameter shifts every position after it, silently relabelling mounts;
// this fails instead.
//
// It cannot see a REORDER of two existing parameters — reflect knows their types,
// not their names — and neither can the walk test, which passes limiters
// positionally exactly as main does. That gap is real and is stated on the walk
// test too.
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
