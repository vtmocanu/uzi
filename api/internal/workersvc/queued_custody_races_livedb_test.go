package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func seededQueuedCustody(t *testing.T, max, usage int, gen int64, generations []int64, limit int) (*claimantExhaustion, uuid.UUID) {
	t.Helper()
	env := setupCodexLiveDB(t)
	c := newClaimantExhaustion(t, env, true, max)
	queuedCustodyCache(t, env, c.svc, limit)
	id := c.seedRun("queued", gen)
	env.exec("UPDATE runs SET worker_id=$2,claimed_at=now(),requeue_count=$3 WHERE id=$1", id, c.workerID, usage)
	for _, g := range generations {
		c.seedHold(c.userID, id, g, "open")
	}
	return c, id
}

func freezeQueuedCustody(t *testing.T, c *claimantExhaustion) (pgx.Tx, *store.Queries, store.Worker, workerRecoveryLockSet, int32) {
	t.Helper()
	limitBefore := c.svc.queuedCustodyCachedLimit(c.env.ctx)
	tx, err := c.env.pool.Begin(c.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	limit, err := c.svc.queuedCustodyAdmission(c.env.ctx, tx, limitBefore)
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(tx)
	w, err := q.GetWorkerForUpdate(c.env.ctx, c.workerID)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := captureWorkerRecoveryLocksWithQueued(c.env.ctx, q, c.workerID, nil, validatedFinalizeResume{}, c.svc.queuedCustodyCandidates(limit))
	if err != nil {
		t.Fatal(err)
	}
	return tx, q, w, locks, limit
}

func runQueuedCustodyFrozen(t *testing.T, c *claimantExhaustion) int {
	t.Helper()
	tx, q, w, locks, limit := freezeQueuedCustody(t, c)
	rows, err := c.svc.parkQueuedCustody(c.env.ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(c.env.ctx); err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestQueuedCustodyExecutionAndMembershipGuardsLiveDB(t *testing.T) {
	for _, name := range []string{"ordinary", "generation_zero", "continuation_exempt", "future_hold", "older_holds_only", "live_execution", "terminal_pending", "overflow", "pending_all", "historical_finalize", "released_finalize", "zero_cap"} {
		t.Run(name, func(t *testing.T) {
			gen := int64(3)
			limit := 2
			usage := 3
			max := 3
			generations := []int64{1, 2}
			switch name {
			case "ordinary":
				generations = nil
			case "generation_zero":
				gen = 0
			case "continuation_exempt":
				generations = []int64{1}
			case "future_hold":
				generations = []int64{4, 5}
			case "zero_cap":
				max = 0
				usage = 0
			}
			c, id := seededQueuedCustody(t, max, usage, gen, generations, limit)
			switch name {
			case "live_execution", "terminal_pending", "pending_all":
				pending := name == "terminal_pending" || name == "pending_all"
				snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: exhaustionNonce, Active: []ActiveRunEntry{entry(id, gen, "running", pending)}}
				if name == "pending_all" {
					snap.PendingAll = []PendingOutcomeIdentity{{RunID: id.String(), ClaimGeneration: gen}}
				}
				if ok, err := applySnapshot(t, c.svc, c.env, c.workerID, snap, snapshotModeHeartbeat); err != nil || !ok {
					t.Fatalf("live snapshot=%v %v", ok, err)
				}
				if name == "terminal_pending" {
					c.env.exec("UPDATE worker_active_runs SET reported_at=now()-interval '1 hour' WHERE run_id=$1", id)
				}
			case "overflow":
				setOverflow(t, c.env, c.workerID, "1 hour")
			case "historical_finalize":
				c.env.exec("UPDATE runs SET finalize_resume_generation=2,requeue_count=4 WHERE id=$1", id)
			case "released_finalize":
				c.env.exec("UPDATE runs SET finalize_resume_generation=3,requeue_count=4,claim_released_at=now(),released_worker_id=$2,released_worker_nonce='released' WHERE id=$1", id, c.workerID)
			}
			before := observeQueuedCustody(t, c.env, id)
			got := runQueuedCustodyFrozen(t, c)
			want := 0
			if name == "older_holds_only" || name == "historical_finalize" || name == "released_finalize" || name == "zero_cap" {
				want = 1
			}
			if got != want {
				t.Fatalf("parked %d want %d", got, want)
			}
			after := observeQueuedCustody(t, c.env, id)
			requireQueuedCustodyPreserved(t, before, after, want == 1)
		})
	}
}

func TestQueuedCustodyClaimedRegistrationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	c := newClaimantExhaustion(t, env, true, 1)
	queuedCustodyCache(t, env, c.svc, 1)
	id := c.seedKind(t, "issue")
	payload, err := c.svc.Claim(env.ctx, c.wkr, nil)
	if err != nil || payload == nil || payload.RunID != id.String() {
		t.Fatalf("Claim=%+v %v", payload, err)
	}
	if statusOf(t, env, id) != "claimed" {
		t.Fatal("must not start executor")
	}
	queuedCustodyTrigger(t, c, "register")
	got := observeQueuedCustody(t, env, id)
	if string(got.run["status"]) != `"recovery_wait"` || string(got.run["requeue_count"]) != "1" {
		t.Fatal("registration missed newly exhausted claimed continuation")
	}
	c.requireAllHoldsOpen(t, id, 1)
}

func TestQueuedCustodyFinalizeGrantSurvivesCyclesLiveDB(t *testing.T) {
	// The grant is produced by real registration, never by seeding a generation mark.
	env := setupCodexLiveDB(t)
	c := newClaimantExhaustion(t, env, true, 1)
	queuedCustodyCache(t, env, c.svc, 1)
	id := c.seedKind(t, "issue")
	c.claimAndRun(t, c.wkr, id, 1, true)
	env.exec("UPDATE runs SET requeue_count=1 WHERE id=$1", id)
	if _, _, err := c.svc.Register(env.ctx, hbWorker(t, env, c.workerID), "fixture", "", nil, nil, c.wkr.ProtocolCapabilities, finalizeSnap(false, fin(id, 1))); err != nil {
		t.Fatal(err)
	}
	before := observeQueuedCustody(t, env, id)
	if string(before.run["status"]) != `"queued"` || string(before.run["finalize_resume_generation"]) != "1" || string(before.run["requeue_count"]) != "2" {
		t.Fatal("attested registration did not produce one-shot grant")
	}
	for cycle := 0; cycle < 2; cycle++ {
		for _, path := range []string{"heartbeat", "register", "sweep", "heartbeat"} {
			queuedCustodyTrigger(t, c, path)
		}
		after := observeQueuedCustody(t, env, id)
		requireQueuedCustodyPreserved(t, before, after, false)
	}
}

func TestQueuedCustodySettingsAndPoolLiveDB(t *testing.T) {
	for _, name := range []string{"stale_cache_increase", "malformed", "small_pool_expired_cache", "decreased_requeue_cap"} {
		t.Run(name, func(t *testing.T) {
			c, id := seededQueuedCustody(t, 3, 3, 1, []int64{1}, 1)
			switch name {
			case "stale_cache_increase":
				c.env.exec("UPDATE app_settings SET value='2' WHERE key='custody_hold_limit'")
			case "malformed":
				c.env.exec("UPDATE app_settings SET value='malformed' WHERE key='custody_hold_limit'")
			case "small_pool_expired_cache":
				cfg := c.env.pool.Config().Copy()
				cfg.MaxConns = 1
				cfg.MinConns = 0
				pool, err := pgxpool.NewWithConfig(c.env.ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				c.svc.SetTxBeginner(pool)
				c.svc.p.CustodyHoldLimit = settings.New(store.New(pool), 0)
			case "decreased_requeue_cap":
				c.svc.p.RunMaxRequeues = 2
			}
			ctx, cancel := context.WithTimeout(c.env.ctx, 5*time.Second)
			defer cancel()
			w := hbWorker(t, c.env, c.workerID)
			_, err := c.svc.Heartbeat(ctx, w, nil, nil, &ActiveSnapshot{SnapshotEpoch: w.SnapshotEpoch + 1, RegisterNonce: w.SnapshotRegisterNonce.String, Active: []ActiveRunEntry{}})
			if err != nil {
				t.Fatal(err)
			}
			want := "recovery_wait"
			if name == "stale_cache_increase" || name == "malformed" {
				want = "queued"
			}
			if got := statusOf(t, c.env, id); got != want {
				t.Fatalf("status=%s want %s", got, want)
			}
			c.requireAllHoldsOpen(t, id, 1)
		})
	}
}

func TestQueuedCustodyDiscardRaceLiveDB(t *testing.T) {
	for _, discardFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(discardFirst), func(t *testing.T) {
			c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
			tx, q, w, locks, limit := freezeQueuedCustody(t, c)
			var hold uuid.UUID
			if err := c.env.pool.QueryRow(c.env.ctx, "SELECT id FROM recovery_custody_holds WHERE run_id=$1", id).Scan(&hold); err != nil {
				t.Fatal(err)
			}
			rs := recovery.New(c.env.q, c.env.pool, c.env.box, recovery.Limits{}, time.Now)
			if discardFirst {
				if ok, err := rs.DiscardHold(c.env.ctx, c.userID, id, hold); err != nil || !ok {
					t.Fatalf("discard=%v %v", ok, err)
				}
				before := observeQueuedCustody(t, c.env, id)
				rows, err := c.svc.parkQueuedCustody(c.env.ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
				if err != nil || len(rows) != 0 {
					t.Fatalf("lost race=%+v %v", rows, err)
				}
				if err := tx.Commit(c.env.ctx); err != nil {
					t.Fatal(err)
				}
				requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
				if statusOf(t, c.env, id) != "queued" {
					t.Fatal("discard winning must leave ordinary admission available")
				}
			} else {
				rows, err := c.svc.parkQueuedCustody(c.env.ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
				if err != nil || len(rows) != 1 {
					t.Fatalf("park=%+v %v", rows, err)
				}
				ctx, cancel := context.WithTimeout(c.env.ctx, 5*time.Second)
				defer cancel()
				owner, err := c.env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = owner.Rollback(context.Background()) }()
				pid := owner.Conn().PgConn().PID()
				done := make(chan error, 1)
				joined := false
				defer func() {
					cancel()
					_ = tx.Rollback(context.Background())
					if !joined {
						if err := <-done; err != nil {
							t.Logf("race cleanup: %v", err)
						}
					}
				}()
				go func() {
					n, err := store.New(owner).DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: hold, RunID: id, UserID: c.userID, ReleaseEvidence: pgconv.TextOrNull("owner_discard")})
					if err == nil && n != 1 {
						err = fmt.Errorf("discard not applied")
					}
					if err == nil {
						err = owner.Commit(ctx)
					}
					done <- err
				}()
				waitQueuedOwnBackendLock(t, c.env, ctx, pid)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				resultErr := <-done
				joined = true
				if resultErr != nil {
					t.Fatal(resultErr)
				}

				if statusOf(t, c.env, id) != "recovery_wait" {
					t.Fatal("writer-first order did not retain owner hold")
				}
			}
			var state string
			if err := c.env.pool.QueryRow(c.env.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state); err != nil || state != "discarded" {
				t.Fatalf("lawful discard state=%s %v", state, err)
			}
		})
	}
}

