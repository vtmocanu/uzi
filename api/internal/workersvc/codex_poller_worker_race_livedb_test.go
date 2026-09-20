package workersvc

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
)

// PRD #1209 M4, DELIVERABLE B: a POLLER refresh (CollectCodexAccountUsage forced down its
// 401 → coordinatedRefresh path) converging with a WORKER refresh (CoordinatedCodexRefresh)
// on the SAME account. The M2 tests drive a single refresh; the m4 (#1147) matrix races two
// WORKERS. Neither races a poller against a worker — the shared coordinator serving BOTH the
// owner-scoped poll path and the run-scoped worker path — which is exactly what this proves.
//
// The invariant: no matter which of the two paths rotates, the provider is called EXACTLY
// ONCE, the account advances EXACTLY ONE generation, the loser reconciles to the winner's
// committed token WITHOUT a second exchange, and a token is released only AFTER the durable
// commit.
//
// Interleaving. The two paths run as concurrent goroutines joined by a WaitGroup, with one
// happens-before edge: the worker holds a STALE observed generation (0) and issues its
// coordinated refresh only after the poll-driven refresh has committed generation 1. This is
// a real, load-bearing scenario — a worker claim snapshots the account generation and may
// issue its refresh after an idle poll already advanced the account — and it is the ONLY
// interleaving that drives the loser through the coordinator's own reconcile rule
// (observedGeneration < acct.Generation) rather than the lease-contention reconcile, so the
// one-generation convergence is proven at the CAS that owns it (and is mutation-sensitive
// there). A free-running barrier race, by contrast, resolves the loser at the lease guard
// and would prove a different, weaker property. The poll path is genuinely FORCED down
// coordinatedRefresh: its first read is 401'd on the pre-rotation token, and it wins the
// (uncontended) rotation itself.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// racingProviderClient is the shared provider seam for the poller↔worker race. It satisfies
// BOTH CodexUsageReader (ReadUsage) and CodexRefreshClient (Refresh + DiscoverIdentity), so
// the ONE injected client backs both paths exactly as the production *codexauth.Client does.
// It COUNTS token exchanges and mints a rotating token per exchange (so a second exchange is
// observable), and is safe for concurrent use.
type racingProviderClient struct {
	mu           sync.Mutex
	refreshCalls int
	readCalls    int

	// gen0Token is the account's pre-rotation committed access token. ReadUsage 401s on it
	// (forcing the poll path down the refresh loop) and returns a matching reading for any
	// ROTATED token, so a poll that reads the freshly-committed token converges.
	gen0Token     string
	matchIdentity codexauth.Identity
}

func (c *racingProviderClient) ReadUsage(_ context.Context, accessToken, _ string) (codexauth.UsageReading, error) {
	c.mu.Lock()
	c.readCalls++
	gen0 := c.gen0Token
	id := c.matchIdentity
	c.mu.Unlock()
	if accessToken == gen0 {
		// The stale pre-rotation token is rejected — the only status that drives the poll
		// path into coordinatedRefresh.
		return codexauth.UsageReading{}, &codexauth.AuthError{Op: "read_usage", StatusCode: http.StatusUnauthorized}
	}
	// A rotated (freshly-committed) token → a matching, successful reading.
	return codexauth.UsageReading{UserID: id.ProviderUserID, Buckets: []codexauth.UsageBucket{{ID: "codex"}}}, nil
}

func (c *racingProviderClient) Refresh(_ context.Context, _ string) (codexauth.RefreshResult, error) {
	c.mu.Lock()
	c.refreshCalls++
	call := c.refreshCalls
	id := c.matchIdentity
	c.mu.Unlock()
	access := codexToken(fmt.Sprintf("access-rot%d", call))
	rotated := codexToken(fmt.Sprintf("refresh-rot%d", call))
	return codexauth.RefreshResult{
		AccessToken:    access,
		RefreshToken:   &rotated,
		IdentityClaims: freshClaimsForIdentity(id),
	}, nil
}

func (c *racingProviderClient) DiscoverIdentity(_ context.Context, _ string) (codexauth.Identity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.matchIdentity, nil
}

func (c *racingProviderClient) exchangeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshCalls
}

