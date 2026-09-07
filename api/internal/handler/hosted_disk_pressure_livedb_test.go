package handler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// These exercise the REAL SQL derivation of the poll's disk_pressure signal (PRD #837
// M4), which is the whole of M4's api logic and is invisible to a fake store: the
// >=2-heartbeat debounce lives in the HeartbeatWorker CASE + the
// ListHostedWorkersForController predicate, and the reset-on-register lives in
// RegisterWorker — none of which a fake can exhibit. Skipped unless
// UZI_TEST_DATABASE_URL is set (run via ./e2e/run-store-it.sh).

const diskPressureThreshold = 0.90

// diskStats builds a heartbeat sample whose /nix volume is at the given used/total
// fraction; total is fixed at 100 so `used` reads as a percent.
func diskStats(usedNixPct int64) *workersvc.WorkerStats {
	total := int64(100)
	return &workersvc.WorkerStats{
		MemBytes:          1,
		Source:            "cgroup",
		DiskNixBytes:      &usedNixPct,
		DiskNixTotalBytes: &total,
	}
}

// TestPollDiskPressureDebouncesAtTwoLiveDB walks the streak: one over-threshold heartbeat
// is NOT enough (debounce at 2, not 1), a second one crosses, and a subsequent
// under-threshold sample resets it back to false. This is the core M4 derivation.
func TestPollDiskPressureDebouncesAtTwoLiveDB(t *testing.T) {
	h, _, q, box, userID := hostedLiveDB(t, "5")
	ctx := context.Background()
	wsvc := workersvc.New(q, box, workersvc.Params{DiskPressureThreshold: diskPressureThreshold})
	// A generous staleness bound: every heartbeat below stamps last_heartbeat_at=now(), so
	// freshness is never the variable here — the streak is.
	poll := hostedsvc.New(q, box, time.Now, time.Hour)

	wkr, err := h.provisionHostedWorker(ctx, userID, "debounce", "base", "m", false, 5)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// One over-threshold heartbeat: streak=1, still under the >=2 debounce.
	if _, err := wsvc.Heartbeat(ctx, wkr, diskStats(95)); err != nil {
		t.Fatalf("heartbeat 1: %v", err)
	}
	if diskPressureOf(ctx, t, poll, wkr.ID) {
		t.Fatal("disk_pressure true after ONE over-threshold heartbeat; the debounce must require >=2")
	}

	// Second over-threshold heartbeat: streak=2, now under pressure.
	if _, err := wsvc.Heartbeat(ctx, wkr, diskStats(95)); err != nil {
		t.Fatalf("heartbeat 2: %v", err)
	}
	if !diskPressureOf(ctx, t, poll, wkr.ID) {
		t.Fatal("disk_pressure false after TWO consecutive over-threshold heartbeats; the debounce should have fired")
	}

	// An under-threshold heartbeat resets the streak to 0: any non-pressured sample
	// breaks the run, so pressure clears immediately.
	if _, err := wsvc.Heartbeat(ctx, wkr, diskStats(50)); err != nil {
		t.Fatalf("heartbeat 3 (under): %v", err)
	}
	if diskPressureOf(ctx, t, poll, wkr.ID) {
		t.Fatal("disk_pressure still true after an under-threshold heartbeat; the streak did not reset")
	}
}

// TestPollDiskPressureResetOnRegisterLiveDB proves the RESET is RegisterWorker's, not a
// side effect of the HeartbeatWorker CASE: a worker at streak=2 (pressured) that then
// REGISTERS — a fresh pod incarnation — must read false on the very next poll even though
// its last_heartbeat_at is still fresh (register stamps it now()). If the reset lived only
// in the heartbeat CASE this would stay true.
func TestPollDiskPressureResetOnRegisterLiveDB(t *testing.T) {
	h, _, q, box, userID := hostedLiveDB(t, "5")
	ctx := context.Background()
	wsvc := workersvc.New(q, box, workersvc.Params{DiskPressureThreshold: diskPressureThreshold})
	poll := hostedsvc.New(q, box, time.Now, time.Hour)

	wkr, err := h.provisionHostedWorker(ctx, userID, "register-reset", "base", "m", false, 5)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Get it pressured (streak=2).
	for i := 0; i < 2; i++ {
		if _, err := wsvc.Heartbeat(ctx, wkr, diskStats(95)); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	if !diskPressureOf(ctx, t, poll, wkr.ID) {
		t.Fatal("precondition: worker should be under pressure after two over-threshold heartbeats")
	}

	// A fresh pod incarnation registers — this stamps last_heartbeat_at=now() (so the
	// worker stays FRESH) AND resets the streak to 0.
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{
		ID:      wkr.ID,
		Version: pgtype.Text{String: "1.0.0", Valid: true},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if diskPressureOf(ctx, t, poll, wkr.ID) {
		t.Fatal("disk_pressure still true after RegisterWorker on a still-fresh worker; the register-reset is missing " +
			"(a HeartbeatWorker-only reset would not clear a streak the register was supposed to)")
	}
}

// TestPollDiskPressureFreshnessGateLiveDB proves the freshness leg: a worker at streak=2
// whose last_heartbeat_at has fallen past the poll's staleness cutoff must read false — a
// silent worker cannot keep asserting pressure off a frozen streak.
func TestPollDiskPressureFreshnessGateLiveDB(t *testing.T) {
	h, pool, q, box, userID := hostedLiveDB(t, "5")
	ctx := context.Background()
	wsvc := workersvc.New(q, box, workersvc.Params{DiskPressureThreshold: diskPressureThreshold})

	wkr, err := h.provisionHostedWorker(ctx, userID, "freshness", "base", "m", false, 5)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := wsvc.Heartbeat(ctx, wkr, diskStats(95)); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}

	// Fresh poll (1h bound): pressured.
	fresh := hostedsvc.New(q, box, time.Now, time.Hour)
	if !diskPressureOf(ctx, t, fresh, wkr.ID) {
		t.Fatal("precondition: worker should read pressured while its heartbeat is fresh")
	}

	// Shove last_heartbeat_at into the past, beyond the poll's cutoff. The streak column is
	// untouched — this isolates the freshness leg from the streak leg.
	if _, err := pool.Exec(ctx, `UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, wkr.ID); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	// A 1-minute staleness bound now puts the worker past the cutoff.
	stale := hostedsvc.New(q, box, time.Now, time.Minute)
	if diskPressureOf(ctx, t, stale, wkr.ID) {
		t.Fatal("disk_pressure true for a worker whose last_heartbeat_at is past the cutoff; the freshness gate is missing")
	}
}

// diskPressureOf polls and returns the derived disk_pressure for one worker.
func diskPressureOf(ctx context.Context, t *testing.T, svc *hostedsvc.Service, id uuid.UUID) bool {
	t.Helper()
	resp, err := svc.Poll(ctx)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	for _, dw := range resp.Workers {
		if dw.ID == id.String() {
			return dw.DiskPressure
		}
	}
	t.Fatalf("worker %s absent from poll", id)
	return false
}
