package handler

// forgeview_test.go covers the DB-free pure helpers in forgeview.go directly:
// mergeStateDTO's BlockedReason priority + Conflicts tri-state passthrough, and the
// forge-error → HTTP mapping (writeForgeError / forgeRetryAfterSeconds). These need no
// database, no router and no repoForRequest/h.q — writeForgeError touches only the
// ResponseWriter, slog and forgeRetryAfterSeconds, so a zero-value &Handler{} suffices.
// The full ErrForgeVersionUnsupported→200 and closed-PR→404 HTTP paths (which DO need a
// DB via repoForRequest) live in the live-DB suite / are deferred.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestMergeStateDTO pins the fixed BlockedReason priority (conflicts → changes
// requested → failing check → pending check → "") by constructing an input that
// exercises each branch, and the Conflicts tri-state passthrough (nil stays nil).
func TestMergeStateDTO(t *testing.T) {
	tru, fls := true, false
	failing := forge.Check{Name: "ci", Status: "completed", Conclusion: "failure"}
	pending := forge.Check{Name: "lint", Status: "in_progress"} // not "completed" ⇒ pending
	passing := forge.Check{Name: "build", Status: "completed", Conclusion: "success"}

	t.Run("conflicts win over changes-requested and a failing check", func(t *testing.T) {
		s := forge.MergeRequestSummary{Conflicts: &tru, ReviewDecision: forge.ReviewChangesRequested}
		m := mergeStateDTO(s, []forge.Check{failing})
		if m.BlockedReason != "conflicts with main" {
			t.Fatalf("conflicts must win, got %q", m.BlockedReason)
		}
		if m.Conflicts == nil || !*m.Conflicts {
			t.Errorf("Conflicts tri-state must pass through (&true), got %v", m.Conflicts)
		}
	})

	t.Run("changes requested beats a failing check", func(t *testing.T) {
		s := forge.MergeRequestSummary{Conflicts: &fls, ReviewDecision: forge.ReviewChangesRequested}
		m := mergeStateDTO(s, []forge.Check{failing})
		if m.BlockedReason != "changes requested" {
			t.Fatalf("changes requested must beat a failing check, got %q", m.BlockedReason)
		}
	})

	t.Run("checks failing when no conflict and no changes requested", func(t *testing.T) {
		s := forge.MergeRequestSummary{Conflicts: &fls, ReviewDecision: forge.ReviewApproved}
		m := mergeStateDTO(s, []forge.Check{passing, failing})
		if m.BlockedReason != "checks failing" {
			t.Fatalf("a failing check with no conflict/changes ⇒ checks failing, got %q", m.BlockedReason)
		}
		if m.RequiredChecksPassed {
			t.Errorf("a failing check ⇒ RequiredChecksPassed=false")
		}
	})

	t.Run("waiting on checks when only a pending check", func(t *testing.T) {
		s := forge.MergeRequestSummary{Conflicts: &fls, ReviewDecision: forge.ReviewNone}
		m := mergeStateDTO(s, []forge.Check{pending})
		if m.BlockedReason != "waiting on checks" {
			t.Fatalf("a pending check alone ⇒ waiting on checks, got %q", m.BlockedReason)
		}
		if m.RequiredChecksPassed {
			t.Errorf("a pending check ⇒ RequiredChecksPassed=false")
		}
	})

	t.Run("failing beats pending when both are present", func(t *testing.T) {
		// The failing-vs-pending priority only bites when a PR has BOTH at once; the
		// disjoint fixtures above never co-occur, so a silent case-order swap would go
		// undetected without this. A failing check outranks a pending one.
		s := forge.MergeRequestSummary{Conflicts: &fls, ReviewDecision: forge.ReviewApproved}
		m := mergeStateDTO(s, []forge.Check{pending, failing})
		if m.BlockedReason != "checks failing" {
			t.Fatalf("failing must outrank pending, got %q", m.BlockedReason)
		}
		if m.RequiredChecksPassed {
			t.Errorf("a failing (and pending) check ⇒ RequiredChecksPassed=false")
		}
	})

	t.Run("clean when nothing blocks", func(t *testing.T) {
		s := forge.MergeRequestSummary{Conflicts: &fls, ReviewDecision: forge.ReviewApproved}
		m := mergeStateDTO(s, []forge.Check{passing})
		if m.BlockedReason != "" {
			t.Fatalf("nothing blocks ⇒ empty BlockedReason, got %q", m.BlockedReason)
		}
		if !m.RequiredChecksPassed {
			t.Errorf("no failing and no pending check ⇒ RequiredChecksPassed=true")
		}
	})

	t.Run("nil Conflicts stays nil", func(t *testing.T) {
		s := forge.MergeRequestSummary{Conflicts: nil, ReviewDecision: forge.ReviewNone}
		m := mergeStateDTO(s, nil)
		if m.Conflicts != nil {
			t.Errorf("nil Conflicts (unknown) must stay nil, got %v", *m.Conflicts)
		}
		if m.BlockedReason != "" {
			t.Errorf("no conflict, no decision, no checks ⇒ empty BlockedReason, got %q", m.BlockedReason)
		}
	})
}

