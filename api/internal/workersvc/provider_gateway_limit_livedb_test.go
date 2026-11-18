package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_gateway_limit_livedb_test.go is the PRD #4320 module 5 gateway-limit contract against a
// real Postgres: the 1m / 5m / 15m / owner-wait schedule keyed on model_provider_limit_streak, the
// sweeper never promoting the owner-wait row, the owner Resume query, and the park never touching a
// vendor pool. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// gwPark claims the fixture's provider run, starts it and parks it on a gateway limit at the pinned
// clock, reporting a VENDOR-STYLE type and reset that the server must ignore.
func gwPark(t *testing.T, pf *pcFix, runID uuid.UUID, now time.Time) store.Run {
	t.Helper()
	pf.svc.now = func() time.Time { return now }
	w := pf.worker(t)
	payload, err := pf.svc.Claim(pf.env.ctx, w, nil)
	if err != nil || payload == nil || payload.RunID != runID.String() {
		t.Fatalf("Claim = (%v, %v)", payload != nil, err)
	}
	gen := payload.ClaimGeneration
	if _, applied, err := pf.svc.SetState(pf.env.ctx, w, runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("start: applied=%v err=%v", applied, err)
	}
	reset := now.Add(5 * time.Hour).UnixMilli()
	r, applied, err := pf.svc.SetState(pf.env.ctx, w, runID, StateRequest{State: "limit_wait", ClaimGeneration: &gen, LimitResetsAt: &reset, RateLimitType: strPtr("five_hour")})
	if err != nil || !applied || r.Status != "limit_wait" {
		t.Fatalf("park: status=%s applied=%v err=%v", r.Status, applied, err)
	}
	return mustRun(t, pf.env, runID)
}

func gwRun(t *testing.T, pf *pcFix, streak int32) uuid.UUID {
	t.Helper()
	id := pf.run(t)
	pf.env.exec(`UPDATE runs SET wait_on_limit = true, model_provider_limit_streak = $2 WHERE id = $1`, id, streak)
	return id
}

func TestProviderGatewayLimitScheduleLiveDB(t *testing.T) {
	pf := newPCFix(t)
	// The owner holds a vendor pool with a gauge; a provider park must leave all of it alone.
	alt := pf.env.seedAnthropicSecret(t, pf.userID, "pool-"+uuid.NewString(), false)
	pf.env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, alt)
	pf.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
	    VALUES ($1, $2, 20, 10, 'usage_endpoint', now())`, alt, pf.userID)
	// The retry budget and ceiling must not touch the owner-wait row: both are at their strictest.
	pf.svc.p.RunLimitMaxWaits = 0
	pf.svc.p.RunLimitMaxPark = time.Second
	observed := &codexParkStore{Queries: pf.env.q}
	pf.svc.q = observed

	base := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		before int32
		retry  time.Duration // 0: NULL
	}{
		{0, time.Minute}, {1, 5 * time.Minute}, {2, 15 * time.Minute}, {3, 0}, {4, 0},
	}
	for _, c := range cases {
		pf.env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'queued') AND user_id = $1`, pf.userID)
		runID := gwRun(t, pf, c.before)
		r := gwPark(t, pf, runID, base)
		if r.ModelProviderLimitStreak != c.before+1 {
			t.Errorf("streak before %d: stored streak %d, want %d", c.before, r.ModelProviderLimitStreak, c.before+1)
		}
		if !r.RateLimitType.Valid || r.RateLimitType.String != RateLimitTypeProviderGatewayLimit || r.LimitResetsAt.Valid {
			t.Errorf("streak before %d: type=%v resets=%v, want provider_gateway_limit and no reset", c.before, r.RateLimitType, r.LimitResetsAt)
		}
		if c.retry == 0 {
			if r.RetryNotBefore.Valid {
				t.Errorf("streak before %d: retry_not_before = %v, want NULL (owner wait)", c.before, r.RetryNotBefore.Time)
			}
		} else if !r.RetryNotBefore.Valid || !r.RetryNotBefore.Time.Equal(base.Add(c.retry)) {
			t.Errorf("streak before %d: retry_not_before = %v, want %v", c.before, r.RetryNotBefore, base.Add(c.retry))
		}
		if r.AnthropicSecretID.Valid || r.LimitDeadSecretID.Valid || r.LimitWaitCount != 1 {
			t.Errorf("streak before %d: vendor columns touched: anthropic=%v dead=%v waits=%d", c.before, r.AnthropicSecretID, r.LimitDeadSecretID, r.LimitWaitCount)
		}
	}
	if observed.candidates != 0 || observed.five != 0 || observed.seven != 0 {
		t.Errorf("vendor pool touched: candidates=%d five=%d seven=%d", observed.candidates, observed.five, observed.seven)
	}
	var five, seven float64
	var rejected int
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT five_hour_pct, seven_day_pct FROM anthropic_rate_limits WHERE user_secret_id = $1`, alt).Scan(&five, &seven); err != nil || five != 20 || seven != 10 {
		t.Errorf("gauge = (%v, %v, %v), want it untouched at (20, 10)", five, seven, err)
	}
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT count(*) FROM user_secrets WHERE user_id = $1 AND anthropic_rejected_at IS NOT NULL`, pf.userID).Scan(&rejected); err != nil || rejected != 0 {
		t.Errorf("rejected credentials = %d (%v), want 0", rejected, err)
	}
}

