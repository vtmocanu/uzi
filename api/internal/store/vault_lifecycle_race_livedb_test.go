package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Every hook wraps the actual transaction's queries, including its state reader.
type vlRaceQueries struct {
	*store.Queries
	beforeSecret func(context.Context) error
	afterSecrets func(context.Context) error
}

func (q *vlRaceQueries) LockSecretMutation(ctx context.Context, id uuid.UUID) error {
	if q.beforeSecret != nil {
		if err := q.beforeSecret(ctx); err != nil {
			return err
		}
	}
	return q.Queries.LockSecretMutation(ctx, id)
}
func (q *vlRaceQueries) LockVaultSecretMaterialBatch(ctx context.Context, p store.LockVaultSecretMaterialBatchParams) ([]store.LockVaultSecretMaterialBatchRow, error) {
	rows, err := q.Queries.LockVaultSecretMaterialBatch(ctx, p)
	if err == nil && len(rows) > 0 && q.afterSecrets != nil {
		err = q.afterSecrets(ctx)
	}
	return rows, err
}
func vlRaceRollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func vlRaceTransact(pool *pgxpool.Pool, wrap func(pgx.Tx, int) vault.Store) vault.Transact {
	var calls atomic.Int32
	return func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer vlRaceRollback(tx)
		q := wrap(tx, int(calls.Add(1)))
		if err := fn(vault.WithStateReader(ctx, q), q); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return &vault.CommitError{Err: err}
		}
		return nil
	}
}
func vlRaceJoin(t *testing.T, done <-chan error) {
	t.Helper()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, vault.ErrUncertainOutcome) {
		t.Errorf("join owned lifecycle operation during cleanup: %v", err)
	}
}

func vlRaceWait(ctx context.Context, release <-chan struct{}) error {
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Positive database evidence of contention, bounded by ctx. A failed observation
// never authorizes releasing the owner barrier. Each independent case owns its
// goroutines and cleanup, so a failure does not leave work in a sibling case.
func vlRaceBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiter, owner uint32) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, "SELECT $2::int = ANY(pg_blocking_pids($1::int))", waiter, owner).Scan(&blocked); err != nil {
			t.Fatal("observe owned transaction blocking failed")
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("expected transaction never blocked on original owner")
		}
	}
}

func TestVaultLostCommitACKWaitsForOriginalResolutionLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) {
			f := vlSeed(t, pool, true, true)
			before, metadata := f.snapshot(t, false), f.snapshot(t, true)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resolve := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resolve) }) }
			originalDone := make(chan error, 1)
			freshAttempt := make(chan uint32, 1)
			result := make(chan error, 1)
			var originalPID uint32
			var calls, deletes atomic.Int32
			var prefix []string
			originalStarted, originalJoined, callerJoined := false, false, false
			// This adapter deliberately violates only ACK timing: the real first
			// transaction is still pending when CommitError reaches the core.
			transact := func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
				n := calls.Add(1)
				tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					return err
				}
				if n == 1 {
					originalPID = tx.Conn().PgConn().PID()
					q := &vlACKQueries{Queries: store.New(tx), deletes: &deletes}
					if err := fn(vault.WithStateReader(ctx, q), q); err != nil {
						vlRaceRollback(tx)
						return err
					}
					originalStarted = true
					go func() {
						err := vlRaceWait(ctx, resolve)
						if err == nil {
							if commit {
								err = tx.Commit(ctx)
							} else {
								err = tx.Rollback(ctx)
							}
						}
						vlRaceRollback(tx)
						originalDone <- err
					}()
					return &vault.CommitError{Err: errors.New("simulated missing ACK with transaction pending")}
				}
				defer vlRaceRollback(tx)
				q := &vlACKQueries{Queries: store.New(tx), deletes: &deletes,
					prefix: func(step string) {
						prefix = append(prefix, step)
						if step == "secret" {
							freshAttempt <- tx.Conn().PgConn().PID()
						}
					},
				}
				if err := fn(vault.WithStateReader(ctx, q), q); err != nil {
					return err
				}
				return tx.Commit(ctx)
			}
			v := vault.New(f.master, store.New(pool), transact)
			defer func() {
				release()
				cancel()
				if !callerJoined {
					vlRaceJoin(t, result)
				}
				if originalStarted && !originalJoined {
					vlRaceJoin(t, originalDone)
				}
			}()
			go func() { result <- v.Unlock(ctx, f.owner, vlLoginPhrase) }()
			var freshPID uint32
			select {
			case freshPID = <-freshAttempt:
			case err := <-result:
				callerJoined = true
				t.Fatalf("uncertain lifecycle returned before fresh prefix attempt: %v", err)
			case <-ctx.Done():
				t.Fatal("fresh reconciliation never attempted owner prefix")
			}
			vlRaceBlocked(t, ctx, pool, freshPID, originalPID)
			select {
			case err := <-result:
				callerJoined = true
				t.Fatalf("uncertain lifecycle returned while original transaction pending: %v", err)
			default:
			}
			if v.Unlocked(f.owner) {
				t.Fatal("private key published during uncertain original outcome")
			}
			// MVCC still exposes the unchanged pre-transaction durable state.
			vlEqual(t, before, f.snapshot(t, false), "pending original durable state")
			release()
			err := <-originalDone
			originalJoined = true
			if err != nil {
				t.Fatal("resolve original transaction failed")
			}
			err = <-result
			callerJoined = true
			var uncertain *vault.UncertainOutcomeError
			if !errors.As(err, &uncertain) || !errors.Is(err, vault.ErrUncertainOutcome) || uncertain.Reconciliation != nil || calls.Load() != 2 {
				t.Fatalf("original resolution did not reconcile as uncertain: %v", err)
			}
			if fmt.Sprint(prefix) != "[settings secret users wrapper]" {
				t.Fatalf("fresh reconciliation prefix = %v", prefix)
			}
			if v.Unlocked(f.owner) || deletes.Load() != 0 {
				t.Fatal("uncertainty published key or compensated by deleting wrapper")
			}
			cold := vault.NewPostgres(f.master, pool)
			if commit {
				f.state(t, cold, store.VaultModeProtected, 1, false, true)
				if err := cold.UnlockExisting(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				f.readable(t, cold, store.SealedWithDEK)
				vlEqual(t, metadata, f.snapshot(t, true), "committed uncertain metadata")
			} else {
				vlEqual(t, before, f.snapshot(t, false), "rolled-back uncertain state")
				f.state(t, cold, store.VaultModeProtected, 0, false, false)
			}
		})
	}
}

