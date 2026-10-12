package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// #2735: run_extension_cap_seconds is a PER-REQUEST maximum. These tests pin CreateExtendInput's
// two bounds - `secs <= cap` and the technical extension-admission bound
// (workersvc.MaxRunWallSeconds) on how far an extend may raise the reserve-based total - against a
// real Postgres.

func extendParams(id uuid.UUID, secs, capv int32, body string) store.CreateExtendInputParams {
	return store.CreateExtendInputParams{
		ID: id, Secs: secs, Cap: capv, BaseWallUpperSeconds: extendTestBaseWallUpper,
		FinalizeAllowanceSeconds: extendTestFinalizeAllowance, MaxWallSeconds: extendTestMaxWall, Body: extendBody(body),
	}
}

// TestCreateExtendInputPerRequestCapLiveDB: the cumulative extension is not capped; each request
// is. Past the old cumulative cap (57000 + 3600 > 57600) applies, three 16h grants reach 48h, and
// the sweep deadline follows the real total.
func TestCreateExtendInputPerRequestCapLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)

	t.Run("past the old cumulative cap", func(t *testing.T) {
		runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running", 8*60*60, 0, 57000, time.Now())
		total, err := q.CreateExtendInput(ctx, extendParams(runID, 3600, 57600, "extend +1h (max 16h per extension)"))
		if err != nil {
			t.Fatalf("CreateExtendInput past the old cumulative cap: %v", err)
		}
		if total != 60600 || extendColumn(ctx, t, pool, runID) != 60600 {
			t.Fatalf("total = %d column = %d, want 60600", total, extendColumn(ctx, t, pool, runID))
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 1 {
			t.Fatalf("audit rows = %d, want 1", n)
		}
	})

	t.Run("repeated 16h grants and the sweep deadline", func(t *testing.T) {
		runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running", 8*60*60, 0, 0, time.Now().Add(-(8*time.Hour + 47*time.Hour)))
		var total int32
		for i := 0; i < 3; i++ {
			var err error
			if total, err = q.CreateExtendInput(ctx, extendParams(runID, 57600, 57600, "extend +16h")); err != nil {
				t.Fatalf("grant %d: %v", i+1, err)
			}
		}
		if total != 172800 {
			t.Fatalf("total after three 16h grants = %d, want 172800 (48h)", total)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 3 {
			t.Fatalf("audit rows = %d, want 3", n)
		}
		staleWorker := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
			 VALUES ($1, $2, 'stale', $3, 'offline', now() - interval '10 years')`, staleWorker, userID, staleWorker[:])
		mustExec(ctx, t, pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, runID, staleWorker)
		staleCutoff := pgtype.Timestamptz{Time: time.Now().UTC().Add(-5 * time.Minute), Valid: true}
		sweep := func() bool {
			now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
			if _, err := q.RequestWallParks(ctx, store.RequestWallParksParams{Now: now, GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: staleCutoff}); err != nil {
				t.Fatalf("RequestWallParks: %v", err)
			}
			parked, err := q.ParkRunsAtWall(ctx, store.ParkRunsAtWallParams{Now: now, GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: staleCutoff, GraceSeconds: 600})
			if err != nil {
				t.Fatalf("ParkRunsAtWall: %v", err)
			}
			for _, p := range parked {
				if p.ID == runID {
					return true
				}
			}
			return false
		}
		// 55h active is below 8h + 48h = 56h: not parked. Past 56h: parked.
		if sweep() {
			t.Fatal("run parked at 55h active despite 48h of extensions (effective 56h wall)")
		}
		mustExec(ctx, t, pool, `UPDATE runs SET started_at = $2 WHERE id = $1`, runID,
			pgtype.Timestamptz{Time: time.Now().Add(-(56*time.Hour + time.Minute)), Valid: true})
		if !sweep() {
			t.Fatal("run NOT parked at 56h01m active (past the 8h + 48h extended wall)")
		}
	})
}

// TestCreateExtendInputRequestOverCapLiveDB: secs > cap matches 0 rows even with room to spare.
func TestCreateExtendInputRequestOverCapLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)
	runID := extendSeedIssueRun(ctx, t, pool, userID, repoID, "running", 8*60*60, 0, 0, time.Now())
	if _, err := q.CreateExtendInput(ctx, extendParams(runID, 57660, 57600, "refused")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("secs > cap err = %v, want pgx.ErrNoRows", err)
	}
	if extendColumn(ctx, t, pool, runID) != 0 || len(extendAuditRows(ctx, t, pool, runID)) != 0 {
		t.Fatal("a refused over-cap request changed the column or wrote an audit row")
	}
	// secs == cap applies, and the time is actually granted.
	total, err := q.CreateExtendInput(ctx, extendParams(runID, 57600, 57600, "ok"))
	if err != nil {
		t.Fatalf("secs == cap: %v", err)
	}
	if total != 57600 || extendColumn(ctx, t, pool, runID) != 57600 {
		t.Fatalf("secs == cap returned total %d, stored %d; want 57600 both", total, extendColumn(ctx, t, pool, runID))
	}
}

// seedBoundRun inserts a running issue run with the given persisted wall (nil = NULL), extension
// and finalize, for the room-bound tests.
func seedBoundRun(t *testing.T, fx extendBoundFixture, wall *int32, ext, finalize int32) uuid.UUID {
	t.Helper()
	runID := extendSeedIssueRun(fx.ctx, t, fx.pool, fx.userID, fx.repoID, "running", 3600, 0, int(ext), time.Now())
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET budget_wall_seconds = $2, budget_finalize_seconds = $3 WHERE id = $1`,
		runID, nullInt32(wall), finalize)
	return runID
}

type extendBoundFixture struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
}

// TestExtendBoundGoHelperAgreesWithSQLLiveDB: at the fenceposts, workersvc.ExtensionRoomSeconds
// and CreateExtendInput's predicate agree - the helper reports the literal room, a request of
// exactly that room applies in SQL, and one minute more is refused by both. The rooms are written
// out as literals (2147483 minus the upper wall, finalize reserve and extension), not recomputed.
func TestExtendBoundGoHelperAgreesWithSQLLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)
	fx := extendBoundFixture{ctx, pool, q, userID, repoID}
	i32 := func(v int32) *int32 { return &v }
	for _, tc := range []struct {
		name          string
		wall          *int32
		ext, finalize int32
		room          int32
	}{
		{"NULL wall", nil, 1881483, 0, 5000},
		{"small persisted wall", i32(28800), 1881483, 0, 5000},
		{"persisted wall at the 72h maximum ceiling", i32(259200), 1881483, 0, 5000},
		{"finalize allowance already granted (reserve equal)", i32(3600), 1881483, 1800, 5000},
		{"finalize above the reserve", i32(3600), 1876083, 7200, 5000},
		{"no room left", i32(3600), 1886483, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wall := pgtype.Int4{}
			if tc.wall != nil {
				wall = pgtype.Int4{Int32: *tc.wall, Valid: true}
			}
			if got := workersvc.ExtensionRoomSeconds(wall, tc.ext, tc.finalize, 2*time.Hour, 24*time.Hour); got != int(tc.room) {
				t.Fatalf("ExtensionRoomSeconds = %d, want %d", got, tc.room)
			}
			over := seedBoundRun(t, fx, tc.wall, tc.ext, tc.finalize)
			if _, err := q.CreateExtendInput(ctx, extendParams(over, tc.room+60, 604800, "over")); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("room+60 err = %v, want pgx.ErrNoRows (Go room is %d)", err, tc.room)
			}
			if got := extendColumn(ctx, t, pool, over); got != tc.ext {
				t.Fatalf("refused request changed the column to %d, want %d", got, tc.ext)
			}
			if tc.room == 0 {
				return
			}
			fill := seedBoundRun(t, fx, tc.wall, tc.ext, tc.finalize)
			total, err := q.CreateExtendInput(ctx, extendParams(fill, tc.room, 604800, "fill"))
			if err != nil {
				t.Fatalf("a request of exactly the room (%d) err = %v, want applied", tc.room, err)
			}
			if int(total) != int(tc.ext)+int(tc.room) {
				t.Fatalf("total = %d, want %d", total, int(tc.ext)+int(tc.room))
			}
			if _, err := q.CreateExtendInput(ctx, extendParams(fill, 60, 604800, "past")); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("+60 after filling err = %v, want pgx.ErrNoRows", err)
			}
		})
	}
}