func TestQueuedCustodyBoundedDiscoveryAndLostTupleLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
	c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", c.workerID)
	at := time.Now()
	rows, err := c.env.q.DiscoverStaleQueuedCustodyTargets(c.env.ctx, store.DiscoverStaleQueuedCustodyTargetsParams{AfterID: uuid.Nil, SweepStarted: pgconv.Time(at), Cutoff: pgconv.Time(at.Add(-10 * time.Minute)), MaxRequeues: 1, CustodyHoldLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var frozen []json.RawMessage
	for _, row := range rows {
		if row.ID == id {
			frozen = append(frozen, row.FrozenTarget)
		}
	}
	if len(frozen) != 1 {
		t.Fatal("candidate not discovered")
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.SubmitInputWithOptions(c.env.ctx, c.userID, id, "cancel", "owner won", nil, SubmitInputOptions{}); err != nil {
		t.Fatal(err)
	}
	before := observeQueuedCustody(t, c.env, id)
	changed, err := c.svc.parkStaleQueuedCustodyBatch(c.env.ctx, pgconv.Time(at.Add(-10*time.Minute)), at, []uuid.UUID{id}, []uuid.UUID{c.workerID}, raw)
	if err != nil || len(changed) != 0 {
		t.Fatalf("changed tuple=%+v %v", changed, err)
	}
	after := observeQueuedCustody(t, c.env, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale discovery changed owner-cancelled row or custody")
	}
}

func TestQueuedCustodyBatchBoundsLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
	for n := 0; n < 64; n++ {
		other := c.seedRun("queued", 1)
		c.env.exec("UPDATE runs SET worker_id=$2,claimed_at=now(),requeue_count=1 WHERE id=$1", other, c.workerID)
		c.seedHold(c.userID, other, 1, "open")
	}
	unrelated := c.seedRun("running", 1)
	c.env.exec("UPDATE runs SET worker_id=$2,claimed_at=now() WHERE id=$1", unrelated, c.workerID)
	c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", c.workerID)
	at := time.Now()
	cutoff := pgconv.Time(at.Add(-10 * time.Minute))
	page, err := c.env.q.DiscoverStaleQueuedCustodyTargets(c.env.ctx, store.DiscoverStaleQueuedCustodyTargetsParams{AfterID: uuid.Nil, SweepStarted: pgconv.Time(at), Cutoff: cutoff, MaxRequeues: 1, CustodyHoldLimit: 1})
	if err != nil || len(page) != 32 {
		t.Fatalf("bounded page=%d %v", len(page), err)
	}
	var ids []uuid.UUID
	var raw []json.RawMessage
	for _, row := range page {
		ids = append(ids, row.ID)
		raw = append(raw, row.FrozenTarget)
	}
	// A bounded parent query must never expand to the worker's other 34 runs.
	tx, err := c.env.pool.Begin(c.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.LockSettingsAdmission(c.env.ctx, tx); err != nil {
		t.Fatal(err)
	}
	q := store.New(tx)
	if _, err := q.GetWorkerForUpdate(c.env.ctx, c.workerID); err != nil {
		t.Fatal(err)
	}
	parents, err := q.LockWorkerRecoveryParents(c.env.ctx, store.LockWorkerRecoveryParentsParams{BoundedTargetIds: ids, AdditionalWorkerIds: []uuid.UUID{c.workerID}})
	if err != nil || len(parents) != 32 {
		t.Fatalf("bounded parents=%d %v", len(parents), err)
	}
	selected := make(map[uuid.UUID]bool)
	for _, v := range ids {
		selected[v] = true
	}
	for _, row := range parents {
		if !selected[row.ID] || row.ID == unrelated {
			t.Fatal("bounded discovery expanded")
		}
	}
	if err := tx.Rollback(c.env.ctx); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.svc.parkStaleQueuedCustodyBatch(c.env.ctx, cutoff, at, ids, []uuid.UUID{c.workerID}, encoded)
	if err != nil || len(first) != 32 {
		t.Fatalf("first page=%d %v", len(first), err)
	}
	rest, err := c.svc.parkStaleQueuedCustody(c.env.ctx, cutoff, at)
	if err != nil || rest != 33 {
		t.Fatalf("remaining pages=%d %v", rest, err)
	}
	again, err := c.svc.parkStaleQueuedCustody(c.env.ctx, cutoff, at)
	if err != nil || again != 0 {
		t.Fatalf("replay=%d %v", again, err)
	}
	if statusOf(t, c.env, unrelated) != "running" || statusOf(t, c.env, id) != "recovery_wait" {
		t.Fatal("batch crossed target scope")
	}
	var open int
	if err := c.env.pool.QueryRow(c.env.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE user_id=$1 AND state='open'", c.userID).Scan(&open); err != nil || open != 65 {
		t.Fatalf("retained holds=%d %v", open, err)
	}
}

func TestQueuedCustodyBoundedHoldSelectionLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 1, 1, 8, []int64{1, 2, 3, 4, 5, 6, 7, 8}, 2)
	before := observeQueuedCustody(t, c.env, id)
	tx, q, w, locks, limit := freezeQueuedCustody(t, c)
	ids, err := q.LockFrozenQueuedCustodyHolds(c.env.ctx, store.LockFrozenQueuedCustodyHoldsParams{FrozenTargets: locks.queuedJSON, LockedParentIds: locks.parentIDs, CustodyHoldLimit: limit})
	if err != nil || len(ids) != 2 {
		t.Fatalf("bounded locked holds=%d %v", len(ids), err)
	}
	rows, err := c.svc.parkQueuedCustodyWithHolds(c.env.ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks.queuedJSON, locks.parentIDs, ids, time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatalf("park=%d %v", len(rows), err)
	}
	if err := tx.Commit(c.env.ctx); err != nil {
		t.Fatal(err)
	}
	requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), true)
}

