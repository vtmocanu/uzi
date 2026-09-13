package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

// TestForgeViewBudgetShedsZeroForgeCallsLiveDB is the D4-pinned shed test: once a
// connection's interactive-read outbound budget is exhausted, GET /pulls answers 429
// with a Retry-After header and makes ZERO forge calls (the budget is charged INSIDE the
// memoized load, before the forge call, so a shed load returns a *forge.RateLimitError
// uncached and never reaches the forge). The zero-call assertion is the load-bearing one
// and is made non-vacuous by a preceding warm request that proves the counting fake
// (reused from the memo suite) really increments on a real forge round-trip.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestForgeViewBudgetShedsZeroForgeCallsLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	fake := &countingForgeView{}
	h.forgeFactory = func(string, string, []byte) (forge.Forge, error) { return fake, nil }
	forgeCalls := func() int64 { return fake.refsCalls.Load() + fake.summaryCalls.Load() }

	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	connID := rmSeedConn(t, pool, owner)
	// Two repos under ONE connection: both share the connection's budget (that is the
	// point — the budget is per-connection, not per-repo), but they have distinct
	// tenant-scoped memo prefixes, so the shed request on `shed` is a guaranteed memo
	// MISS that reaches the load closure — no dependence on the warm request's memo
	// entries or on the TTL clock.
	warm := rmSeedRepo(t, pool, connID, 1255201, true)
	shed := rmSeedRepo(t, pool, connID, 1255202, true)

	// Non-vacuity: a generous budget lets a normal GET /pulls make its real forge calls
	// (refs + per-PR summary), so the fake's counter moves. If it did not, the zero-call
	// assertion below would be meaningless.
	h.forgeBudget = newForgeBudget(1000, nil)
	if rec := bearerReq(router, http.MethodGet, fmt.Sprintf("/api/repos/%s/pulls", warm), uzc); rec.Code != http.StatusOK {
		t.Fatalf("warm GET pulls = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if forgeCalls() == 0 {
		t.Fatalf("warm GET pulls made 0 forge calls; the counting fake is not wired (the zero-call assertion would be vacuous)")
	}

	// Exhaust the connection's budget: a max-1 bucket on a frozen clock, pre-drained of
	// its single token, so the shed request finds it empty with no refill.
	frozen := time.Unix(0, 0)
	drained := newForgeBudget(1, func() time.Time { return frozen })
	if ok, _ := drained.Take(connID); !ok {
		t.Fatalf("pre-drain Take = false, want true (the single burst token)")
	}
	h.forgeBudget = drained

	before := forgeCalls()
	rec := bearerReq(router, http.MethodGet, fmt.Sprintf("/api/repos/%s/pulls", shed), uzc)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("shed GET pulls = %d, want 429\nbody: %s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatalf("shed GET pulls: missing Retry-After header (429 must carry one)")
	}
	if got := forgeCalls(); got != before {
		t.Fatalf("shed GET pulls made %d forge call(s), want 0 (a shed request must make no forge call)", got-before)
	}
}