type vlACKQueries struct {
	*store.Queries
	deletes *atomic.Int32
	prefix  func(string)
}

func (q *vlACKQueries) mark(step string) {
	if q.prefix != nil {
		q.prefix(step)
	}
}
func (q *vlACKQueries) LockSettingsAdmission(ctx context.Context) error {
	q.mark("settings")
	return q.Queries.LockSettingsAdmission(ctx)
}
func (q *vlACKQueries) LockSecretMutation(ctx context.Context, id uuid.UUID) error {
	q.mark("secret")
	return q.Queries.LockSecretMutation(ctx, id)
}
func (q *vlACKQueries) LockVaultAuthState(ctx context.Context, id uuid.UUID) (store.LockVaultAuthStateRow, error) {
	q.mark("users")
	return q.Queries.LockVaultAuthState(ctx, id)
}
func (q *vlACKQueries) LockUserVault(ctx context.Context, id uuid.UUID) (store.UserVault, error) {
	q.mark("wrapper")
	return q.Queries.LockUserVault(ctx, id)
}

func (q *vlACKQueries) DeleteUserVault(ctx context.Context, id uuid.UUID) (int64, error) {
	q.deletes.Add(1)
	return q.Queries.DeleteUserVault(ctx, id)
}

// The replacement writer takes the public advisory/fence seam and uses the
// current transaction's state reader. Recovery uses a separate cold Vault so the
// supported locked recovery exception writes master_aad at the SAME epoch.
func vlRaceReplace(ctx context.Context, f vlFixture, warm, cold *vault.Vault, pid chan<- uint32) error {
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer vlRaceRollback(tx)
	if pid != nil {
		pid <- tx.Conn().PgConn().PID()
	}
	q := store.New(tx)
	if err := q.LockSecretMutation(ctx, f.owner); err != nil {
		return err
	}
	state, err := q.GetUserVaultState(ctx, f.owner)
	if err != nil {
		return err
	}
	if _, err := q.FenceVaultWrite(ctx, f.owner, state.VaultMode, state.VaultEpoch); err != nil {
		return err
	}
	bound := vault.WithStateReader(ctx, q)
	for _, kind := range []string{store.KindAnthropicToken, store.KindOpenAIAPIKey, store.KindCodexAuth} {
		sealed, err := warm.Seal(bound, f.owner, kind, []byte("replacement-"+kind), vault.SealForUse)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE user_secrets SET ciphertext=$2,sealed_with=$3 WHERE user_id=$1 AND kind=$4", f.owner, sealed.Ciphertext, sealed.SealedWith, kind)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("replacement secret cardinality")
		}
	}
	live, err := warm.Seal(bound, f.owner, store.KindCodexAuth, []byte("replacement-live"), vault.SealForUse)
	if err != nil {
		return err
	}
	recovery, err := cold.Seal(bound, f.owner, store.KindCodexAuth, []byte("replacement-recovery"), vault.SealForRecovery)
	if err != nil {
		return err
	}
	if !recovery.RecoveryMaster || recovery.Epoch != state.VaultEpoch {
		return errors.New("same epoch recovery fixture did not use locked exception")
	}
	tag, err := tx.Exec(ctx, "UPDATE codex_provider_account SET sealed_login=$2,sealed_with=$3,recovery_sealed=$4,recovery_sealed_with=$5 WHERE user_id=$1", f.owner, live.Ciphertext, live.SealedWith, recovery.Ciphertext, recovery.SealedWith)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 2 {
		return errors.New("replacement account cardinality")
	}
	return tx.Commit(ctx)
}
func vlRaceReplacementReadable(t *testing.T, ctx context.Context, f vlFixture, v *vault.Vault, recoveryMarker string) {
	t.Helper()
	rows, err := f.pool.Query(ctx, "SELECT kind,ciphertext,sealed_with FROM user_secrets WHERE user_id=$1 ORDER BY id", f.owner)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var kind, marker string
		var ct []byte
		if err := rows.Scan(&kind, &ct, &marker); err != nil {
			t.Fatal(err)
		}
		plain, err := v.Open(f.owner, kind, marker, ct)
		if err != nil || marker != store.SealedWithDEK || !bytes.Equal(plain, []byte("replacement-"+kind)) {
			t.Fatalf("current replacement secret differs: kind=%s open_error=%v", kind, err)
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 3 {
		t.Fatal("replacement secrets not completely checked")
	}
	rows, err = f.pool.Query(ctx, "SELECT sealed_login,sealed_with,recovery_sealed,recovery_sealed_with FROM codex_provider_account WHERE user_id=$1 ORDER BY id", f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count = 0
	for rows.Next() {
		var live, recovery []byte
		var lm, rm string
		if err := rows.Scan(&live, &lm, &recovery, &rm); err != nil {
			t.Fatal(err)
		}
		for _, slot := range []struct {
			ct                       []byte
			marker, want, wantMarker string
		}{{live, lm, "replacement-live", store.SealedWithDEK}, {recovery, rm, "replacement-recovery", recoveryMarker}} {
			plain, err := v.Open(f.owner, store.KindCodexAuth, slot.marker, slot.ct)
			if err != nil || slot.marker != slot.wantMarker || !bytes.Equal(plain, []byte(slot.want)) {
				t.Fatalf("current account slot differs: marker=%s open_error=%v", slot.marker, err)
			}
		}
		count++
	}
	if rows.Err() != nil || count != 2 {
		t.Fatal("replacement account slots not completely checked")
	}
}
func vlRaceRowsLocked(t *testing.T, ctx context.Context, f vlFixture) {
	t.Helper()
	for _, table := range []struct {
		name  string
		count int
	}{{"codex_provider_account", 2}, {"user_secrets", 3}} {
		rows, err := f.pool.Query(ctx, "SELECT id FROM "+table.name+" WHERE user_id=$1 ORDER BY id", f.owner)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(ids) != table.count {
			t.Fatal("incomplete current-row lock probe fixture")
		}
		// Probe every row independently: an error on the first row must not hide
		// missing locks on its siblings. There are exactly five fixture rows.
		for _, id := range ids {
			tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			var locked uuid.UUID
			err = tx.QueryRow(ctx, "SELECT id FROM "+table.name+" WHERE user_id=$1 AND id=$2 FOR UPDATE NOWAIT", f.owner, id).Scan(&locked)
			vlRaceRollback(tx)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
				t.Fatalf("lazy batch did not hold current %s row %s", table.name, id)
			}
		}
	}
}

func TestVaultProtectedLazyRewrapRacesLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	for _, order := range []string{"writer-before-lazy", "lazy-before-writer"} {
		for _, writer := range []string{"same-epoch-all-slots", "disable"} {
			t.Run(order+"/"+writer, func(t *testing.T) {
				f := vlSeed(t, pool, true, true)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				writerPID := make(chan uint32, 1)
				var observeDisable atomic.Bool
				warmTx := vlRaceTransact(pool, func(tx pgx.Tx, _ int) vault.Store {
					return &vlRaceQueries{Queries: store.New(tx), beforeSecret: func(context.Context) error {
						if observeDisable.Load() {
							select {
							case writerPID <- tx.Conn().PgConn().PID():
							default:
							}
						}
						return nil
					}}
				})
				warm := vault.New(f.master, store.New(pool), warmTx)
				if err := warm.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				// Restore legacy master material in every slot without changing ANY metadata
				// or epoch. This is the supported lazy-upgrade starting state.
				for _, kind := range []string{store.KindAnthropicToken, store.KindOpenAIAPIKey, store.KindCodexAuth} {
					ct, err := f.master.Seal([]byte("secret-" + kind))
					if err != nil {
						t.Fatal(err)
					}
					vlExec(t, pool, "UPDATE user_secrets SET ciphertext=$2,sealed_with='master' WHERE user_id=$1 AND kind=$3", f.owner, ct, kind)
				}
				for i := 0; i < 2; i++ {
					live, err := f.master.Seal([]byte(fmt.Sprintf("live-%d", i)))
					if err != nil {
						t.Fatal(err)
					}
					recovery, err := f.master.Seal([]byte(fmt.Sprintf("recovery-%d", i)))
					if err != nil {
						t.Fatal(err)
					}
					vlExec(t, pool, "UPDATE codex_provider_account SET sealed_login=$2,sealed_with='master',recovery_sealed=$3,recovery_sealed_with='master' WHERE user_id=$1 AND provider_user_id=$4", f.owner, live, recovery, fmt.Sprintf("principal-%d", i))
				}
				metadata := f.snapshot(t, true)
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				var lazyPID uint32
				tx := vlRaceTransact(pool, func(tx pgx.Tx, call int) vault.Store {
					q := &vlRaceQueries{Queries: store.New(tx)}
					if call == 2 {
						lazyPID = tx.Conn().PgConn().PID()
						hook := func(ctx context.Context) error { close(entered); return vlRaceWait(ctx, release) }
						if order == "writer-before-lazy" {
							q.beforeSecret = hook
						} else {
							q.afterSecrets = hook
						}
					}
					return q
				})
				lazy := vault.New(f.master, store.New(pool), tx)
				cold := vault.NewPostgres(f.master, pool)
				lazyDone, writerDone := make(chan error, 1), make(chan error, 1)
				lazyJoined, writerStarted, writerJoined := false, false, false
				defer func() {
					unblock()
					cancel()
					if !lazyJoined {
						vlRaceJoin(t, lazyDone)
					}
					if writerStarted && !writerJoined {
						vlRaceJoin(t, writerDone)
					}
				}()
				go func() { lazyDone <- lazy.UnlockExisting(ctx, f.owner, vlLoginPhrase) }()
				select {
				case <-entered:
				case err := <-lazyDone:
					lazyJoined = true
					t.Fatalf("lazy conversion returned before barrier: %v", err)
				case <-ctx.Done():
					t.Fatal("lazy conversion never reached query barrier")
				}
				if order == "lazy-before-writer" {
					vlRaceRowsLocked(t, ctx, f)
				}
				writerStarted = true
				go func() {
					if writer == "same-epoch-all-slots" {
						writerDone <- vlRaceReplace(ctx, f, warm, cold, writerPID)
						return
					}
					// A separate core instance avoids process-local lifecycle serialization.
					observeDisable.Store(true)
					writerDone <- warm.Disable(ctx, f.owner)
				}()
				if order == "writer-before-lazy" {
					err := <-writerDone
					writerJoined = true
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var pid uint32
					select {
					case pid = <-writerPID:
					case <-ctx.Done():
						t.Fatal("writer never attempted transaction")
					}
					vlRaceBlocked(t, ctx, pool, pid, lazyPID)
					select {
					case err := <-writerDone:
						writerJoined = true
						t.Fatalf("writer returned before lazy barrier release: %v", err)
					default:
					}
				}
				unblock()
				lazyErr := <-lazyDone
				lazyJoined = true
				if !writerJoined {
					err := <-writerDone
					writerJoined = true
					if err != nil {
						t.Fatal(err)
					}
				}
				if writer == "disable" {
					if order == "writer-before-lazy" {
						if !errors.Is(lazyErr, vault.ErrInvalidState) {
							t.Fatalf("lazy phase accepted retired mode/epoch: %v", lazyErr)
						}
					} else if lazyErr != nil {
						t.Fatal(lazyErr)
					}
					f.state(t, cold, store.VaultModeUnprotected, 2, false, false)
					f.readable(t, cold, store.SealedWithMasterAAD)
				} else {
					if lazyErr != nil {
						t.Fatal(lazyErr)
					}
					recoveryMarker := store.SealedWithDEK
					if order == "lazy-before-writer" {
						recoveryMarker = store.SealedWithMasterAAD
					}
					vlRaceReplacementReadable(t, ctx, f, lazy, recoveryMarker)
					f.state(t, lazy, store.VaultModeProtected, 1, true, true)
				}
				vlEqual(t, metadata, f.snapshot(t, true), "lazy race complete metadata")
			})
		}
	}
}