func waitQueuedOwnBackendLock(t *testing.T, env codexTestEnv, ctx context.Context, pid uint32) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := env.pool.QueryRow(ctx, "SELECT COALESCE((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)", pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("owned test transaction did not reach lock barrier")
		}
	}
}

func TestQueuedCustodyOwnerControlRaceLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
	ctx, cancel := context.WithTimeout(c.env.ctx, 10*time.Second)
	defer cancel()
	owner, err := c.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Rollback(context.Background()) }()
	if _, err := owner.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", id); err != nil {
		t.Fatal(err)
	}
	hook := &exhaustionTestBeginner{pool: c.env.pool, started: make(chan uint32, 1)}
	c.svc.SetTxBeginner(hook)
	w := hbWorker(t, c.env, c.workerID)
	done := make(chan error, 1)
	joined := false
	defer func() {
		cancel()
		_ = owner.Rollback(context.Background())
		if !joined {
			if err := <-done; err != nil {
				t.Logf("race cleanup: %v", err)
			}
		}
	}()
	go func() {
		_, err := c.svc.Heartbeat(ctx, w, nil, nil, &ActiveSnapshot{SnapshotEpoch: w.SnapshotEpoch + 1, RegisterNonce: w.SnapshotRegisterNonce.String, Active: []ActiveRunEntry{}})
		done <- err
	}()
	var pid uint32
	select {
	case pid = <-hook.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitQueuedOwnBackendLock(t, c.env, ctx, pid)
	before := observeQueuedCustody(t, c.env, id)
	if _, err := store.New(owner).CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: id, UserID: c.userID, StopReason: pgconv.TextOrNull("owner won")}); err != nil {
		t.Fatal(err)
	}
	var ownerRow []byte
	if err := owner.QueryRow(ctx, "SELECT to_jsonb(r) FROM runs r WHERE id=$1", id).Scan(&ownerRow); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ownerRow, &before.run); err != nil {
		t.Fatal(err)
	}
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	resultErr := <-done
	joined = true
	if resultErr != nil {
		t.Fatal(resultErr)
	}
	after := observeQueuedCustody(t, c.env, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("lost owner-control race changed cancelled state, evidence or custody")
	}
}

