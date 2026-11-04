package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Inject only at existing transaction/query seams; PostgreSQL performs the
// statement failure, savepoint rollback, locks and outer commit.
type queuedSettingsFault struct {
	pool                                *pgxpool.Pool
	mode                                string
	hits, guards, savepoints, rollbacks int
	isolation                           string
	lockRetained                        bool
	managementErr                       error
	cancel                              context.CancelFunc
}

func (f *queuedSettingsFault) Begin(ctx context.Context) (pgx.Tx, error) {
	// A stricter connection default must not leak into heartbeat admission.
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	return &queuedCustodySettingsReadFaultTx{Tx: tx, fault: f}, nil
}

type queuedCustodySettingsReadFaultTx struct {
	pgx.Tx
	fault  *queuedSettingsFault
	nested bool
}

func (tx *queuedCustodySettingsReadFaultTx) Begin(ctx context.Context) (pgx.Tx, error) {
	f := tx.fault
	if f.guards != 1 {
		return nil, errors.New("settings savepoint preceded settings lock")
	}
	f.savepoints++
	if f.mode == "begin" {
		return nil, f.managementErr
	}
	sp, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &queuedCustodySettingsReadFaultTx{Tx: sp, fault: f, nested: true}, nil
}

func (tx *queuedCustodySettingsReadFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if sql == "SELECT pg_advisory_xact_lock_shared($1)" && len(args) == 1 && args[0] == store.SettingsMutationLockKey {
		tx.fault.guards++
		if tx.fault.mode == "lock" {
			return pgconn.CommandTag{}, tx.fault.managementErr
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *queuedCustodySettingsReadFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.HasPrefix(sql, "-- name: ListAppSettings ") {
		return tx.Tx.Query(ctx, sql, args...)
	}
	f := tx.fault
	f.hits++
	if err := tx.Tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&f.isolation); err != nil {
		return nil, err
	}
	switch f.mode {
	case "release":
		return tx.Tx.Query(ctx, sql, args...)
	case "context":
		f.cancel()
		return nil, ctx.Err()
	case "connection":
		_ = tx.Conn().Close(ctx)
		return nil, errors.New("settings connection closed")
	case "timeout":
		if _, err := tx.Tx.Exec(ctx, "SET LOCAL statement_timeout = '5ms'"); err != nil {
			return nil, err
		}
		return tx.Tx.Query(ctx, "SELECT pg_sleep(0.1)")
	default:
		return tx.Tx.Query(ctx, "SELECT 1 / 0")
	}
}

func (tx *queuedCustodySettingsReadFaultTx) Rollback(ctx context.Context) error {
	if tx.nested {
		tx.fault.rollbacks++
	}
	err := tx.Tx.Rollback(ctx)
	if tx.nested && err == nil && (tx.fault.mode == "statement" || tx.fault.mode == "timeout") {
		var acquired bool
		if probeErr := tx.fault.pool.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", store.SettingsMutationLockKey).Scan(&acquired); probeErr != nil {
			return probeErr
		}
		tx.fault.lockRetained = !acquired
	}
	if tx.nested && tx.fault.mode == "rollback" {
		return tx.fault.managementErr
	}
	return err
}

func (tx *queuedCustodySettingsReadFaultTx) Commit(ctx context.Context) error {
	if tx.nested && tx.fault.mode == "release" {
		return tx.fault.managementErr
	}
	return tx.Tx.Commit(ctx)
}

