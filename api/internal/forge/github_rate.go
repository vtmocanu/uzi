package forge

// github_rate.go is the GitHub side of D4's outbound budget: the primary
// rate-limit RESERVE (PRD #1255 D4, shedding rule 1). GitHub's ~5000/h primary
// limit is SHARED across every lane that uses one PAT — the interactive
// forge-view reads AND the poller/worker board sync — so a burst of interactive
// reads (capped per-connection by the FORGE_INTERACTIVE_RATE_MAX token bucket but
// still up to 120/min) could drain the budget the poller depends on. To protect
// the poller's lane, every GitHub response's rate state is recorded here (keyed by
// the PAT's sha256, no token stored), and an INTERACTIVE forge-view read is shed
// BEFORE any forge call while the recorded Remaining sits below the reserve
// max(500, 10% of Limit). The poller's own reads carry no interactive flag
// (WithInteractiveRead is set only by the forge-view handler), so this NEVER sheds
// the lane it is protecting.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// githubRateReserveFloor is the absolute headroom (requests) held back for the
// poller/worker lanes, the "500" in D4's max(500, 10% of Limit). On the classic
// PAT's 5000/h limit the 10% term (500) ties it; on a smaller limit the floor
// dominates so a low ceiling still keeps a fixed reserve.
const githubRateReserveFloor = 500

// githubRateMaxTokens bounds the process-wide recorder. It is one entry per live
// PAT (a forge connection), the same per-token bound rationale as the ETag cache
// (etagCacheMaxEntries) — a forge connection set is small, and a rotated PAT's
// stale entry is pruned by record when the map fills.
const githubRateMaxTokens = etagCacheMaxEntries

// githubTokenHash is the SINGLE source of truth for the per-PAT cache/recorder key:
// the hex sha256 of the plaintext token. The ETag transport's salt and the driver's
// tokenHash both derive their key through this function, so the key the transport
// RECORDS a rate under and the key the driver SHEDS against are guaranteed to agree
// for one connection. No raw token is ever stored — only this hash.
func githubTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// interactiveReadKey is the unexported context key marking a forge read as
// interactive (a TUI/CLI-driven forge-view read), so the reserve sheds it while the
// poller's unmarked reads pass through.
type interactiveReadKey struct{}

// WithInteractiveRead marks ctx as an interactive forge-view read. The forge-view
// HANDLER wraps its memo load context with this; the poller/worker lanes never do,
// which is what makes the reserve shed interactive reads without ever shedding the
// background lane it reserves headroom for (PRD #1255 D4).
func WithInteractiveRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveReadKey{}, true)
}

// IsInteractiveRead reports whether ctx was marked by WithInteractiveRead. A driver
// gates its reserve-shed on this so only interactive reads are shed.
func IsInteractiveRead(ctx context.Context) bool {
	v, _ := ctx.Value(interactiveReadKey{}).(bool)
	return v
}

// githubRateRecord is one PAT's last-seen primary rate state.
type githubRateRecord struct {
	remaining int
	limit     int
	reset     time.Time
}

// githubRateRecorder is the process-wide, mutex-guarded map of PAT-hash → last-seen
// rate state. It is process-wide (not per-driver) for the same reason the ETag cache
// is: ForgeForConnection rebuilds the driver per request, so a per-driver recorder
// would never survive to inform a second read.
type githubRateRecorder struct {
	mu sync.Mutex
	m  map[string]githubRateRecord
}

// githubRates is the process-wide recorder. recordGitHubRate writes it from the ETag
// transport; gitHubRateReserveLow reads it from the GitHub driver's forge-view methods.
var githubRates = &githubRateRecorder{m: make(map[string]githubRateRecord)}

// recordGitHubRate stores the current primary rate state for a PAT (keyed by its
// sha256 hex). Called from the ETag transport on every GitHub response that carries
// the X-RateLimit-* headers, so the record reflects the true current Remaining
// regardless of which lane made the call.
func recordGitHubRate(tokenHash string, remaining, limit int, reset time.Time) {
	githubRates.record(tokenHash, remaining, limit, reset)
}

// gitHubRateReserveLow reports whether the recorded budget for a PAT sits below the
// D4 reserve. low is true iff a record exists AND Remaining < max(500, Limit/10) AND
// Reset is still in the future (an expired window means the budget has already
// refilled — not low). reset is the recorded window-reset wall time, used by the
// driver as the RateLimitError's Reset so the handler's Retry-After is honest.
func gitHubRateReserveLow(tokenHash string) (low bool, reset time.Time) {
	return githubRates.reserveLow(tokenHash, time.Now())
}

func (r *githubRateRecorder) record(tokenHash string, remaining, limit int, reset time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.m[tokenHash]; !exists && len(r.m) >= githubRateMaxTokens {
		r.pruneLocked()
	}
	r.m[tokenHash] = githubRateRecord{remaining: remaining, limit: limit, reset: reset}
}

func (r *githubRateRecorder) reserveLow(tokenHash string, now time.Time) (bool, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.m[tokenHash]
	if !ok {
		return false, time.Time{}
	}
	if !rec.reset.After(now) {
		// Window already refilled: the budget is not low regardless of the stale
		// Remaining frozen at the last response.
		return false, rec.reset
	}
	threshold := githubRateReserveFloor
	if tenth := rec.limit / 10; tenth > threshold {
		threshold = tenth
	}
	return rec.remaining < threshold, rec.reset
}

// pruneLocked keeps the recorder bounded. It first drops every entry whose window has
// already expired (a rotated/idle PAT), then, only if still at the cap (more distinct
// live-window PATs than the bound — not expected in practice), drops one arbitrary
// entry. Losing a record degrades safely: that PAT reads as "no record" so its next
// interactive read is not shed, and the per-connection token bucket still guards the
// poller. Caller holds r.mu.
func (r *githubRateRecorder) pruneLocked() {
	now := time.Now()
	for k, rec := range r.m {
		if !rec.reset.After(now) {
			delete(r.m, k)
		}
	}
	for k := range r.m {
		if len(r.m) < githubRateMaxTokens {
			break
		}
		delete(r.m, k)
	}
}

// tokenHash is this driver's PAT key for the process-wide recorder, derived through
// githubTokenHash so it matches the salt the ETag transport records under. Computed
// on demand: the sha256 is cheap and a driver is short-lived (rebuilt per request),
// so caching would buy nothing.
func (g *github) tokenHash() string {
	return githubTokenHash(g.token)
}

// shedIfReserved returns a *RateLimitError when ctx is an interactive read AND the
// recorded GitHub primary budget for this driver's PAT is below the D4 reserve
// (max(500, 10% of Limit)) — so an interactive forge-view read is shed BEFORE any
// forge call, reserving that headroom for the poller/worker lanes. Those lanes carry
// no interactive flag (WithInteractiveRead is set only by the forge-view handler),
// so they are never shed. nil means the read may proceed.
func (g *github) shedIfReserved(ctx context.Context) error {
	if !IsInteractiveRead(ctx) {
		return nil
	}
	if low, reset := gitHubRateReserveLow(g.tokenHash()); low {
		return &RateLimitError{
			Reset: reset,
			Err:   errors.New("forge: github primary rate budget reserved for background sync; interactive read shed"),
		}
	}
	return nil
}