func TestCodexPollerWorkerRefreshConvergesSingleExchangeLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fake := &racingProviderClient{}
	// newRefreshFixture seeds a linked account at generation 0 with a claimed run/worker and
	// freezes the binding. It pins matchIdentity only for the known fake types, so set it (and
	// the pre-rotation token the poll path 401s on) on the racing client explicitly.
	f := newRefreshFixture(t, env, fake)
	fake.gen0Token = f.accessToken
	fake.matchIdentity = codexauth.Identity{ProviderUserID: f.providerUserID, WorkspaceAccountID: f.workspaceAcctID}
	capw := env.mintCap(t, f.runID, f.workerID)

	var (
		pollerReading CodexUsageReading
		pollerErr     error
		workerRes     CodexRefreshResult
		workerErr     error
		wg            sync.WaitGroup
	)
	release := make(chan struct{})
	pollerCommitted := make(chan struct{})

	wg.Add(2)
	// POLLER: forced down the 401 → coordinatedRefresh path and WINS the (uncontended)
	// rotation, committing generation 1 and reconciling its own read to the fresh token.
	go func() {
		defer wg.Done()
		<-release
		pollerReading, pollerErr = f.svc.CollectCodexAccountUsage(env.ctx, f.userID, f.accountID)
		close(pollerCommitted)
	}()
	// WORKER: a stale observed generation (0) whose coordinated refresh is issued after the
	// poll-driven refresh committed generation 1 — it must RECONCILE to the committed token
	// with NO second exchange.
	go func() {
		defer wg.Done()
		<-release
		<-pollerCommitted
		workerRes, workerErr = f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capw, uuid.New(), 0)
	}()
	close(release)
	wg.Wait()

	if pollerErr != nil {
		t.Fatalf("poller refresh: %v", pollerErr)
	}
	if workerErr != nil {
		t.Fatalf("worker refresh: %v", workerErr)
	}

	// Exactly ONE provider token exchange across BOTH callers (the loser reconciles).
	if got := fake.exchangeCount(); got != 1 {
		t.Fatalf("provider token exchanges = %d, want exactly 1 (one rotation shared by both paths)", got)
	}

	// The account advanced by EXACTLY ONE generation and settled.
	acct := env.mustAccount(t, f.userID, f.accountID)
	if acct.Generation != 1 {
		t.Fatalf("generation = %d, want 1 (advanced by exactly one)", acct.Generation)
	}
	if acct.CoordState != codexCoordIdle {
		t.Fatalf("coord_state = %q, want idle (settled after the single committed rotation)", acct.CoordState)
	}

	// The poller converged: its reading was captured under the single committed generation.
	if pollerReading.ObservedGeneration != 1 {
		t.Fatalf("poller observed generation = %d, want 1 (the committed rotation)", pollerReading.ObservedGeneration)
	}
	if pollerReading.ObservedCredentialRevision != 0 {
		t.Fatalf("poller observed credential_revision = %d, want 0", pollerReading.ObservedCredentialRevision)
	}

	// The worker reconciled to the winner's committed token — never a second exchange.
	if workerRes.Outcome != CodexRefreshReconciled {
		t.Fatalf("worker outcome = %v, want reconciled (the loser must not exchange)", workerRes.Outcome)
	}
	if workerRes.Generation != 1 {
		t.Fatalf("worker generation = %d, want 1 (the single committed generation)", workerRes.Generation)
	}

	// NO access token released before the durable commit: the token handed to the worker IS
	// the durably-committed sealed login's token (read fresh from the DB), and it is a
	// rotated token, never the pre-rotation one.
	committed := env.accountBlob(t, f.userID, f.accountID).AccessToken
	if committed == "" || committed == f.accessToken {
		t.Fatalf("committed access token = %q, want a rotated token (not the pre-rotation %q)", committed, f.accessToken)
	}
	if workerRes.AccessToken != committed {
		t.Fatalf("worker token = %q, want the durably-committed token %q (a token must never be released before its commit)", workerRes.AccessToken, committed)
	}
	if acct.ReauthRequired {
		t.Fatal("a successful convergence must not set reauth_required")
	}
}