// TestProviderGatewayLimitSweeperLiveDB: the sweeper promotes rows 1-3 at their time, keeps the streak
// across the promotion, and NEVER promotes the NULL owner-wait row however late the clock.
func TestProviderGatewayLimitSweeperLiveDB(t *testing.T) {
	pf := newPCFix(t)
	pf.env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'pool_wait')`)
	base := time.Now().UTC().Truncate(time.Second)
	for _, c := range []struct {
		before int32
		retry  time.Duration
	}{{0, time.Minute}, {1, 5 * time.Minute}, {2, 15 * time.Minute}} {
		runID := gwRun(t, pf, c.before)
		gwPark(t, pf, runID, base)
		pf.svc.now = func() time.Time { return base.Add(c.retry - time.Second) }
		if _, err := pf.svc.Sweep(pf.env.ctx); err != nil {
			t.Fatal(err)
		}
		if got := statusOf(t, pf.env, runID); got != "limit_wait" {
			t.Fatalf("streak before %d: status %s one second early, want limit_wait", c.before, got)
		}
		pf.svc.now = func() time.Time { return base.Add(c.retry) }
		if _, err := pf.svc.Sweep(pf.env.ctx); err != nil {
			t.Fatal(err)
		}
		r := mustRun(t, pf.env, runID)
		if r.Status != "queued" || r.ModelProviderLimitStreak != c.before+1 {
			t.Fatalf("streak before %d: after promotion status=%s streak=%d, want queued and %d kept", c.before, r.Status, r.ModelProviderLimitStreak, c.before+1)
		}
		pf.env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, runID)
	}
	runID := gwRun(t, pf, 3)
	gwPark(t, pf, runID, base)
	pf.svc.now = func() time.Time { return base.Add(10000 * time.Hour) }
	for i := 0; i < 2; i++ {
		if _, err := pf.svc.Sweep(pf.env.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if r := mustRun(t, pf.env, runID); r.Status != "limit_wait" || r.RetryNotBefore.Valid {
		t.Fatalf("owner-wait row: status=%s retry=%v, want it parked with NULL retry after a late sweep", r.Status, r.RetryNotBefore)
	}
}

// TestProviderGatewayLimitOwnerResumeLiveDB: the owner Resume query re-queues a gateway park from any
// row, resets the streak and clears retry_not_before, refuses a vendor limit_wait, and the run claims
// again and parks on the first row of the schedule.
func TestProviderGatewayLimitOwnerResumeLiveDB(t *testing.T) {
	pf := newPCFix(t)
	base := time.Now().UTC().Truncate(time.Second)
	for before := int32(0); before <= 4; before++ {
		pf.env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'queued') AND user_id = $1`, pf.userID)
		runID := gwRun(t, pf, before)
		gwPark(t, pf, runID, base)
		rows, err := pf.env.q.ResumeProviderGatewayLimitWait(pf.env.ctx, store.ResumeProviderGatewayLimitWaitParams{ID: runID, UserID: pf.userID})
		if err != nil || rows != 1 {
			t.Fatalf("streak before %d: resume rows=%d err=%v", before, rows, err)
		}
		r := mustRun(t, pf.env, runID)
		if r.Status != "queued" || r.ModelProviderLimitStreak != 0 || r.RetryNotBefore.Valid || r.StartedAt.Valid {
			t.Fatalf("streak before %d: after resume status=%s streak=%d retry=%v started=%v", before, r.Status, r.ModelProviderLimitStreak, r.RetryNotBefore, r.StartedAt)
		}
		if rows, err := pf.env.q.ResumeProviderGatewayLimitWait(pf.env.ctx, store.ResumeProviderGatewayLimitWaitParams{ID: runID, UserID: pf.userID}); err != nil || rows != 0 {
			t.Fatalf("second resume rows=%d err=%v, want a 0-row no-op", rows, err)
		}
		if rows, err := pf.env.q.ResumeProviderGatewayLimitWait(pf.env.ctx, store.ResumeProviderGatewayLimitWaitParams{ID: runID, UserID: uuid.New()}); err != nil || rows != 0 {
			t.Fatalf("foreign resume rows=%d err=%v, want 0", rows, err)
		}
	}
	// After a reset the next gateway park is row 0 again: one minute.
	pf.env.exec(`UPDATE runs SET status = 'completed' WHERE status IN ('limit_wait', 'queued') AND user_id = $1`, pf.userID)
	runID := gwRun(t, pf, 4)
	gwPark(t, pf, runID, base)
	if _, err := pf.env.q.ResumeProviderGatewayLimitWait(pf.env.ctx, store.ResumeProviderGatewayLimitWaitParams{ID: runID, UserID: pf.userID}); err != nil {
		t.Fatal(err)
	}
	r := gwPark(t, pf, runID, base)
	if !r.RetryNotBefore.Valid || !r.RetryNotBefore.Time.Equal(base.Add(time.Minute)) || r.ModelProviderLimitStreak != 1 {
		t.Fatalf("park after resume: retry=%v streak=%d, want +1m and streak 1", r.RetryNotBefore, r.ModelProviderLimitStreak)
	}

	// A vendor limit_wait (and a provider limit_wait of another type) stays unresumable here.
	vendor := pf.queuedRun(t, "issue")
	pf.env.exec(`UPDATE runs SET status = 'limit_wait', rate_limit_type = 'five_hour', retry_not_before = now() + interval '1 hour', limit_wait_count = 1 WHERE id = $1`, vendor)
	if rows, err := pf.env.q.ResumeProviderGatewayLimitWait(pf.env.ctx, store.ResumeProviderGatewayLimitWaitParams{ID: vendor, UserID: pf.userID}); err != nil || rows != 0 {
		t.Fatalf("vendor limit_wait resumed: rows=%d err=%v", rows, err)
	}
	pf.env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, vendor)
}