// TestCreateExtendInputConcurrentNearBoundLiveDB: two extends that each fit alone but not
// together serialize on the row lock and exactly one applies (the predicate is re-evaluated on
// the committed row, not on a stale read).
func TestCreateExtendInputConcurrentNearBoundLiveDB(t *testing.T) {
	ctx, pool, q := extendSteeringDB(t)
	userID, repoID := extendSeedRepo(ctx, t, pool)
	fx := extendBoundFixture{ctx, pool, q, userID, repoID}
	for round := 0; round < 5; round++ {
		// 5000s of room; two requests of 3000s.
		runID := seedBoundRun(t, fx, nil, 1881483, 0)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = q.CreateExtendInput(ctx, extendParams(runID, 3000, 604800, "concurrent"))
			}()
		}
		close(start)
		wg.Wait()
		applied, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				applied++
			case errors.Is(err, pgx.ErrNoRows):
				refused++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if applied != 1 || refused != 1 {
			t.Fatalf("round %d: applied=%d refused=%d, want exactly one of each", round, applied, refused)
		}
		if got := extendColumn(ctx, t, pool, runID); got != 1881483+3000 {
			t.Fatalf("round %d: column = %d, want %d", round, got, 1881483+3000)
		}
		if n := len(extendAuditRows(ctx, t, pool, runID)); n != 1 {
			t.Fatalf("round %d: audit rows = %d, want 1", round, n)
		}
	}
}