// TestWriteForgeError pins the forge-error → HTTP mapping on a zero-value &Handler{}
// (writeForgeError touches no DB): a *forge.RateLimitError becomes 429 + Retry-After
// (from Retry or the Reset wall time, rounded up), and any other error becomes 502
// carrying the already-redacted message.
func TestWriteForgeError(t *testing.T) {
	h := &Handler{}

	t.Run("rate limit with explicit Retry ⇒ 429 + Retry-After", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.writeForgeError(rec, "list pulls", &forge.RateLimitError{Retry: 30 * time.Second})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "30" {
			t.Errorf("Retry-After = %q, want 30", got)
		}
	})

	t.Run("rate limit from Reset wall time ⇒ ~90s", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.writeForgeError(rec, "list pulls", &forge.RateLimitError{Reset: time.Now().Add(90 * time.Second)})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if err != nil {
			t.Fatalf("Retry-After not an int: %q (%v)", rec.Header().Get("Retry-After"), err)
		}
		if secs < 89 || secs > 90 {
			t.Errorf("Retry-After = %d, want ~90 (>=89, allowing round-up + a little scheduling delay)", secs)
		}
	})

	t.Run("sub-second Retry rounds up to 1", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.writeForgeError(rec, "list pulls", &forge.RateLimitError{Retry: 500 * time.Millisecond})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want 1 (a sub-second wait still asks for at least 1s)", got)
		}
	})

	t.Run("plain error ⇒ 502, no token leak", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.writeForgeError(rec, "list pulls", errors.New("bad gateway from upstream"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		if h := rec.Header().Get("Retry-After"); h != "" {
			t.Errorf("a non-rate-limit error must not set Retry-After, got %q", h)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "bad gateway from upstream") {
			t.Errorf("502 body should carry the redacted message, got %q", body)
		}
		// The driver already redacts before this point; confirm the plain path emits
		// no token-shaped string of its own.
		for _, tok := range []string{"ghp_", "glpat-", "github_pat_"} {
			if strings.Contains(body, tok) {
				t.Errorf("body must not contain a token-shaped prefix %q, got %q", tok, body)
			}
		}
	})
}

// TestForgeRetryAfterSeconds pins the whole-second rounding directly: a whole-second
// Retry passes through, a sub-second Retry rounds up to 1, a fractional Retry rounds
// up, and neither-signal / past-Reset yield 0 (no Retry-After hint).
func TestForgeRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		name string
		rl   forge.RateLimitError
		want int
	}{
		{"whole-second Retry", forge.RateLimitError{Retry: 30 * time.Second}, 30},
		{"sub-second Retry rounds up", forge.RateLimitError{Retry: 500 * time.Millisecond}, 1},
		{"fractional Retry rounds up", forge.RateLimitError{Retry: 1500 * time.Millisecond}, 2},
		{"neither Retry nor Reset ⇒ 0", forge.RateLimitError{}, 0},
		{"Reset already elapsed ⇒ 0", forge.RateLimitError{Reset: time.Now().Add(-time.Minute)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rl := tc.rl
			if got := forgeRetryAfterSeconds(&rl); got != tc.want {
				t.Errorf("forgeRetryAfterSeconds = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestForgeMemoPrefixIsolatesTenants pins the ONE invariant that keeps the forge-view
// memo from leaking across tenants: forgeMemoPrefix must yield a DISTINCT prefix whenever
// the connection identity or repo changes, because singleflight collapses concurrent
// misses on a single key — an under-scoped prefix would serve connection A's data to B.
// It also confirms determinism (identical rows share one prefix, else co-polling clients
// never share a load) and that the RAW sealed ciphertext never lands in the key (only its
// sha256). DB-free: forgeMemoPrefix is a pure function of three GetRepoForUserRow fields.
func TestForgeMemoPrefixIsolatesTenants(t *testing.T) {
	connA, connB := uuid.New(), uuid.New()
	repoA, repoB := uuid.New(), uuid.New()
	// Recognizable, non-token-shaped markers so the "raw ciphertext absent from the key"
	// assertion below is meaningful (the key hex-encodes only the sha256, never these bytes).
	ctA := []byte("SEALED-CIPHERTEXT-TENANT-A")
	ctB := []byte("SEALED-CIPHERTEXT-TENANT-B")

	base := store.GetRepoForUserRow{ConnectionID: connA, TokenCiphertext: ctA, ID: repoA}

	t.Run("differs on ConnectionID alone", func(t *testing.T) {
		other := base
		other.ConnectionID = connB
		if forgeMemoPrefix(base) == forgeMemoPrefix(other) {
			t.Fatalf("two connections must not share a memo prefix (connID is the tenant boundary)")
		}
	})

	t.Run("differs on TokenCiphertext alone (PAT rotation)", func(t *testing.T) {
		rotated := base
		rotated.TokenCiphertext = ctB
		if forgeMemoPrefix(base) == forgeMemoPrefix(rotated) {
			t.Fatalf("a rotated PAT (new sealed ciphertext) must yield a fresh prefix so it misses the old tenant's entries")
		}
	})

	t.Run("differs on repo ID alone", func(t *testing.T) {
		other := base
		other.ID = repoB
		if forgeMemoPrefix(base) == forgeMemoPrefix(other) {
			t.Fatalf("PR #5 on repo A must not collide with PR #5 on repo B")
		}
	})

	t.Run("identical rows yield the same prefix (determinism)", func(t *testing.T) {
		same := store.GetRepoForUserRow{
			ConnectionID:    connA,
			TokenCiphertext: append([]byte(nil), ctA...), // distinct backing array, same bytes
			ID:              repoA,
		}
		if forgeMemoPrefix(base) != forgeMemoPrefix(same) {
			t.Fatalf("identical (connID, ciphertext, repoID) must map to one prefix, else co-polling clients never share a load")
		}
	})

	t.Run("raw token ciphertext never appears in the prefix", func(t *testing.T) {
		if strings.Contains(forgeMemoPrefix(base), string(ctA)) {
			t.Fatalf("the raw sealed ciphertext must not leak into the memo key (only its sha256)")
		}
	})
}
