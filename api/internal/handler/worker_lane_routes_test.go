package handler

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// worker_lane_routes_test.go pins the PRD #1906 M5 (D-D) route allowlist for isolated-lane
// workers against the REAL routers. The route set comes from chi.Walk over Routes and
// WorkerRoutes, so a route added to mountWorkerRoutes tomorrow is classified here without
// editing this file: it must carry the guard, and unless it is added to BOTH the production
// allowlist and wantLaneAllowed below it is refused.

// wantLaneAllowed is this test's own statement of the lane allowlist, written out rather than
// read from laneWorkerAllowlist, so dropping or adding a production entry reddens here.
var wantLaneAllowed = map[string]bool{
	"POST /register":                 true,
	"POST /heartbeat":                true,
	"POST /runs/claim":               true,
	"POST /runs/{id}/state":          true,
	"POST /runs/{id}/messages":       true,
	"GET /runs/{id}/message-gaps":    true,
	"GET /runs/{id}/inputs":          true,
	"POST /runs/{id}/inputs/ack":     true,
	"POST /runs/{id}/inputs/applied": true,
	// PRD #1976: the three job routes a profile-bound job needs on the lane.
	"POST /runs/{id}/job-result":    true,
	"GET /runs/{id}/files/{fileID}": true,
	"POST /runs/{id}/files":         true,
}

var routeParam = regexp.MustCompile(`\{[^/}]+\}`)

// concretePath fills every {param} of a chi pattern with a fixed value.
func concretePath(pattern string) string {
	return routeParam.ReplaceAllString(pattern, "123")
}

const laneProbeSentinel = http.StatusTeapot

// laneReplica mounts the given relative worker patterns under /api/worker on a fresh router
// with the SAME guard, behind a stand-in for RequireWorker that puts wkr in the context. Its
// handlers answer laneProbeSentinel, so "reached the handler" is never confused with an answer
// the guard wrote.
func laneReplica(wkr store.Worker, routes [][2]string) http.Handler {
	root := chi.NewRouter()
	root.Route("/api/worker", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), wkr)))
			})
		})
		r.Use(laneWorkerRouteGuard)
		for _, rt := range routes {
			r.Method(rt[0], rt[1], http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(laneProbeSentinel)
			}))
		}
	})
	return root
}

func laneStatus(t *testing.T, h http.Handler, method, target string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Code
}