// TestProviderGatewayLimitOptOutFailsLiveDB: a run that does not wait on limits fails with the
// rate_limited origin instead of parking, and the owner-wait/budget rules do not apply to it.
func TestProviderGatewayLimitOptOutFailsLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := pf.run(t)
	pf.env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, runID)
	w := pf.worker(t)
	payload, err := pf.svc.Claim(pf.env.ctx, w, nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim = (%v, %v)", payload != nil, err)
	}
	gen := payload.ClaimGeneration
	if _, applied, err := pf.svc.SetState(pf.env.ctx, w, runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("start: applied=%v err=%v", applied, err)
	}
	r, applied, err := pf.svc.SetState(pf.env.ctx, w, runID, StateRequest{State: "limit_wait", ClaimGeneration: &gen})
	if err != nil || !applied || r.Status != "failed" || r.FailOrigin.String != "rate_limited" {
		t.Fatalf("opt-out park: status=%s origin=%v applied=%v err=%v", r.Status, r.FailOrigin, applied, err)
	}
}

// TestProviderGatewayLimitOwnerWaitCancelsLiveDB: the owner-wait row (streak 4, NULL retry) is not a
// dead end: its owner can Cancel it.
func TestProviderGatewayLimitOwnerWaitCancelsLiveDB(t *testing.T) {
	pf := newPCFix(t)
	runID := gwRun(t, pf, 3)
	r := gwPark(t, pf, runID, time.Now().UTC().Truncate(time.Second))
	if r.Status != "limit_wait" || r.RetryNotBefore.Valid {
		t.Fatalf("precondition: status=%s retry=%v", r.Status, r.RetryNotBefore)
	}
	if _, err := pf.svc.SubmitInput(pf.env.ctx, pf.userID, runID, "cancel", "", nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := statusOf(t, pf.env, runID); got != "cancelled" {
		t.Fatalf("status = %s, want cancelled", got)
	}
}
