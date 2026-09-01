package handler

import (
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// This file is a file-name-independent completeness backstop for "invariant #3":
// every route mounted under the worker/controller auth groups must be Bearer-only.
// The route set is derived from the REAL router via chi.Walk, so a route added to
// mountWorkerRoutes/mountControllerRoutes tomorrow is covered here without touching
// this file — there is no hand-maintained route table to fall out of date.
//
// The property, per route under /api/worker/ or /api/controller/:
//   - POSITIVE: its middleware chain contains RequireWorker (resp. RequireController).
//     This catches a *replaced* auth middleware — a route whose group guard was swapped
//     for something else reddens because the expected Bearer guard is gone.
//   - DENYLIST: its chain contains NEITHER RequireAuth NOR RequireUser, the only two
//     middlewares in api/internal/middleware that read the session cookie / CSRF token
//     (RequireAuth: auth.go — r.Cookie(...) + ValidateCSRF; RequireUser: cli_auth.go —
//     falls back to the RequireAuth cookie path). RequireWorker (worker_auth.go) and
//     RequireController (controller_auth.go) read only the Authorization Bearer header.
//
// Residual gap (matching the issue's assertion (2)): the denylist is complete only for
// TODAY's set of cookie/CSRF readers — RequireAuth and RequireUser are the only two in
// api/internal/middleware, confirmed this session. A *newly added* cookie-reading
// middleware mounted alongside RequireWorker would slip past this denylist; catching
// that would need a positive allowlist ("the chain contains ONLY known-Bearer
// middleware"). That is a deliberate, documented residual gap.
//
// chi.Walk (v5.3.2) passes the FULL accumulated middleware chain to the WalkFunc,
// INCLUDING the group's r.Use(...) middleware, as separate slice entries — verified in
// go-chi/chi/v5@v5.3.2/tree.go: mws := slices.Concat(parentMw, r.Middlewares()), and the
// mounted RequireWorker was observed as a distinct chain entry for /api/worker/register.
//
// ── Why identity is by runtime function NAME, not reflect pointer ──────────────────
// The obvious approach — compare reflect.ValueOf(m).Pointer() of each walked middleware
// to a freshly-built reflect.ValueOf(mw.RequireWorker(nil)).Pointer() — is UNSOUND here,
// measured this session:
//   - RequireWorker/RequireController/RequireAuth/RequireUser each RETURN A CLOSURE that
//     captures its argument, so reflect.Pointer() yields a per-instance funcval address,
//     not a stable per-function code pointer: two calls to mw.RequireWorker(nil) gave two
//     DIFFERENT pointers.
//   - Building the reference INLINE in the test additionally lets the compiler inline the
//     constructor and emit a *distinct* closure function per instantiation, so the
//     reference's pointer never equals the mounted (non-inlined, canonical) closure's.
// runtime.FuncForPC(reflect.ValueOf(m).Pointer()).Name() sidesteps both: the mounted
// closure resolves to the canonical name
// "…/api/internal/middleware.RequireWorker.func1", and matching a package-function needle
// like ".RequireWorker.func" identifies that closure regardless of closure-instancing or
// of any future inlining that re-attributes the name's prefix to a caller.

// authKind labels a middleware as one of the four auth guards we care about, or "other"
// (a limiter, chi Recoverer/RequestID, etc. — all allowed on a Bearer route).
type authKind int

const (
	kindOther authKind = iota
	kindWorker
	kindController
	kindAuth
	kindUser
)

// Needles matched against runtime.FuncForPC(...).Name() to identify each middleware's
// returned closure. Each is anchored with a leading AND trailing "." so it matches the
// closure of exactly that constructor while surviving compiler inlining — measured this
// session, the SAME constructor's closure appears under two runtime-name shapes: not
// inlined it is "…/middleware.RequireWorker.func1", and inlined into its mount site it is
// "…mountControllerRoutes.func1.RequireController.1". The common, stable token across both
// is ".RequireController." (trailing "." present in both ".RequireController.func1" and
// ".RequireController.1"), whereas ".func" appears only in the non-inlined shape. The
// trailing "." also excludes the bare constructor func ("middleware.RequireWorker", no
// trailing segment) and any same-prefixed helper (".RequireWorkerFoo.1" does not contain
// ".RequireWorker."). The four are mutually exclusive as substrings;
// TestAuthMiddlewareIdentitiesAreDistinct proves the classifier is injective over the
// real constructors.
const (
	needleWorker     = ".RequireWorker."
	needleController = ".RequireController."
	needleAuth       = ".RequireAuth."
	needleUser       = ".RequireUser."
)

func classifyAuthMW(m func(http.Handler) http.Handler) authKind {
	fn := runtime.FuncForPC(reflect.ValueOf(m).Pointer())
	if fn == nil {
		return kindOther
	}
	name := fn.Name()
	switch {
	case strings.Contains(name, needleWorker):
		return kindWorker
	case strings.Contains(name, needleController):
		return kindController
	case strings.Contains(name, needleAuth):
		return kindAuth
	case strings.Contains(name, needleUser):
		return kindUser
	default:
		return kindOther
	}
}

// TestAuthMiddlewareIdentitiesAreDistinct is the trust guard for
// TestWorkerControllerRoutesAreBearerOnly. It builds each of the four real auth
// middlewares and asserts the classifier maps it to its OWN distinct kind. If a
// constructor's closure name stops matching its needle (a refactor renamed it, or made
// it match a sibling), the classifier can no longer tell the middleware apart and the
// Bearer-only completeness assertions in the sibling test would be unsound — so this
// fails loudly rather than letting that test pass vacuously. Mirrors the spirit of the
// limiter-probe trust guard in route_limiter_mounts_test.go.
//
// Building the constructors with nil/zero args does NOT dereference those args: each
// returns a closure that only runs at request time, and this test never issues a
// request.
func TestAuthMiddlewareIdentitiesAreDistinct(t *testing.T) {
	cases := []struct {
		name string
		got  authKind
		want authKind
	}{
		{"RequireWorker", classifyAuthMW(mw.RequireWorker(nil)), kindWorker},
		{"RequireController", classifyAuthMW(mw.RequireController(nil)), kindController},
		{"RequireAuth", classifyAuthMW(mw.RequireAuth(nil, config.Config{})), kindAuth},
		{"RequireUser", classifyAuthMW(mw.RequireUser(nil, config.Config{})), kindUser},
	}
	seen := map[authKind]string{}
	for _, c := range cases {
		if c.got == kindOther {
			t.Fatalf("%s classified as kindOther — the name-based classifier no longer "+
				"recognizes it, so the Bearer-only completeness assertions in "+
				"TestWorkerControllerRoutesAreBearerOnly would be unsound", c.name)
		}
		if c.got != c.want {
			t.Fatalf("%s classified as kind %d, want %d — the classifier confuses two "+
				"auth middlewares, so the Bearer-only completeness assertions in "+
				"TestWorkerControllerRoutesAreBearerOnly would be unsound", c.name, c.got, c.want)
		}
		if prev, dup := seen[c.got]; dup {
			t.Fatalf("%s and %s both classify as kind %d — reflect/runtime can no longer "+
				"tell them apart, so the completeness assertions in "+
				"TestWorkerControllerRoutesAreBearerOnly would be unsound", prev, c.name, c.got)
		}
		seen[c.got] = c.name
	}
}

// TestWorkerControllerRoutesAreBearerOnly walks BOTH the main router (Routes) and the
// TLS-listener router (WorkerRoutes) and asserts, for every route mounted under
// /api/worker/ or /api/controller/, that its middleware chain carries the correct Bearer
// group guard and carries NEITHER cookie/CSRF reader. See the file comment for the
// property and its documented residual gap.
func TestWorkerControllerRoutesAreBearerOnly(t *testing.T) {
	limiters := newProbeLimiters()
	// WorkerHostingEnabled:true so mountControllerRoutes does NOT early-return and the
	// /api/controller/ group is actually mounted (handler.go: the group is absent when
	// hosting is off). h.q stays nil — the RequireWorker closure is never invoked here.
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}

	// Routes is NOT variadic (9 fixed *mw.Limiter params) — call positionally.
	mainRouter := h.Routes(limiters[0], limiters[1], limiters[2], limiters[3],
		limiters[4], limiters[5], limiters[6], limiters[7], limiters[8])
	// WorkerRoutes takes the single proposalLimiter (position 4).
	tlsRouter := h.WorkerRoutes(limiters[4])

	var workerSeen, controllerSeen int

	walk := func(name string, router http.Handler) {
		routes, ok := router.(chi.Routes)
		if !ok {
			t.Fatalf("%s router is not a chi.Routes", name)
		}
		err := chi.Walk(routes, func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
			kinds := make(map[authKind]bool, len(mws))
			for _, m := range mws {
				kinds[classifyAuthMW(m)] = true
			}

			// The trailing slash is load-bearing: "/api/worker/" must NOT match the admin
			// "/api/workers/{id}" group, which is cookie-authenticated by design.
			switch {
			case strings.HasPrefix(pattern, "/api/worker/"):
				workerSeen++
				if !kinds[kindWorker] {
					t.Errorf("%s: %s %s under /api/worker/ is missing RequireWorker — its Bearer guard was replaced", name, method, pattern)
				}
				if kinds[kindAuth] || kinds[kindUser] {
					t.Errorf("%s: %s %s under /api/worker/ carries a cookie/CSRF middleware (RequireAuth/RequireUser); worker routes must be Bearer-only", name, method, pattern)
				}
			case strings.HasPrefix(pattern, "/api/controller/"):
				controllerSeen++
				if !kinds[kindController] {
					t.Errorf("%s: %s %s under /api/controller/ is missing RequireController — its Bearer guard was replaced", name, method, pattern)
				}
				if kinds[kindAuth] || kinds[kindUser] {
					t.Errorf("%s: %s %s under /api/controller/ carries a cookie/CSRF middleware (RequireAuth/RequireUser); controller routes must be Bearer-only", name, method, pattern)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s router: %v", name, err)
		}
	}

	walk("Routes", mainRouter)
	walk("WorkerRoutes", tlsRouter)

	// Non-vacuity: if the classification matched nothing, the assertions above never ran
	// and a green here would prove nothing. Both groups are mounted by both routers.
	if workerSeen == 0 {
		t.Fatal("no /api/worker/ routes were walked — the test is vacuous; check the mount and the pattern prefix")
	}
	if controllerSeen == 0 {
		t.Fatal("no /api/controller/ routes were walked — the test is vacuous; check WorkerHostingEnabled and the pattern prefix")
	}
}