func TestQueuedCustodyClaimWinsDiscoveryLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
	c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1", c.workerID)
	at := time.Now()
	cutoff := pgconv.Time(at.Add(-10 * time.Minute))
	page, err := c.env.q.DiscoverStaleQueuedCustodyTargets(c.env.ctx, store.DiscoverStaleQueuedCustodyTargetsParams{AfterID: uuid.Nil, SweepStarted: pgconv.Time(at), Cutoff: cutoff, MaxRequeues: 1, CustodyHoldLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var frozen []json.RawMessage
	for _, r := range page {
		if r.ID == id {
			frozen = append(frozen, r.FrozenTarget)
		}
	}
	if len(frozen) != 1 {
		t.Fatal("candidate absent")
	}
	raw, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	var hold uuid.UUID
	if err := c.env.pool.QueryRow(c.env.ctx, "SELECT id FROM recovery_custody_holds WHERE run_id=$1", id).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if ok, err := recovery.New(c.env.q, c.env.pool, c.env.box, recovery.Limits{}, time.Now).DiscardHold(c.env.ctx, c.userID, id, hold); err != nil || !ok {
		t.Fatalf("discard=%v %v", ok, err)
	}
	second := seedSnapshotWorker(t, c.env, c.userID, "second")
	c.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", second, c.wkr.ProtocolCapabilities)
	p, err := c.svc.Claim(c.env.ctx, hbWorker(t, c.env, second), nil)
	if err != nil || p == nil || p.RunID != id.String() || p.ClaimGeneration != 2 {
		t.Fatalf("competing Claim=%+v %v", p, err)
	}
	before := observeQueuedCustody(t, c.env, id)
	rows, err := c.svc.parkStaleQueuedCustodyBatch(c.env.ctx, cutoff, at, []uuid.UUID{id}, []uuid.UUID{c.workerID}, raw)
	if err != nil || len(rows) != 0 {
		t.Fatalf("lost claim race=%+v %v", rows, err)
	}
	after := observeQueuedCustody(t, c.env, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale tuple changed successor claim, usage or holds")
	}
}

type queuedSettingsBarrier struct {
	pool    *pgxpool.Pool
	started chan uint32
}

func (b *queuedSettingsBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &queuedSettingsTx{Tx: tx, started: b.started}, nil
}

type queuedSettingsTx struct {
	pgx.Tx
	started chan uint32
}

func (tx *queuedSettingsTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "pg_advisory_xact_lock_shared") {
		select {
		case tx.started <- tx.Conn().PgConn().PID():
		case <-ctx.Done():
			return pgconn.CommandTag{}, ctx.Err()
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestQueuedCustodySettingsRaceLiveDB(t *testing.T) {
	for _, settingsFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(settingsFirst), func(t *testing.T) {
			c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
			ctx, cancel := context.WithTimeout(c.env.ctx, 10*time.Second)
			defer cancel()
			if settingsFirst {
				owner, err := c.env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = owner.Rollback(context.Background()) }()
				if _, err := owner.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.SettingsMutationLockKey); err != nil {
					t.Fatal(err)
				}
				if _, err := owner.Exec(ctx, "UPDATE app_settings SET value='2' WHERE key='custody_hold_limit'"); err != nil {
					t.Fatal(err)
				}
				before := observeQueuedCustody(t, c.env, id)
				hook := &queuedSettingsBarrier{pool: c.env.pool, started: make(chan uint32, 1)}
				c.svc.SetTxBeginner(hook)
				w := hbWorker(t, c.env, c.workerID)
				done := make(chan error, 1)
				joined := false
				defer func() {
					cancel()
					_ = owner.Rollback(context.Background())
					if !joined {
						if err := <-done; err != nil {
							t.Logf("race cleanup: %v", err)
						}
					}
				}()
				go func() {
					_, err := c.svc.Heartbeat(ctx, w, nil, nil, &ActiveSnapshot{SnapshotEpoch: w.SnapshotEpoch + 1, RegisterNonce: w.SnapshotRegisterNonce.String, Active: []ActiveRunEntry{}})
					done <- err
				}()
				var pid uint32
				select {
				case pid = <-hook.started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				waitQueuedOwnBackendLock(t, c.env, ctx, pid)
				if err := owner.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				resultErr := <-done
				joined = true
				if resultErr != nil {
					t.Fatal(resultErr)
				}
				requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
				if statusOf(t, c.env, id) != "queued" {
					t.Fatal("stale cache raised an owner hold")
				}
				c.svc.p.CustodyHoldLimit.(*settings.Cache).Invalidate()
				c.svc.SetTxBeginner(c.env.pool)
				queuedCustodyTrigger(t, c, "heartbeat")
				p, err := c.svc.Claim(c.env.ctx, hbWorker(t, c.env, c.workerID), nil)
				if err != nil || p == nil || p.RunID != id.String() {
					t.Fatalf("ordinary claim after capacity change=%+v %v", p, err)
				}
			} else {
				tx, q, w, locks, limit := freezeQueuedCustody(t, c)
				rows, err := c.svc.parkQueuedCustody(ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
				if err != nil || len(rows) != 1 {
					t.Fatalf("park=%d %v", len(rows), err)
				}
				owner, err := c.env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = owner.Rollback(context.Background()) }()
				pid := owner.Conn().PgConn().PID()
				done := make(chan error, 1)
				joined := false
				defer func() {
					cancel()
					_ = tx.Rollback(context.Background())
					if !joined {
						if err := <-done; err != nil {
							t.Logf("race cleanup: %v", err)
						}
					}
				}()
				go func() {
					_, err := owner.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", store.SettingsMutationLockKey)
					if err == nil {
						_, err = owner.Exec(ctx, "UPDATE app_settings SET value='2' WHERE key='custody_hold_limit'")
					}
					if err == nil {
						err = owner.Commit(ctx)
					}
					done <- err
				}()
				waitQueuedOwnBackendLock(t, c.env, ctx, pid)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				resultErr := <-done
				joined = true
				if resultErr != nil {
					t.Fatal(resultErr)
				}
				if statusOf(t, c.env, id) != "recovery_wait" {
					t.Fatal("later settings change rewrote committed disposition")
				}
				c.requireAllHoldsOpen(t, id, 1)
			}
		})
	}
}

func TestQueuedCustodyStaleReportFencesLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 3, 1, 3, []int64{1, 2}, 2)
	before := observeQueuedCustody(t, c.env, id)
	c.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,'credential_switch_v1') WHERE id=$1", c.workerID)
	w := hbWorker(t, c.env, c.workerID)
	if _, _, _, err := c.svc.SetStateReport(c.env.ctx, w, id, gateReq(i64(2), uid())); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale gate report err=%v", err)
	}
	requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
	oldNonce := w.SnapshotRegisterNonce.String
	queuedCustodyTrigger(t, c, "register")
	afterRegister := observeQueuedCustody(t, c.env, id)
	if _, err := c.svc.Heartbeat(c.env.ctx, w, nil, nil, &ActiveSnapshot{SnapshotEpoch: 999, RegisterNonce: oldNonce, Active: []ActiveRunEntry{}}); err != nil {
		t.Fatal(err)
	}
	requireQueuedCustodyPreserved(t, afterRegister, observeQueuedCustody(t, c.env, id), false)
}

func TestQueuedCustodyFinalizeProducerE2E(t *testing.T) {
	if os.Getenv("UZI_QUEUED_CUSTODY_FINALIZE_E2E") != "1" {
		t.Skip("opt in with UZI_QUEUED_CUSTODY_FINALIZE_E2E=1 on the provisioned Linux worker")
	}
	env := setupCodexLiveDB(t)
	c := newClaimantExhaustion(t, env, true, 1)
	queuedCustodyCache(t, env, c.svc, 1)
	id := c.seedKind(t, "issue")
	c.claimAndRun(t, c.wkr, id, 1, true)
	env.exec("UPDATE runs SET requeue_count=1 WHERE id=$1", id)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	// Produce the authenticated durable record through the real Runner seam,
	// reload it, then capture the real Worker's register transport. Aborting that
	// transport keeps the record unacknowledged; the exact body goes to real SQL below.
	source := `
import assert from "node:assert/strict";
import path from "node:path";
import {Outbox} from "./src/outbox.ts";
import {RunRunner} from "./src/runner.ts";
import {GitCache} from "./src/git.ts";
import {Worker} from "./src/worker.ts";
import {ActiveRunRegistry} from "./src/active-run-registry.ts";
const root=process.env.M1_OUTBOX_ROOT, id=process.env.M1_RUN_ID;
const log={debug(){},info(){},warn(){},error(){},addSecret(){},removeSecret(){},child(){return this;}};
const opts={root:path.join(root,"outbox"),log,runMaxBytes:1<<24,maxBytes:1<<25,retentionMs:86400000};
const outbox=new Outbox(opts);await outbox.init();
const git=new GitCache(path.join(root,"data"),log);
const runner=new RunRunner({},git,()=>{throw new Error("executor must not run");},log,1,undefined,{outbox});
await runner["journalFinalizeRecord"]({runId:id,claimGeneration:1},{text:"completed turn",sessionId:"fixture"});
assert.deepEqual(outbox.listPendingFinalizes(),[{run_id:id,claim_generation:1}]);
const reloaded=new Outbox(opts);await reloaded.init();
assert.deepEqual(reloaded.listPendingFinalizes(),[{run_id:id,claim_generation:1}]);
const controller=new AbortController();let captured;
const client={hasFeature:()=>false,register:async(...args)=>{captured={snapshot:args[5],capabilities:args[4]};controller.abort();return {};}};
const registry=new ActiveRunRegistry(()=>reloaded.listPendingTerminals(),()=>32);
const worker=new Worker({workerName:"fixture",workerTemplate:"base",maxConcurrentRuns:1,crossCheckSlots:0,dockerWiring:{}},client,runner,{},{},{},log,()=>({ok:true,missing:[]}),reloaded,new Map(),registry);
await worker["registerWithRetry"](controller.signal);
assert.deepEqual(captured.snapshot.finalize_resume,[{run_id:id,claim_generation:1}]);
assert.deepEqual(reloaded.listPendingFinalizes(),[{run_id:id,claim_generation:1}]);
process.stdout.write(JSON.stringify(captured));
`
	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "--input-type=module", "-e", source)
	cmd.Dir = filepath.Join(root, "agent")
	cmd.Env = append(os.Environ(), "M1_OUTBOX_ROOT="+t.TempDir(), "M1_RUN_ID="+id.String())
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("candidate finalize producer: %v, %.2000s", err, body)
	}
	var offered struct {
		Snapshot     ActiveSnapshot `json:"snapshot"`
		Capabilities []string       `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &offered); err != nil {
		t.Fatal(err)
	}
	if len(offered.Snapshot.FinalizeResume) != 1 || offered.Snapshot.FinalizeResume[0] != fin(id, 1) {
		t.Fatal("producer did not offer exact authenticated generation")
	}
	c.wkr.ProtocolCapabilities = offered.Capabilities
	if _, _, err := c.svc.Register(env.ctx, hbWorker(t, env, c.workerID), "fixture", "", nil, nil, offered.Capabilities, &offered.Snapshot); err != nil {
		t.Fatal(err)
	}
	before := observeQueuedCustody(t, env, id)
	if statusOf(t, env, id) != "queued" || string(before.run["finalize_resume_generation"]) != "1" || string(before.run["requeue_count"]) != "2" {
		t.Fatal("real finalize offer did not produce the usable grant")
	}
	for cycle := 0; cycle < 2; cycle++ {
		for _, path := range []string{"heartbeat", "register", "sweep", "heartbeat"} {
			queuedCustodyTrigger(t, c, path)
		}
		requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, env, id), false)
	}
}

func TestQueuedCustodyLiveDiscoveryBoundsLiveDB(t *testing.T) {
	c, _ := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
	for n := 0; n < 64; n++ {
		id := c.seedRun("queued", 1)
		c.env.exec("UPDATE runs SET worker_id=$2,claimed_at=now(),requeue_count=1 WHERE id=$1", id, c.workerID)
		c.seedHold(c.userID, id, 1, "open")
	}
	active := c.seedRun("running", 1)
	c.env.exec("UPDATE runs SET worker_id=$2,claimed_at=now() WHERE id=$1", active, c.workerID)
	for _, want := range []int{32, 32, 1} {
		tx, q, w, locks, limit := freezeQueuedCustody(t, c)
		if len(locks.parentIDs) != want+1 {
			t.Fatalf("new queued population must be bounded without dropping active parent: got %d want %d", len(locks.parentIDs), want+1)
		}
		rows, err := c.svc.parkQueuedCustody(c.env.ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
		if err != nil || len(rows) != want {
			t.Fatalf("page=%d want=%d err=%v", len(rows), want, err)
		}
		if err := tx.Commit(c.env.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if statusOf(t, c.env, active) != "running" {
		t.Fatal("changed existing active discovery")
	}
	var open int
	if err := c.env.pool.QueryRow(c.env.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE user_id=$1 AND state='open'", c.userID).Scan(&open); err != nil || open != 65 {
		t.Fatalf("custody=%d %v", open, err)
	}
}

func TestQueuedCustodyCandidateStopE2E(t *testing.T) {
	if os.Getenv("UZI_QUEUED_CUSTODY_FINALIZE_E2E") != "1" {
		t.Skip("opt-in candidate worker bridge requires provisioned agent dependencies")
	}
	env := setupCodexLiveDB(t)
	c := newClaimantExhaustion(t, env, true, 3)
	queuedCustodyCache(t, env, c.svc, 2)
	// This is an interrupted continuation, with sparse legacy custody history.
	// The real new claim opens its second hold and loses its bounded exemption.
	id := c.seedKind(t, "mr_rework")
	env.exec("UPDATE runs SET claim_generation=29,worker_id=$2,branch='agent/issue-3404',pipeline_ref='agent/issue-3404' WHERE id=$1", id, c.workerID)
	env.exec("UPDATE runs SET branch='agent/issue-3404' WHERE id=(SELECT target_run_id FROM runs WHERE id=$1)", id)
	c.seedHold(c.userID, id, 29, "open")
	caps := append(append([]string{}, c.wkr.ProtocolCapabilities...), "credential_switch_v1", "completion_interlock_v1", "recovery_archive_v2")
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", c.workerID, caps)
	p, err := c.svc.Claim(env.ctx, hbWorker(t, env, c.workerID), nil)
	if err != nil || p == nil || p.RunID != id.String() || p.ClaimGeneration != 30 {
		t.Fatalf("Claim=%+v %v", p, err)
	}
	before := observeQueuedCustody(t, env, id)
	var running, snapshots, terminals atomic.Int32
	failures := make(chan error, 16)
	fail := func(w http.ResponseWriter, err error) {
		select {
		case failures <- err:
		default:
		}
		http.Error(w, "bridge rejected request", http.StatusBadRequest)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		worker, err := env.q.GetWorkerByID(r.Context(), c.workerID)
		if err != nil {
			fail(w, err)
			return
		}
		switch r.URL.Path {
		case "/state":
			var wire struct {
				RunID string       `json:"run_id"`
				Body  StateRequest `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
				fail(w, err)
				return
			}
			if wire.RunID != id.String() || wire.Body.ClaimGeneration == nil || *wire.Body.ClaimGeneration != 30 {
				fail(w, fmt.Errorf("wrong state tuple"))
				return
			}
			if wire.Body.State != "running" {
				terminals.Add(1)
				fail(w, fmt.Errorf("unexpected status %s", wire.Body.State))
				return
			}
			row, applied, revision, err := c.svc.SetStateReport(r.Context(), worker, id, wire.Body)
			if err != nil || !applied {
				fail(w, fmt.Errorf("running ACK applied=%v err=%v", applied, err))
				return
			}
			// Test clock only: avoid waiting out the production missing-run fence.
			if _, err := env.pool.Exec(r.Context(), "UPDATE runs SET status_since=now()-interval '3 hours' WHERE id=$1", id); err != nil {
				fail(w, err)
				return
			}
			running.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"applied": applied, "status": row.Status, "claim_generation": row.ClaimGeneration, "gate_revision": revision})
		case "/snapshot":
			var snap ActiveSnapshot
			if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
				fail(w, err)
				return
			}
			if len(snap.Active) != 0 || snap.RegisterNonce != worker.SnapshotRegisterNonce.String {
				fail(w, fmt.Errorf("not a stopped current-incarnation snapshot"))
				return
			}
			if _, err := c.svc.Heartbeat(r.Context(), worker, nil, nil, &snap); err != nil {
				fail(w, err)
				return
			}
			snapshots.Add(1)
			_, _ = w.Write([]byte("{}"))
		default:
			fail(w, fmt.Errorf("unexpected bridge path"))
		}
	}))
	defer server.Close()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `
import {it} from "node:test";
import assert from "node:assert/strict";
import {pathToFileURL} from "node:url";
const root=process.env.M1_REPO_ROOT;
const h=await import(pathToFileURL(root+"/agent/test/runner-harness.ts").href);
const scene=await import(pathToFileURL(root+"/agent/test/orphan-handoff-scene.ts").href);
h.installHarness();
it("candidate pre-model retained stop drives real database heartbeat",async t=>{
 const s=await scene.scene(t,{name:"queued custody bridge",owner:{kind:"mr_rework",slug:"agent-issue-3404"},successor:"mr_rework",samePath:true,codex:false,restart:true});
 s.claim.run_id=process.env.M1_RUN_ID;
 s.claim.claim_generation=30;
 const before=scene.durable(s);
 const held=await scene.holdCycle(t,s,"mr_rework");
 const states=[];
 h.client.reportState=async(runId,body,signal)=>{
  states.push(body.status);
  const response=await fetch(process.env.M1_BRIDGE+"/state",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({run_id:runId,body}),signal});
  assert.equal(response.ok,true,"real API running ACK");
  return await response.json();
 };
 try {
  await s.runner.execute(s.claim);
  await scene.assertUnchanged(s,before,assert);
  assert.deepEqual(states,["running"]);
  assert.equal(s.settle.mock.callCount(),0,"no guarded custody settlement");
  assert.equal(s.registry.has(s.claim.run_id),false);
  assert.ok(scene.diagnostics(s).some(d=>d.reason==="completion_retirement_pending"),"production pending-source refusal");
  const snap=s.registry.build();
  snap.register_nonce=process.env.M1_REGISTER_NONCE;
  assert.deepEqual(snap.active,[]);
  const response=await fetch(process.env.M1_BRIDGE+"/snapshot",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify(snap)});
  assert.equal(response.ok,true,"real database heartbeat accepted");
 } finally {await held.release();}
});
`
	file := filepath.Join(dir, "candidate-stop.mjs")
	if err := os.WriteFile(file, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(env.ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--import", "tsx", "--test", "--test-timeout=60000", file) //nolint:gosec // G204: fixed node command runs only the fixed-content test scene just written in t.TempDir.
	cmd.Dir = filepath.Join(root, "agent")
	cmd.Env = append(os.Environ(), "M1_REPO_ROOT="+root, "M1_RUN_ID="+id.String(), "M1_BRIDGE="+server.URL, "M1_REGISTER_NONCE="+hbWorker(t, env, c.workerID).SnapshotRegisterNonce.String)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("candidate retained-stop bridge: %v, %.3000s", err, output)
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if running.Load() != 1 || snapshots.Load() != 1 || terminals.Load() != 0 {
		t.Fatalf("bridge running=%d snapshots=%d terminals=%d", running.Load(), snapshots.Load(), terminals.Load())
	}
	after := observeQueuedCustody(t, env, id)
	if statusOf(t, env, id) != "queued" || string(after.run["requeue_count"]) != "1" || before.holds != after.holds {
		t.Fatal("candidate stop did not retain blocked continuation and both holds")
	}
	for cycle := 0; cycle < 2; cycle++ {
		for _, path := range []string{"heartbeat", "register", "sweep", "heartbeat"} {
			queuedCustodyTrigger(t, c, path)
		}
		requireQueuedCustodyPreserved(t, after, observeQueuedCustody(t, env, id), false)
	}
}

func TestQueuedCustodyNonPublishingProfilesLiveDB(t *testing.T) {
	for _, kind := range []string{"job", "judge", "chat", "cross_check", "isolated"} {
		t.Run(kind, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			c := newClaimantExhaustion(t, env, true, 1)
			queuedCustodyCache(t, env, c.svc, 1)
			id := uuid.New()
			switch kind {
			case "job":
				env.exec("INSERT INTO runs(id,user_id,kind,job_type,status,worker_id,claim_generation,requeue_count,issue_title,issue_description) VALUES($1,$2,'job','research','queued',$3,1,1,'fixture','fixture')", id, c.userID, c.workerID)
			case "chat":
				env.exec("INSERT INTO runs(id,user_id,kind,status,worker_id,claim_generation,requeue_count,issue_title,issue_description) VALUES($1,$2,'chat','queued',$3,1,1,'fixture','fixture')", id, c.userID, c.workerID)
			case "judge":
				target := c.seedRun("completed", 1)
				env.exec("INSERT INTO runs(id,user_id,kind,target_run_id,status,worker_id,claim_generation,requeue_count,issue_title,issue_description) VALUES($1,$2,'judge',$3,'queued',$4,1,1,'fixture','fixture')", id, c.userID, target, c.workerID)
			case "cross_check":
				lead := c.seedRun("running", 1)
				id = seedRecoveryCheck(t, env, c.userID, c.repoID, c.workerID, lead)
				env.exec("UPDATE runs SET status='queued',requeue_count=1 WHERE id=$1", id)
			case "isolated":
				profile := uuid.New()
				env.exec("INSERT INTO egress_profiles(id,name,hosts) VALUES($1,$2,ARRAY['docs.example.test'])", profile, "q-"+profile.String())
				env.exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,status,egress_profile_id,worker_id,claim_generation,requeue_count,issue_title,issue_description) VALUES($1,$2,$3,'issue',12345,'queued',$4,$5,1,1,'fixture','fixture')", id, c.userID, c.repoID, profile, c.workerID)
			}
			// Adversarial/legacy custody cannot turn a non-publishing profile into a
			// publishing continuation. The unexpected evidence must remain untouched.
			c.seedHold(c.userID, id, 1, "open")
			before := observeQueuedCustody(t, env, id)
			if n := runQueuedCustodyFrozen(t, c); n != 0 {
				t.Fatal("non-publishing profile acquired exhaustion hold")
			}
			requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, env, id), false)
		})
	}
}

