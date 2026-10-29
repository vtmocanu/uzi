package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type forgeOriginHookBeginner struct {
	inner interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	attempts     int
	pid          uint32
	beforeBegin  func(int)
	beforeCommit func()
}

func (b *forgeOriginHookBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	b.attempts++
	if b.beforeBegin != nil {
		b.beforeBegin(b.attempts)
	}
	tx, err := b.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	b.pid = tx.Conn().PgConn().PID()
	return forgeOriginHookTx{Tx: tx, beforeCommit: b.beforeCommit}, nil
}

type forgeOriginHookTx struct {
	pgx.Tx
	beforeCommit func()
}

func (tx forgeOriginHookTx) Commit(ctx context.Context) error {
	if tx.beforeCommit != nil {
		tx.beforeCommit()
	}
	return tx.Tx.Commit(ctx)
}

// Probe the actual service immediately before commit. A mapping writer must not
// pass the observation transaction, and parent deletion must not invert its locks.
func TestForgeOriginMappingPruneSerializationLiveDB(t *testing.T) {
	for _, change := range []string{"origin", "repo_delete", "connection_delete", "worker_delete", "owner_delete"} {
		t.Run(change, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			mhOpenHold(t, e, id, 1, w)
			var conn uuid.UUID
			if err := e.pool.QueryRow(e.ctx, "SELECT connection_id FROM repos WHERE id=$1", e.repoID).Scan(&conn); err != nil {
				t.Fatal(err)
			}
			var probe string
			var arg uuid.UUID
			switch change {
			case "origin", "repo_delete":
				probe, arg = "SELECT id FROM repos WHERE id=$1 FOR NO KEY UPDATE NOWAIT", e.repoID
			case "connection_delete":
				probe, arg = "SELECT id FROM forge_connections WHERE id=$1 FOR UPDATE NOWAIT", conn
			case "worker_delete":
				probe, arg = "SELECT id FROM workers WHERE id=$1 FOR UPDATE NOWAIT", w
			case "owner_delete":
				probe, arg = "SELECT id FROM users WHERE id=$1 FOR UPDATE NOWAIT", e.userID
			}
			hooks := &forgeOriginHookBeginner{inner: e.pool}
			hooks.beforeCommit = func() {
				b, err := e.pool.Begin(e.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = b.Rollback(e.ctx) }()
				if _, err := b.Exec(e.ctx, probe, arg); !isLockNotAvailable(err) {
					t.Fatalf("mapping/lifecycle writer escaped observation lock: %v", err)
				}
			}
			svc := e.forgeParkService(t, 6)
			svc.SetTxBeginner(hooks)
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{
				State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1),
			})
			if err != nil || !applied {
				t.Fatalf("park=%v, %v", applied, err)
			}
			if e.originCount(t, w) != 1 {
				t.Fatal("committed observation missing")
			}
			switch change {
			case "origin":
				e.exec(t, "UPDATE repos SET web_url='https://new.example/group/repo' WHERE id=$1", e.repoID)
			case "repo_delete":
				e.exec(t, "DELETE FROM repos WHERE id=$1", e.repoID)
			case "connection_delete":
				e.exec(t, "DELETE FROM forge_connections WHERE id=$1", conn)
			case "worker_delete":
				e.exec(t, "DELETE FROM workers WHERE id=$1", w)
			case "owner_delete":
				e.exec(t, "DELETE FROM users WHERE id=$1", e.userID)
			}
			if e.originCount(t, w) != 0 {
				t.Fatal("lifecycle retained obsolete origin")
			}
		})
	}
}

