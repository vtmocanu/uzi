package workersvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only Commit is intercepted: production SQL, lock acquisition and isolation
// selection still run on real PostgreSQL transactions.
type cordonRaceBeginner struct {
	pool    *pgxpool.Pool
	pid     chan uint32
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *cordonRaceBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.BeginTx(ctx, pgx.TxOptions{})
}

func (b *cordonRaceBeginner) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	b.pid <- tx.Conn().PgConn().PID()
	pause := false
	b.once.Do(func() { pause = b.ready != nil })
	if pause {
		return &cordonRaceTx{Tx: tx, ready: b.ready, release: b.release}, nil
	}
	return tx, nil
}

type cordonRaceTx struct {
	pgx.Tx
	ready   chan struct{}
	release chan struct{}
}

func (tx *cordonRaceTx) Commit(ctx context.Context) error {
	close(tx.ready)
	select {
	case <-tx.release:
		return tx.Tx.Commit(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cordonRaceReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatalf("production race barrier: %v", ctx.Err())
		var zero T
		return zero
	}
}

func cordonRaceBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiter, holder uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, "SELECT $1::int = ANY(pg_blocking_pids($2::int))", holder, waiter).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			t.Logf("observed PostgreSQL lock barrier: waiter=%d holder=%d", waiter, holder)
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("no observed production lock barrier: %v", ctx.Err())
		}
	}
}

// Covers the legacy bodyless claim, snapshot claim, dedicated checker and chat.
// The API worker argument is always captured before either transaction begins.
func TestManualCordonProductionRaceLiveDB(t *testing.T) {
	for _, lane := range []string{"ordinary-bodyless", "ordinary-snapshot", "checker", "chat"} {
		for _, cordonFirst := range []bool{true, false} {
			order := "claim-first"
			if cordonFirst {
				order = "cordon-first"
			}
			t.Run(lane+"/"+order, func(t *testing.T) {
				var env codexTestEnv
				var svc *Service
				var owner, worker, run uuid.UUID
				if lane == "checker" {
					f := newCrossCheckContentionFixture(t)
					env, svc, owner, worker, run = f.env, f.svc, f.userID, f.workerID, f.runID
					env.exec("UPDATE workers SET max_cross_check_slots=1,protocol_capabilities=array_append(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1", worker)
				} else {
					f := newIsoFix(t)
					env, svc, owner, worker = f.env, f.svc, f.userID, f.workerID
					if lane == "chat" {
						run = uuid.New()
						env.exec("INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status) VALUES($1,$2,'chat','race chat','hello','queued')", run, owner)
					} else {
						run = f.queuedRun(t, 3310, false)
					}
				}
				env.exec("UPDATE workers SET last_heartbeat_at=now(),snapshot_register_nonce='cordon-race',snapshot_epoch=0 WHERE id=$1", worker)
				stale := wkrRow(t, env, worker)
				if stale.ManualCordonedAt.Valid {
					t.Fatal("fixture worker was already cordoned")
				}
				ctx, cancel := context.WithTimeout(env.ctx, 20*time.Second)
				var wg sync.WaitGroup
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer func() {
					unblock()
					cancel()
					wg.Wait()
				}()
				first := &cordonRaceBeginner{pool: env.pool, pid: make(chan uint32, 16), ready: make(chan struct{}), release: release}
				second := &cordonRaceBeginner{pool: env.pool, pid: make(chan uint32, 16)}
				claimDB, cordonDB := first, second
				if cordonFirst {
					claimDB, cordonDB = second, first
				}
				svc.SetTxBeginner(claimDB)
				type claimResult struct {
					id         string
					generation int64
					err        error
				}
				claimed := make(chan claimResult, 1)
				cordoned := make(chan error, 1)
				startClaim := func() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if lane == "chat" {
							p, err := svc.ClaimChat(ctx, stale)
							r := claimResult{err: err}
							if p != nil {
								r.id, r.generation = p.RunID, p.ClaimGeneration
							}
							claimed <- r
							return
						}
						var snap *ActiveSnapshot
						if lane == "ordinary-snapshot" {
							snap = &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "cordon-race", Active: []ActiveRunEntry{}}
						}
						var p *ClaimPayload
						var err error
						if lane == "checker" {
							p, err = svc.ClaimCrossCheck(ctx, stale, snap)
						} else {
							p, err = svc.Claim(ctx, stale, snap)
						}
						r := claimResult{err: err}
						if p != nil {
							r.id, r.generation = p.RunID, p.ClaimGeneration
						}
						claimed <- r
					}()
				}
				startCordon := func() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := store.ChangeWorkerManualCordon(ctx, cordonDB, owner, worker, "race operator", pgtype.Text{}, true)
						cordoned <- err
					}()
				}
				if cordonFirst {
					startCordon()
				} else {
					startClaim()
				}
				holder := cordonRaceReceive(t, ctx, first.pid)
				cordonRaceReceive(t, ctx, first.ready)
				if cordonFirst {
					startClaim()
				} else {
					startCordon()
				}
				waiter := cordonRaceReceive(t, ctx, second.pid)
				cordonRaceBlocked(t, ctx, env.pool, waiter, holder)
				unblock()
				if err := cordonRaceReceive(t, ctx, cordoned); err != nil {
					t.Fatalf("production cordon: %v", err)
				}
				result := cordonRaceReceive(t, ctx, claimed)
				if result.err != nil {
					t.Fatalf("production claim: %v", result.err)
				}
				if !wkrRow(t, env, worker).ManualCordonedAt.Valid {
					t.Fatal("cordon did not persist")
				}
				persisted := mustRun(t, env, run)
				if cordonFirst {
					if result.id != "" || persisted.Status != "queued" || persisted.WorkerID.Valid || persisted.ClaimGeneration != 0 {
						t.Fatalf("committed cordon admitted unrelated intake: payload=%q status=%s worker=%v generation=%d", result.id, persisted.Status, persisted.WorkerID, persisted.ClaimGeneration)
					}
				} else if result.id != run.String() || result.generation != 1 || persisted.Status != "claimed" ||
					!persisted.WorkerID.Valid || uuid.UUID(persisted.WorkerID.Bytes) != worker || persisted.ClaimGeneration != 1 {
					t.Fatalf("cordon lost preceding admission: payload=%q generation=%d status=%s worker=%v persisted-generation=%d", result.id, result.generation, persisted.Status, persisted.WorkerID, persisted.ClaimGeneration)
				}
				if lane == "ordinary-snapshot" && workerEpoch(t, env, worker) != 1 {
					t.Fatal("valid active snapshot was not committed")
				}
			})
		}
	}
}