func TestQueuedCustodyMembershipRaceLiveDB(t *testing.T) {
	for _, membershipFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(membershipFirst), func(t *testing.T) {
			c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
			other, _, _ := c.env.seedCodexInfra(t)
			tx, q, w, locks, limit := freezeQueuedCustody(t, c)
			var hold uuid.UUID
			if err := c.env.pool.QueryRow(c.env.ctx, "SELECT id FROM recovery_custody_holds WHERE run_id=$1", id).Scan(&hold); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, err := c.env.pool.Exec(context.Background(), "DELETE FROM recovery_custody_holds WHERE id=$1", hold)
				if err != nil {
					t.Error(err)
				}
			})
			ctx, cancel := context.WithTimeout(c.env.ctx, 10*time.Second)
			defer cancel()
			// Deliberately adversarial persisted membership drift, not an owner API
			// permission: test only our own fixture row, never settle or remove it.
			owner, err := c.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = owner.Rollback(context.Background()) }()
			if membershipFirst {
				if _, err := owner.Exec(ctx, "UPDATE recovery_custody_holds SET user_id=$2 WHERE id=$1", hold, other); err != nil {
					t.Fatal(err)
				}
				if err := owner.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				before := observeQueuedCustody(t, c.env, id)
				rows, err := c.svc.parkQueuedCustody(ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
				if err != nil || len(rows) != 0 {
					t.Fatalf("membership lost race=%+v %v", rows, err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
			} else {
				rows, err := c.svc.parkQueuedCustody(ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
				if err != nil || len(rows) != 1 {
					t.Fatalf("park=%+v %v", rows, err)
				}
				pid := owner.Conn().PgConn().PID()
				done := make(chan error, 1)
				joined := false
				defer func() {
					cancel()
					_ = tx.Rollback(context.Background())
					if !joined {
						if err := <-done; err != nil {
							t.Logf("race cleanup: %v", err)
						}
					}
				}()
				go func() {
					_, err := owner.Exec(ctx, "UPDATE recovery_custody_holds SET user_id=$2 WHERE id=$1", hold, other)
					if err == nil {
						err = owner.Commit(ctx)
					}
					done <- err
				}()
				waitQueuedOwnBackendLock(t, c.env, ctx, pid)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				resultErr := <-done
				joined = true
				if resultErr != nil {
					t.Fatal(resultErr)
				}
				if statusOf(t, c.env, id) != "recovery_wait" {
					t.Fatal("later membership drift rewrote disposition")
				}
			}
			var state string
			var ownerID uuid.UUID
			if err := c.env.pool.QueryRow(ctx, "SELECT state,user_id FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state, &ownerID); err != nil || state != "open" || ownerID != other {
				t.Fatalf("retained membership=%s %s %v", state, ownerID, err)
			}
			var counted int64
			if err := c.env.pool.QueryRow(ctx, "SELECT fn_custody_admission_count($1,now()-interval '1 minute')", other).Scan(&counted); err != nil || counted != 1 {
				t.Fatalf("custody disappeared from accounting: %d %v", counted, err)
			}
			// Cleanup belongs to this test; fixture cleanup normally uses the first owner.

		})
	}
}