// B starts its actual UPDATE while A is still uncommitted. Observe its
// database lock dependency, then let A commit; B's same-statement pruning
// must see and remove A's formerly uncommitted cooldown.
func TestForgeOriginWaitingMappingPruneLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	mhOpenHold(t, e, id, 1, w)
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	b, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Rollback(ctx) }()
	bpid := b.Conn().PgConn().PID()
	done := make(chan error, 1)
	hooks := &forgeOriginHookBeginner{inner: e.pool}
	hooks.beforeCommit = func() {
		go func() {
			_, err := b.Exec(ctx, "UPDATE repos SET web_url='https://new.example/group/repo' WHERE id=$1", e.repoID)
			if err == nil {
				err = b.Commit(ctx)
			}
			done <- err
		}()
		for {
			var blocked bool
			if err := e.pool.QueryRow(ctx, "SELECT $1::integer = ANY(pg_blocking_pids($2::integer))", int(hooks.pid), int(bpid)).Scan(&blocked); err != nil {
				t.Fatalf("waiting for owned mapping lock: %v", err)
			}
			if blocked {
				return
			}
			select {
			case err := <-done:
				t.Fatalf("mapping edit completed before observation commit: %v", err)
			default:
			}
		}
	}
	svc := e.forgeParkService(t, 6)
	svc.SetTxBeginner(hooks)
	_, applied, err := svc.SetState(ctx, store.Worker{ID: w}, id, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1),
	})
	if err != nil || !applied {
		t.Fatalf("park=%v,%v", applied, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if e.originCount(t, w) != 0 {
		t.Fatal("waiting mapping update retained an orphan cooldown")
	}
}

func TestForgeOriginMappingContentionRetryLiveDB(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(fmt.Sprint(release), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, nil)
			id := e.fpSeedRunning(t, w, 1)
			h := mhOpenHold(t, e, id, 1, w)
			b, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = b.Rollback(e.ctx) }()
			if _, err := b.Exec(e.ctx, "UPDATE repos SET path_with_namespace='group/renamed' WHERE id=$1", e.repoID); err != nil {
				t.Fatal(err)
			}
			hooks := &forgeOriginHookBeginner{inner: e.pool}
			hooks.beforeBegin = func(attempt int) {
				if release && attempt == 2 {
					if err := b.Rollback(e.ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			svc := e.forgeParkService(t, 6)
			svc.SetTxBeginner(hooks)
			_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
			if release {
				if err != nil || !applied || hooks.attempts != 2 {
					t.Fatalf("retried park=%v,%v attempts=%d", applied, err, hooks.attempts)
				}
				c := e.originCooldown(t, w, "https://forge.e2e")
				if c.ObservationVersion != 1 {
					t.Fatal("database retries counted as failure groups")
				}
				_, _, count, _, waits := e.fpRun(t, id)
				if count != 1 || waits != 1 {
					t.Fatalf("retry spent park allowance: %d/%d", count, waits)
				}
			} else {
				if !isLockNotAvailable(err) || applied || hooks.attempts != 3 {
					t.Fatalf("bounded contention=%v,%v attempts=%d", applied, err, hooks.attempts)
				}
				status, _, count, _, waits := e.fpRun(t, id)
				state, _ := e.fpHoldEvidence(t, h)
				if status != "running" || count != 0 || waits != 0 || state != "open" || e.originCount(t, w) != 0 {
					t.Fatal("rejected lock contention changed run/custody/counters")
				}
			}
		})
	}
}

func TestForgeOriginObservationErrorRollsBackServiceLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 1)
	h := mhOpenHold(t, e, id, 1, w)
	fn := "forge_origin_failure_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	e.exec(t, fmt.Sprintf("CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected observation failure'; END $$", fn))
	e.exec(t, fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT ON forge_origin_cooldowns FOR EACH ROW EXECUTE FUNCTION %s()", fn, fn))
	t.Cleanup(func() {
		e.exec(t, fmt.Sprintf("DROP TRIGGER %s ON forge_origin_cooldowns", fn))
		e.exec(t, fmt.Sprintf("DROP FUNCTION %s()", fn))
	})
	svc := e.forgeParkService(t, 6)
	_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: "recovery_wait", RecoveryCause: strPtr("forge_unreachable"), ClaimGeneration: i64Ptr(1)})
	if err == nil || applied || errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("injected observation=%v,%v", applied, err)
	}
	status, _, count, _, waits := e.fpRun(t, id)
	state, _ := e.fpHoldEvidence(t, h)
	if status != "running" || count != 0 || waits != 0 || state != "open" || e.originCount(t, w) != 0 {
		t.Fatal("observation failure leaked park/custody/counter mutation")
	}
}
