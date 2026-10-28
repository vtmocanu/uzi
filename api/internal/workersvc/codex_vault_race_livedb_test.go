package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

const codexRacePassword = "refresh-conversion-fixture-password" //nolint:gosec // G101: public passphrase for isolated test users, never a provider credential.

// The base exchange completes exactly once before the lifecycle callback. No
// lifecycle call runs under the material writer's advisory/users/account locks.
type codexRaceProvider struct {
	*fakeRefreshClient
	after func()
}

func (p *codexRaceProvider) Refresh(ctx context.Context, token string) (codexauth.RefreshResult, error) {
	result, err := p.fakeRefreshClient.Refresh(ctx, token)
	if err == nil && p.after != nil {
		p.after()
	}
	return result, err
}

func codexRaceFixture(t *testing.T, e codexTestEnv) (refreshFixture, *codexRaceProvider) {
	t.Helper()
	nextRefresh := "rotation-refresh-result"
	p := &codexRaceProvider{fakeRefreshClient: &fakeRefreshClient{result: codexauth.RefreshResult{
		AccessToken: "rotation-access-result", RefreshToken: &nextRefresh,
	}}}
	f := newRefreshFixture(t, e, p)
	p.matchIdentity = codexauth.Identity{ProviderUserID: f.providerUserID, WorkspaceAccountID: f.workspaceAcctID}
	hash, err := auth.HashPassword(codexRacePassword)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE users SET password_hash=$2 WHERE id=$1", f.userID, hash)
	optionalWorkerVaultPolicy(t, e)
	return f, p
}