// Recovery must recount after its hold-lock statement waits on an owner discard.
// The returned ID remains in the lock result even though the winning UPDATE
// changed the row to discarded; that ID alone is not fresh capacity evidence.
func TestQueuedCustodyRecoveryWaitsForDiscardLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 1, 1, 1, []int64{1}, 1)
	tx, q, w, locks, limit := freezeQueuedCustody(t, c)
	ctx, cancel := context.WithTimeout(c.env.ctx, 10*time.Second)
	defer cancel()
	owner, err := c.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Rollback(context.Background()) }()
	var hold uuid.UUID
	if err := owner.QueryRow(ctx, "SELECT id FROM recovery_custody_holds WHERE run_id=$1", id).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	n, err := store.New(owner).DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{
		HoldID: hold, RunID: id, UserID: c.userID, ReleaseEvidence: pgconv.TextOrNull("owner_discard"),
	})
	if err != nil || n != 1 {
		t.Fatalf("owner discard=%d %v", n, err)
	}
	// Keep the owner UPDATE uncommitted until the recovery backend is waiting.
	type result struct {
		rows []store.WorkerRecoveryDisposition
		err  error
	}
	done := make(chan result, 1)
	joined := false
	defer func() {
		cancel()
		_ = owner.Rollback(context.Background())
		if !joined {
			if result := <-done; result.err != nil {
				t.Logf("race cleanup: %v", result.err)
			}
		}
	}()
	pid := tx.Conn().PgConn().PID()
	go func() {
		rows, err := c.svc.parkQueuedCustody(ctx, q, w.ID, w.SnapshotRegisterNonce, limit, locks, nil)
		done <- result{rows, err}
	}()
	waitQueuedOwnBackendLock(t, c.env, ctx, pid)
	if err := owner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	before := observeQueuedCustody(t, c.env, id)
	r := <-done
	joined = true
	if r.err != nil || len(r.rows) != 0 {
		t.Fatalf("discard winning hold wait=%+v %v", r.rows, r.err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
	if statusOf(t, c.env, id) != "queued" {
		t.Fatal("winning discard must leave queued admission available")
	}
	var state string
	if err := c.env.pool.QueryRow(ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state); err != nil || state != "discarded" {
		t.Fatalf("owner disposition=%s %v", state, err)
	}
}

// A requeue followed by exhaustion parking in the same transaction has only one
// committed status to announce. Consumers must never receive a trailing queued.
func TestQueuedCustodyFinalStatusPublicationLiveDB(t *testing.T) {
	for _, path := range []string{"register", "heartbeat"} {
		t.Run(path, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			c := newClaimantExhaustion(t, env, true, 1)
			queuedCustodyCache(t, env, c.svc, 1)
			id := c.seedKind(t, "issue")
			c.claimAndRun(t, c.wkr, id, 1, true)
			spy := &stateSpy{}
			c.svc.SetBroadcaster(spy)
			queuedCustodyTrigger(t, c, path)
			if got := statusOf(t, env, id); got != "recovery_wait" {
				t.Fatalf("committed status=%s", got)
			}
			var states []string
			for _, e := range spy.events {
				if e.id == id {
					states = append(states, e.status)
				}
			}
			if !reflect.DeepEqual(states, []string{"recovery_wait"}) {
				t.Fatalf("committed state events=%v", states)
			}
			c.requireAllHoldsOpen(t, id, 1)
		})
	}
}
