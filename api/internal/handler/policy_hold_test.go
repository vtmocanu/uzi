package handler

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Classify every middleware from both production mounts, including per-worker
// and IP limiters that the per-user inventory deliberately does not classify.
func TestPolicyHoldRouteHasNoRateLimiter(t *testing.T) {
	for _, name := range []string{"application", "worker"} {
		t.Run(name, func(t *testing.T) {
			ls := newProbeLimiters()
			h := &Handler{}
			router := h.WorkerRoutes(ls[4])
			if name == "application" {
				router = h.Routes(ls[0], ls[1], ls[2], ls[3], ls[4], ls[5], ls[6], ls[7], ls[8], ls[9], ls[10])
			}
			routes, ok := router.(chi.Routes)
			if !ok {
				t.Fatalf("router %T cannot be walked", router)
			}
			seen := 0
			p := &prober{}
			err := chi.Walk(routes, func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
				if method != http.MethodPost || pattern != "/api/worker/runs/{id}/policy-hold" {
					return nil
				}
				seen++
				for _, m := range mws {
					if budget, _ := p.probe(m); budget != 0 {
						t.Errorf("unexpected rate limiter with budget %d", budget)
					}
				}
				return nil
			})
			if err != nil || seen != 1 {
				t.Fatalf("route count=%d, walk error=%v", seen, err)
			}
		})
	}
}

func TestPolicyHoldNotReady(t *testing.T) {
	if slices.Contains(protocolFeatures(true, true), capability.ProviderPolicyHoldV1) {
		t.Fatal("incomplete policy continuation advertised ready")
	}
}

func TestPolicyHoldRequiresWorker(t *testing.T) {
	w := httptest.NewRecorder()
	(&Handler{}).WorkerRunPolicyHold(w, httptest.NewRequest(http.MethodPost, "/api/worker/runs/id/policy-hold", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestPolicyHoldStrictBody(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":   `{"execution":{}}`,
		"trailing":  `{} {}`,
		"overflow":  `{}` + strings.Repeat(" ", 17*1024),
		"malformed": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			var req workersvc.PolicyHoldRequest
			if httpx.DecodeJSONStrictBounded(r, &req, 17*1024) == nil {
				t.Fatal("accepted invalid body")
			}
		})
	}
}