func TestLaneWorkerRouteTable(t *testing.T) {
	limiters := newProbeLimiters()
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}
	mainRouter := h.Routes(limiters[0], limiters[1], limiters[2], limiters[3],
		limiters[4], limiters[5], limiters[6], limiters[7], limiters[8], limiters[9])
	tlsRouter := h.WorkerRoutes(limiters[4])

	guardPtr := reflect.ValueOf(laneWorkerRouteGuard).Pointer()
	lane := store.Worker{ID: uuid.New(), IsolatedLane: true}
	plain := store.Worker{ID: uuid.New()}

	for _, tc := range []struct {
		name   string
		router http.Handler
	}{{"Routes", mainRouter}, {"WorkerRoutes", tlsRouter}} {
		t.Run(tc.name, func(t *testing.T) {
			var routes [][2]string
			seen := map[string]bool{}
			err := chi.Walk(tc.router.(chi.Routes), func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
				if !strings.HasPrefix(pattern, "/api/worker/") {
					return nil
				}
				rel := strings.TrimPrefix(pattern, "/api/worker")
				key := method + " " + rel
				seen[key] = true
				routes = append(routes, [2]string{method, rel})

				// The guard is in the chain, after RequireWorker (it reads the worker that set).
				guardAt, workerAt := -1, -1
				for i, m := range mws {
					if reflect.ValueOf(m).Pointer() == guardPtr {
						guardAt = i
					}
					if classifyAuthMW(m) == kindWorker {
						workerAt = i
					}
				}
				if guardAt < 0 {
					t.Errorf("%s %s: laneWorkerRouteGuard is not in the chain, so a lane worker reaches it", method, pattern)
				} else if workerAt < 0 || workerAt > guardAt {
					t.Errorf("%s %s: laneWorkerRouteGuard runs before RequireWorker (guard %d, worker %d)", method, pattern, guardAt, workerAt)
				}

				// Every route is either allowlisted or refused: the matcher agrees with the list.
				if got := laneRouteAllowed(method, concretePath(rel)); got != wantLaneAllowed[key] {
					t.Errorf("%s: laneRouteAllowed = %v, want %v", key, got, wantLaneAllowed[key])
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if len(routes) < 20 {
				t.Fatalf("walked only %d /api/worker routes; the walk is vacuous", len(routes))
			}
			for key := range wantLaneAllowed {
				if !seen[key] {
					t.Errorf("allowlisted %s is not a mounted worker route (stale allowlist entry)", key)
				}
			}

			// The same patterns behind the real guard: a lane worker gets 403 on every refused
			// route and reaches the handler on every allowlisted one; an ordinary worker reaches
			// every handler.
			laneMux := laneReplica(lane, routes)
			plainMux := laneReplica(plain, routes)
			refused := 0
			for _, rt := range routes {
				target := "/api/worker" + concretePath(rt[1])
				want := http.StatusForbidden
				if wantLaneAllowed[rt[0]+" "+rt[1]] {
					want = laneProbeSentinel
				} else {
					refused++
				}
				if got := laneStatus(t, laneMux, rt[0], target); got != want {
					t.Errorf("lane worker %s %s = %d, want %d", rt[0], target, got, want)
				}
				if got := laneStatus(t, plainMux, rt[0], target); got != laneProbeSentinel {
					t.Errorf("ordinary worker %s %s = %d, want the handler (%d)", rt[0], target, got, laneProbeSentinel)
				}
			}
			if refused == 0 {
				t.Fatal("no worker route was refused to a lane worker")
			}
		})
	}
}

// TestLaneWorkerNewRouteDefaultsRefused: a route nobody listed (here one that does not exist
// in the real router yet) is refused to a lane worker, so a new worker route is closed to the
// lane until it is allowlisted on purpose.
func TestLaneWorkerNewRouteDefaultsRefused(t *testing.T) {
	routes := [][2]string{{http.MethodGet, "/runs/{id}/brand-new-read"}, {http.MethodPost, "/brand-new-write"}}
	lane := laneReplica(store.Worker{IsolatedLane: true}, routes)
	plain := laneReplica(store.Worker{}, routes)
	for _, rt := range routes {
		target := "/api/worker" + concretePath(rt[1])
		if laneRouteAllowed(rt[0], concretePath(rt[1])) {
			t.Errorf("laneRouteAllowed(%s %s) = true for an unlisted route", rt[0], rt[1])
		}
		if got := laneStatus(t, lane, rt[0], target); got != http.StatusForbidden {
			t.Errorf("lane worker %s %s = %d, want 403", rt[0], target, got)
		}
		if got := laneStatus(t, plain, rt[0], target); got != laneProbeSentinel {
			t.Errorf("ordinary worker %s %s = %d, want the handler", rt[0], target, got)
		}
	}
	// A method mismatch on an allowlisted path is refused too.
	if laneRouteAllowed(http.MethodGet, "/runs/123/state") {
		t.Error("GET /runs/{id}/state is allowed, but only POST is listed")
	}
}

// TestLaneWorkerChatClaimIsIdle: the chat claim lane answers a lane worker 204 without
// reaching the claim handler; the run lane reaches it.
func TestLaneWorkerChatClaimIsIdle(t *testing.T) {
	routes := [][2]string{{http.MethodPost, "/runs/claim"}}
	lane := laneReplica(store.Worker{IsolatedLane: true}, routes)
	if got := laneStatus(t, lane, http.MethodPost, "/api/worker/runs/claim?lane=chat"); got != http.StatusNoContent {
		t.Errorf("lane chat claim = %d, want 204", got)
	}
	if got := laneStatus(t, lane, http.MethodPost, "/api/worker/runs/claim"); got != laneProbeSentinel {
		t.Errorf("lane run claim = %d, want the handler", got)
	}
	plain := laneReplica(store.Worker{}, routes)
	if got := laneStatus(t, plain, http.MethodPost, "/api/worker/runs/claim?lane=chat"); got != laneProbeSentinel {
		t.Errorf("ordinary chat claim = %d, want the handler", got)
	}
}