// Compare all metadata, including the in-progress lease that already exists
// inside Refresh. Only the four account crypto fields and two alias crypto
// fields may change. Full worker/run rows pin capabilities and binding fences.
func codexRaceMetadata(t *testing.T, e codexTestEnv, owner uuid.UUID) string {
	t.Helper()
	var snapshot string
	err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_object(
		'accounts', (SELECT coalesce(jsonb_agg(to_jsonb(a) - ARRAY['sealed_login','sealed_with','recovery_sealed','recovery_sealed_with'] ORDER BY a.id), '[]'::jsonb) FROM codex_provider_account a WHERE a.user_id=$1),
		'aliases', (SELECT coalesce(jsonb_agg(to_jsonb(s) - ARRAY['ciphertext','sealed_with'] ORDER BY s.id), '[]'::jsonb) FROM user_secrets s WHERE s.user_id=$1),
		'states', (SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.user_secret_id), '[]'::jsonb) FROM codex_credential_state c JOIN user_secrets s ON s.id=c.user_secret_id WHERE s.user_id=$1),
		'workers', (SELECT coalesce(jsonb_agg(to_jsonb(w) ORDER BY w.id), '[]'::jsonb) FROM workers w WHERE w.user_id=$1),
		'runs', (SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.id), '[]'::jsonb) FROM runs r WHERE r.user_id=$1),
		'user', (SELECT to_jsonb(u) - ARRAY['vault_mode','vault_epoch'] FROM users u WHERE u.id=$1)
	)::text`, owner).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func codexRaceToggle(t *testing.T, e codexTestEnv, f refreshFixture, enable bool) {
	t.Helper()
	before := codexRaceMetadata(t, e, f.userID)
	state, err := f.svc.vlt.State(e.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if enable {
		err = f.svc.vlt.Enable(e.ctx, f.userID, codexRacePassword)
	} else {
		err = f.svc.vlt.Disable(e.ctx, f.userID)
	}
	if err != nil {
		t.Fatal(err)
	}
	if after := codexRaceMetadata(t, e, f.userID); after != before {
		t.Fatal("crypto-only conversion changed credential, lease, revision, default, capability or binding metadata")
	}
	after, err := f.svc.vlt.State(e.ctx, f.userID)
	wantMode := store.VaultModeUnprotected
	if enable {
		wantMode = store.VaultModeProtected
	}
	if err != nil || after.VaultMode != wantMode || after.VaultEpoch != state.VaultEpoch+1 {
		t.Fatalf("conversion state = %+v, %v; want %s epoch %d", after, err, wantMode, state.VaultEpoch+1)
	}
}

func codexRaceLogin(t *testing.T, v *vault.Vault, f refreshFixture, marker string, ciphertext []byte) {
	t.Helper()
	raw, err := v.Open(f.userID, "codex_auth", marker, ciphertext)
	if err != nil {
		t.Fatalf("open rotated login under current authority: %v", err)
	}
	defer clear(raw)
	var blob codexLoginBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatal(err)
	}
	if blob.AccessToken != "rotation-access-result" || blob.RefreshToken != "rotation-refresh-result" {
		t.Fatal("retained login differs from the single provider result")
	}
}

// Restart with no cached key, then use the existing recovery interface. This
// checks durable wrappers/markers rather than relying on the converting cache.
func codexRaceRestart(t *testing.T, e codexTestEnv, f refreshFixture, p *codexRaceProvider, op uuid.UUID, recovery bool) {
	t.Helper()
	v := vault.NewPostgres(e.box, e.pool)
	state, err := v.State(e.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Unlocked(f.userID) {
		t.Fatal("new vault unexpectedly has a cached key")
	}
	pre := e.mustAccount(t, f.userID, f.accountID)
	if state.VaultMode == store.VaultModeProtected {
		if _, err := v.Open(f.userID, "codex_auth", pre.SealedWith, pre.SealedLogin); !errors.Is(err, vault.ErrLocked) {
			t.Fatalf("protected restart live open = %v, want locked", err)
		}
		if err := v.UnlockExisting(e.ctx, f.userID, codexRacePassword); err != nil {
			t.Fatal(err)
		}
	} else if v.RequiresUnlock(e.ctx, f.userID) {
		t.Fatal("off restart requires unlock")
	}
	f.svc.vlt, f.svc.q = v, e.q
	if recovery {
		n, err := f.svc.ReconcileUnresolvedCodexRefresh(e.ctx, f.userID, f.accountID)
		if err != nil || n != 1 {
			t.Fatalf("recovery promotion = %d, %v; want 1, nil", n, err)
		}
	}
	acct := e.mustAccount(t, f.userID, f.accountID)
	wantMarker := store.SealedWithMasterAAD
	if state.VaultMode == store.VaultModeProtected {
		wantMarker = store.SealedWithDEK
	}
	if acct.Generation != 1 || acct.CoordState != "idle" || acct.SealedWith != wantMarker || len(acct.RecoverySealed) != 0 {
		t.Fatalf("final account gen=%d state=%s key=%s recovery=%d", acct.Generation, acct.CoordState, acct.SealedWith, len(acct.RecoverySealed))
	}
	codexRaceLogin(t, v, f, acct.SealedWith, acct.SealedLogin)
	intent := mustIntent(t, e, op, f.userID)
	wantIntent := codexIntentCommitted
	if recovery {
		wantIntent = codexIntentReconciled
	}
	if intent.State != wantIntent || intent.FromGeneration != 0 || intent.ProviderAccountID != f.accountID {
		t.Fatalf("final intent = %+v", intent)
	}
	if p.calls != 1 || p.lastRefreshToken != f.prevRefreshToken {
		t.Fatalf("provider exchanges=%d; must spend the original refresh exactly once", p.calls)
	}
}

func codexRaceRecovery(t *testing.T, e codexTestEnv, f refreshFixture, op uuid.UUID, locked bool) {
	t.Helper()
	a := e.mustAccount(t, f.userID, f.accountID)
	if a.Generation != 0 || a.CoordState != codexCoordQuarantined || !a.CoordOperationID.Valid || uuid.UUID(a.CoordOperationID.Bytes) != op ||
		!a.RecoveryGeneration.Valid || a.RecoveryGeneration.Int64 != 0 || !a.RecoverySealedWith.Valid || len(a.RecoverySealed) == 0 {
		t.Fatal("recovery did not preserve operation/generation coordinates")
	}
	state, err := f.svc.vlt.State(e.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	if locked {
		if a.RecoverySealedWith.String != store.SealedWithMasterAAD || !a.RecoveryCause.Valid || a.RecoveryCause.String != "vault_locked" {
			t.Fatal("locked protected recovery lacks master AAD marker/vault_locked cause")
		}
	} else if state.VaultMode == store.VaultModeUnprotected {
		if a.RecoverySealedWith.String != store.SealedWithMasterAAD || a.RecoveryCause.Valid {
			t.Fatal("off recovery must use master AAD without a vault lock cause")
		}
	} else if a.RecoverySealedWith.String == store.SealedWithDEK {
		if a.RecoveryCause.Valid {
			t.Fatal("unlocked DEK recovery acquired a lock cause")
		}
	} else if a.RecoverySealedWith.String != store.SealedWithMasterAAD || !a.RecoveryCause.Valid || a.RecoveryCause.String != "vault_locked" {
		t.Fatal("protected publication-gap recovery has invalid key/cause")
	}
	codexRaceLogin(t, f.svc.vlt, f, a.RecoverySealedWith.String, a.RecoverySealed)
	if it := mustIntent(t, e, op, f.userID); it.State != codexIntentRotating {
		t.Fatalf("retained intent state=%s, want rotating", it.State)
	}
}

func TestCodexRefreshVaultConversionBarrierLiveDB(t *testing.T) {
	for _, mode := range []string{"enable", "disable", "disable-reenable", "enable-lock"} {
		t.Run(mode, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			f, p := codexRaceFixture(t, e)
			if mode == "disable" || mode == "disable-reenable" {
				codexRaceToggle(t, e, f, true)
			}
			op := uuid.New()
			p.after = func() {
				a := e.mustAccount(t, f.userID, f.accountID)
				if a.CoordState != "in_progress" || a.Generation != 0 || !a.CoordOperationID.Valid || uuid.UUID(a.CoordOperationID.Bytes) != op {
					t.Fatal("provider barrier must follow lease acquisition")
				}
				switch mode {
				case "enable", "enable-lock":
					codexRaceToggle(t, e, f, true)
				case "disable":
					codexRaceToggle(t, e, f, false)
				case "disable-reenable":
					codexRaceToggle(t, e, f, false)
					codexRaceToggle(t, e, f, true)
				}
				if mode == "enable-lock" {
					f.svc.vlt.Lock(f.userID)
				}
			}
			result, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, e.mintCap(t, f.runID, f.workerID), op, 0)
			locked := mode == "enable-lock"
			if locked {
				if !errors.Is(err, ErrCodexVaultLocked) || result.AccessToken != "" {
					t.Fatalf("locked result=%+v err=%v", result, err)
				}
				codexRaceRecovery(t, e, f, op, true)
			} else {
				if err != nil || result.Outcome != CodexRefreshAdvanced || result.Generation != 1 || result.AccessToken != "rotation-access-result" {
					t.Fatalf("advance result=%+v err=%v", result, err)
				}
				a := e.mustAccount(t, f.userID, f.accountID)
				codexRaceLogin(t, f.svc.vlt, f, a.SealedWith, a.SealedLogin)
			}
			codexRaceRestart(t, e, f, p, op, locked)
		})
	}
}

// One wrapper retains both fault hooks when rebound to the actual transaction.
// Failed slot attempts return before rollback; toggles happen only after the
// service queues its owned raw clone and all foreground transactions have ended.
type codexRaceFaults struct{ slots int }

type codexRaceFaultStore struct {
	*store.Queries
	faults *codexRaceFaults
}

func (s codexRaceFaultStore) bindCodexRefreshTx(q *store.Queries) codexRefreshStore {
	return codexRaceFaultStore{Queries: q, faults: s.faults}
}

func (s codexRaceFaultStore) CommitCodexRefresh(context.Context, store.CommitCodexRefreshParams) (store.CommitCodexRefreshRow, error) {
	return store.CommitCodexRefreshRow{}, errors.New("test foreground commit failure")
}

func (s codexRaceFaultStore) SetCodexRecoverySlot(ctx context.Context, arg store.SetCodexRecoverySlotParams) (int64, error) {
	s.faults.slots++
	if s.faults.slots <= 2 {
		return 0, errors.New("test synchronous recovery failure")
	}
	return s.Queries.SetCodexRecoverySlot(ctx, arg)
}

func TestCodexRefreshBackgroundVaultABALiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	f, p := codexRaceFixture(t, e)
	codexRaceToggle(t, e, f, true)
	// Keep a distinct vault with retired key A. It must not open key B material.
	retired := f.svc.vlt
	f.svc.vlt = vault.NewPostgres(e.box, e.pool)
	if err := f.svc.vlt.UnlockExisting(e.ctx, f.userID, codexRacePassword); err != nil {
		t.Fatal(err)
	}
	faults := &codexRaceFaults{}
	f.svc.q = codexRaceFaultStore{Queries: e.q, faults: faults}
	var queued func()
	f.svc.background = func(fn func()) {
		if queued != nil {
			t.Fatal("multiple background handoffs")
		}
		queued = fn
	}
	op := uuid.New()
	result, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, e.mintCap(t, f.runID, f.workerID), op, 0)
	if !errors.Is(err, ErrCodexRefreshQuarantined) || result.AccessToken != "" || queued == nil || faults.slots != 2 {
		t.Fatalf("background handoff result=%+v err=%v slots=%d queued=%t", result, err, faults.slots, queued != nil)
	}
	if a := e.mustAccount(t, f.userID, f.accountID); len(a.RecoverySealed) != 0 || a.Generation != 0 {
		t.Fatal("failed synchronous transactions persisted material")
	}
	codexRaceToggle(t, e, f, false)
	codexRaceToggle(t, e, f, true)
	queued()
	if faults.slots != 3 {
		t.Fatalf("slot attempts=%d, want two rolled back plus one background", faults.slots)
	}
	codexRaceRecovery(t, e, f, op, false)
	a := e.mustAccount(t, f.userID, f.accountID)
	if a.RecoverySealedWith.String != store.SealedWithDEK {
		t.Fatal("completed re-enable must persist with current key B")
	}
	if raw, err := retired.Open(f.userID, "codex_auth", store.SealedWithDEK, a.RecoverySealed); err == nil {
		clear(raw)
		t.Fatal("retired key A opened recovery sealed after re-enable")
	}
	codexRaceRestart(t, e, f, p, op, true)
}

// No production timer is shortened: the factory maps the requested foreground
// and background phases to narrow test deadlines. The conversion still holds
// real RC/advisory/users locks, a private wrapper and converted material.
func TestCodexRefreshBackgroundWaitsForVaultConversionLiveDB(t *testing.T) {
	for _, enable := range []bool{true, false} {
		name := "disable"
		if enable {
			name = "enable"
		}
		t.Run(name, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			f, p := codexRaceFixture(t, e)
			if !enable {
				codexRaceToggle(t, e, f, true)
			}
			reached, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			conversionDone := make(chan error, 1)
			bgStarted, bgDone := make(chan struct{}), make(chan struct{})
			var bgOnce sync.Once
			var conversionLaunched, backgroundLaunched atomic.Bool
			// Registered after pool cleanup, so all owned goroutines finish first.
			t.Cleanup(func() {
				unblock()
				if backgroundLaunched.Load() {
					select {
					case <-bgDone:
					case <-time.After(5 * time.Second):
						t.Error("background goroutine did not finish during cleanup")
					}
				}
			})
			conversionJoined := make(chan struct{})
			conversionCtx, cancelConversion := context.WithTimeout(e.ctx, 10*time.Second)
			t.Cleanup(func() {
				unblock()
				cancelConversion()
				if conversionLaunched.Load() {
					select {
					case <-conversionJoined:
					case <-time.After(5 * time.Second):
						t.Error("conversion goroutine did not finish during cleanup")
					}
				}
			})
			// Unlock uses Transact too. Arm only after loading Disable's key.
			var armed atomic.Bool
			controlled := vault.New(e.box, e.q, func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
				tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				if err != nil {
					return err
				}
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_ = tx.Rollback(cleanup)
				}()
				q := store.New(tx)
				if err := fn(vault.WithStateReader(ctx, q), q); err != nil {
					return err
				}
				if armed.CompareAndSwap(true, false) {
					close(reached)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if err := tx.Commit(ctx); err != nil {
					return &vault.CommitError{Err: err}
				}
				return nil
			})
			if !enable {
				if err := controlled.UnlockExisting(e.ctx, f.userID, codexRacePassword); err != nil {
					t.Fatal(err)
				}
			}
			armed.Store(true)
			f.svc.vlt = controlled
			var shortCalls, shortExpirations atomic.Int32
			f.svc.codexWriteContext = func(ctx context.Context, requested time.Duration) (context.Context, context.CancelFunc) {
				d := 20 * time.Millisecond
				if requested > vault.LifecycleTimeout {
					bgOnce.Do(func() { close(bgStarted) })
					d = 500 * time.Millisecond
				} else {
					shortCalls.Add(1)
				}
				writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
				return writeCtx, func() {
					if requested <= vault.LifecycleTimeout && errors.Is(writeCtx.Err(), context.DeadlineExceeded) {
						shortExpirations.Add(1)
					}
					cancel()
				}
			}
			f.svc.background = func(fn func()) {
				if !backgroundLaunched.CompareAndSwap(false, true) {
					t.Error("multiple background launches")
					return
				}
				go func() { defer close(bgDone); fn() }()
			}
			p.after = func() {
				conversionLaunched.Store(true)
				go func() {
					defer close(conversionJoined)
					if enable {
						conversionDone <- controlled.Enable(conversionCtx, f.userID, codexRacePassword)
					} else {
						conversionDone <- controlled.Disable(conversionCtx, f.userID)
					}
				}()
				select {
				case <-reached:
				case err := <-conversionDone:
					t.Fatalf("conversion ended before pre-commit barrier: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("conversion never reached pre-commit barrier")
				}
				if enable && controlled.Unlocked(f.userID) {
					t.Fatal("Enable published the private key before commit")
				}
			}
			op := uuid.New()
			result, err := f.svc.CoordinatedCodexRefresh(e.ctx, f.wkr, f.runID, e.mintCap(t, f.runID, f.workerID), op, 0)
			if !errors.Is(err, ErrCodexRefreshQuarantined) || result.AccessToken != "" {
				t.Fatalf("waiting result=%+v err=%v", result, err)
			}
			select {
			case <-bgStarted:
			case <-bgDone:
				t.Fatal("background exhausted without requesting the long write phase")
			case <-time.After(time.Second):
				t.Fatal("background never requested the long write phase")
			}
			if shortCalls.Load() != 3 || shortExpirations.Load() != 3 {
				t.Fatalf("short phases=%d expirations=%d, want normal commit plus two sync attempts expiring behind conversion", shortCalls.Load(), shortExpirations.Load())
			}
			unblock()
			select {
			case err := <-conversionDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("conversion did not commit")
			}
			select {
			case <-bgDone:
			case <-time.After(5 * time.Second):
				t.Fatal("background persistence did not finish")
			}
			codexRaceRecovery(t, e, f, op, false)
			f.svc.codexWriteContext = nil
			codexRaceRestart(t, e, f, p, op, true)
		})
	}
}