func TestQueuedCustodySettingsFailureLivenessLiveDB(t *testing.T) {
	for _, path := range []string{"heartbeat", "register"} {
		for _, failure := range []string{"statement", "timeout"} {
			t.Run(path+"/"+failure, func(t *testing.T) {
				c, id := seededQueuedCustody(t, 3, 3, 1, []int64{1}, 1)
				before := observeQueuedCustody(t, c.env, id)
				c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 minute' WHERE id=$1", c.workerID)
				f := &queuedSettingsFault{pool: c.env.pool, mode: failure}
				c.svc.SetTxBeginner(f)
				queuedCustodyTrigger(t, c, path)
				// Snapshot and exhaustion processing may use additional savepoints.
				// Require recovery from the settings failure, not a global count.
				if f.hits != 1 || f.guards != 1 || f.savepoints < 1 || f.rollbacks < 1 || f.isolation != "read committed" || !f.lockRetained {
					t.Fatalf("settings failure not isolated: %+v", f)
				}
				assertHeartbeatFresh(t, c.env, c.workerID)
				requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
				if path == "heartbeat" && workerEpoch(t, c.env, c.workerID) != 1 {
					t.Fatal("settings failure prevented snapshot acceptance")
				}
				if path == "register" && hbWorker(t, c.env, c.workerID).SnapshotRegisterNonce.String == exhaustionNonce {
					t.Fatal("settings failure prevented registration nonce rotation")
				}
				// The rollback probe observed the outer settings guard. After
				// the outer commit a different transaction can acquire it.
				tx, err := c.env.pool.Begin(c.env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				var locked bool
				if err := tx.QueryRow(c.env.ctx, "SELECT pg_try_advisory_xact_lock($1)", store.SettingsMutationLockKey).Scan(&locked); err != nil || !locked {
					t.Fatalf("outer settings lock leaked: locked=%v err=%v", locked, err)
				}
			})
		}
	}
}

func TestQueuedCustodySettingsManagementFailureLiveDB(t *testing.T) {
	for _, mode := range []string{"lock", "begin", "rollback", "release", "context", "connection"} {
		t.Run(mode, func(t *testing.T) {
			c, id := seededQueuedCustody(t, 3, 3, 1, []int64{1}, 1)
			before := observeQueuedCustody(t, c.env, id)
			c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 minute' WHERE id=$1", c.workerID)
			w := hbWorker(t, c.env, c.workerID)
			ctx, cancel := context.WithTimeout(c.env.ctx, 5*time.Second)
			defer cancel()
			f := &queuedSettingsFault{pool: c.env.pool, mode: mode, managementErr: errors.New("settings transaction management failed"), cancel: cancel}
			c.svc.SetTxBeginner(f)
			_, err := c.svc.Heartbeat(ctx, w, nil, nil, &ActiveSnapshot{
				RegisterNonce: w.SnapshotRegisterNonce.String, SnapshotEpoch: 1, Active: []ActiveRunEntry{},
			})
			if err == nil {
				t.Fatal("unusable settings transaction reported successful heartbeat")
			}
			if mode != "context" && mode != "connection" && !errors.Is(err, f.managementErr) {
				t.Fatalf("management error lost: %v", err)
			}
			after := hbWorker(t, c.env, c.workerID)
			if after.LastHeartbeatAt != w.LastHeartbeatAt || after.SnapshotEpoch != w.SnapshotEpoch {
				t.Fatal("failed heartbeat changed committed liveness/snapshot")
			}
			requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
		})
	}
}

type queuedSettingsCacheSpy struct {
	*settings.Cache
	reads int
}

func (s *queuedSettingsCacheSpy) CustodyHoldLimit(ctx context.Context) (int32, error) {
	s.reads++
	return s.Cache.CustodyHoldLimit(ctx)
}

func TestQueuedCustodySettingsRefreshOnlyLiveDB(t *testing.T) {
	c, id := seededQueuedCustody(t, 3, 3, 1, []int64{1}, 1)
	before := observeQueuedCustody(t, c.env, id)
	cache := &queuedSettingsCacheSpy{Cache: settings.New(c.env.q, time.Hour)}
	c.svc.p.CustodyHoldLimit = cache
	f := &queuedSettingsFault{pool: c.env.pool, mode: "statement"}
	c.svc.SetTxBeginner(f)
	c.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '1 minute' WHERE id=$1", c.workerID)
	w := hbWorker(t, c.env, c.workerID)
	got, err := c.svc.Heartbeat(c.env.ctx, w, nil, nil, nil, &ProtocolRefresh{
		RegisterNonce: w.SnapshotRegisterNonce.String, MaxConcurrentRuns: 8, MaxCrossCheckSlots: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cache.reads != 0 || f.hits != 0 || f.guards != 0 || f.savepoints != 0 {
		t.Fatalf("refresh-only consulted custody settings: cache=%d fault=%+v", cache.reads, f)
	}
	if got.MaxConcurrentRuns.Int32 != 8 || got.MaxCrossCheckSlots.Int32 != 4 || got.SnapshotEpoch != w.SnapshotEpoch {
		t.Fatal("refresh-only failed to update protocol limits or changed snapshot epoch")
	}
	assertHeartbeatFresh(t, c.env, c.workerID)
	requireQueuedCustodyPreserved(t, before, observeQueuedCustody(t, c.env, id), false)
}
