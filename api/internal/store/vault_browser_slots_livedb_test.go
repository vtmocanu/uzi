package store_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

type bsFixture struct {
	pool   *pgxpool.Pool
	id     uuid.UUID
	master *secretbox.Box
	v      *vault.Vault
}

func bsSeed(t *testing.T, pool *pgxpool.Pool) bsFixture {
	t.Helper()
	id := uuid.New()
	hash, err := auth.HashPassword(vlLoginPhrase)
	if err != nil {
		t.Fatal(err)
	}
	vlExec(t, pool, "INSERT INTO users(id,email,password_hash,vault_mode) VALUES($1,$2,$3,'protected')", id, id.String()+"@example.test", hash)
	master, err := secretbox.New(bytes.Repeat([]byte{79}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return bsFixture{pool: pool, id: id, master: master, v: vault.NewPostgres(master, pool)}
}
func (f bsFixture) user(t *testing.T) store.User {
	t.Helper()
	user, err := store.New(f.pool).GetUserByID(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	return user
}
func (f bsFixture) mint(t *testing.T, browser uuid.UUID) *vault.BrowserKey {
	t.Helper()
	key, err := f.v.UnlockAndMint(context.Background(), vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveCreate,
		&vault.BrowserMintRequest{HTTPS: true, BrowserID: browser, SessionExpiresAt: time.Now().Add(2 * time.Hour)})
	if err != nil || key == nil {
		t.Fatalf("mint: %v", err)
	}
	t.Cleanup(key.Destroy)
	return key
}
func (f bsFixture) count(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM vault_browser_slots WHERE user_id=$1", f.id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
func (f bsFixture) session(t *testing.T, browser uuid.UUID) vault.VerifiedSession {
	user := f.user(t)
	return vault.VerifiedSession{Auth: vault.Snapshot(user), TokenVersion: user.TokenVersion, BrowserID: browser}
}
func (f bsFixture) cold() *vault.Vault { return vault.NewPostgres(f.master, f.pool) }

func TestVaultBrowserSlotsLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	ctx := context.Background()
	t.Run("restore constraints and absolute lifetime", func(t *testing.T) {
		f := bsSeed(t, pool)
		browser := uuid.New()
		key := f.mint(t, browser)
		value, err := key.CookieValue()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := vault.DecodeBrowserKey(value)
		if err != nil {
			t.Fatal(err)
		}
		defer decoded.Destroy()
		before := f.user(t)
		var wrapped []byte
		var expiry time.Time
		if err := pool.QueryRow(ctx, "SELECT wrapped_dek,expires_at FROM vault_browser_slots WHERE user_id=$1", f.id).Scan(&wrapped, &expiry); err != nil {
			t.Fatal(err)
		}
		if time.Until(key.ExpiresAt()) > 2*time.Hour {
			t.Fatal("session expiry not applied")
		}
		vlExec(t, pool, "INSERT INTO app_settings(key,value) VALUES('vault_reunlock_days','1') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value")
		cold := f.cold()
		if err := cold.Restore(ctx, f.session(t, browser), decoded); err != nil || !cold.Unlocked(f.id) {
			t.Fatalf("restore: %v", err)
		}
		var afterWrapped []byte
		var afterExpiry time.Time
		if err := pool.QueryRow(ctx, "SELECT wrapped_dek,expires_at FROM vault_browser_slots WHERE user_id=$1", f.id).Scan(&afterWrapped, &afterExpiry); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wrapped, afterWrapped) || !expiry.Equal(afterExpiry) || f.count(t) != 1 {
			t.Fatal("restore extended/minted")
		}
		// Exercise the real minted row, with its actual AAD. A detached crypto
		// example or a wrong-AAD master probe would not catch master wrapping.
		parts := strings.Split(value, ".")
		slotID, err := uuid.Parse(parts[1])
		if err != nil {
			t.Fatal("invalid minted slot identity")
		}
		cookieSecret, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal("invalid minted cookie encoding")
		}
		defer clear(cookieSecret)
		aad := append([]byte("uzi-vault-browser-dek-v1\x00"), f.id[:]...)
		aad = append(aad, slotID[:]...)
		cookieBox, err := secretbox.New(cookieSecret)
		if err != nil {
			t.Fatal("invalid minted cookie key")
		}
		dek, err := cookieBox.OpenWithAAD(wrapped, aad)
		if err != nil || len(dek) != 32 {
			t.Fatal("cookie key did not open the actual persisted DEK envelope")
		}
		defer clear(dek)
		if _, err := f.master.OpenWithAAD(wrapped, aad); err == nil {
			t.Fatal("server key opened the actual slot without its cookie")
		}
		metadataKey := append(append([]byte(nil), f.id[:]...), slotID[:]...)
		metadataBox, err := secretbox.New(metadataKey)
		if err != nil {
			t.Fatal("metadata test key")
		}
		if _, err := metadataBox.OpenWithAAD(wrapped, aad); err == nil {
			t.Fatal("database metadata opened the actual slot without its cookie")
		}
		const kind = "browser-slot-proof"
		plain := []byte("credential encrypted by the cached DEK")
		sealed, err := f.v.Seal(ctx, f.id, kind, plain, vault.SealForUse)
		if err != nil || sealed.SealedWith != store.SealedWithDEK {
			t.Fatal("failed to seal DEK proof")
		}
		dekBox, err := secretbox.New(dek)
		if err != nil {
			t.Fatal("invalid recovered DEK")
		}
		materialAAD := append(append([]byte(nil), f.id[:]...), 0)
		materialAAD = append(materialAAD, kind...)
		opened, err := dekBox.OpenWithAAD(sealed.Ciphertext, materialAAD)
		if err != nil || !bytes.Equal(opened, plain) {
			t.Fatal("persisted slot did not wrap the live credential DEK")
		}
		clear(opened)
		cases := []struct {
			name    string
			user    store.User
			version int32
			browser uuid.UUID
		}{
			{"browser", before, before.TokenVersion, uuid.New()},
			{"version", before, before.TokenVersion + 1, browser},
			{"epoch", func() store.User { u := before; u.VaultEpoch++; return u }(), before.TokenVersion, browser},
			{"generation", func() store.User { u := before; u.VaultSlotGeneration++; return u }(), before.TokenVersion, browser},
			{"owner", func() store.User { u := before; u.ID = bsSeed(t, pool).id; return u }(), before.TokenVersion, browser},
			{"inactive snapshot", func() store.User { u := before; u.IsActive = false; return u }(), before.TokenVersion, browser},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				v := f.cold()
				if err := v.Restore(ctx, vault.VerifiedSession{Auth: vault.Snapshot(tc.user), TokenVersion: tc.version, BrowserID: tc.browser}, decoded); err == nil || v.Unlocked(f.id) {
					t.Fatal("invalid restore published")
				}
			})
		}
		// Keep the authenticated snapshot current while independently corrupting
		// persisted metadata, so snapshot rejection cannot mask these guards.
		for _, tc := range []struct {
			name, corrupt, reset string
			original             any
		}{
			{"persisted slot version", "UPDATE vault_browser_slots SET token_version=token_version+1 WHERE user_id=$1", "UPDATE vault_browser_slots SET token_version=$2 WHERE user_id=$1", before.TokenVersion},
			{"persisted slot epoch", "UPDATE vault_browser_slots SET vault_epoch=vault_epoch+1 WHERE user_id=$1", "UPDATE vault_browser_slots SET vault_epoch=$2 WHERE user_id=$1", before.VaultEpoch},
			{"persisted slot generation", "UPDATE vault_browser_slots SET generation=generation+1 WHERE user_id=$1", "UPDATE vault_browser_slots SET generation=$2 WHERE user_id=$1", before.VaultSlotGeneration},
		} {
			t.Run(tc.name, func(t *testing.T) {
				current := f.session(t, browser)
				vlExec(t, pool, tc.corrupt, f.id)
				t.Cleanup(func() { vlExec(t, pool, tc.reset, f.id, tc.original) })
				cold := f.cold()
				if err := cold.Restore(ctx, current, decoded); err == nil || cold.Unlocked(f.id) {
					t.Fatal("invalid persisted slot metadata published a DEK")
				}
			})
		}
		vlExec(t, pool, "UPDATE vault_browser_slots SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE user_id=$1", f.id)
		if err := f.cold().Restore(ctx, f.session(t, browser), decoded); err == nil {
			t.Fatal("expired restore")
		}
	})
	t.Run("admin lifetime and absent setting default", func(t *testing.T) {
		f := bsSeed(t, pool)
		for _, tc := range []struct {
			name, value string
			days        int
		}{
			{"absent setting", "", 7},
			{"minimum", "1", 1},
			{"maximum", "30", 30},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Cleanup(func() { vlExec(t, pool, "DELETE FROM app_settings WHERE key='vault_reunlock_days'") })
				if tc.value == "" {
					vlExec(t, pool, "DELETE FROM app_settings WHERE key='vault_reunlock_days'")
				} else {
					vlExec(t, pool, "INSERT INTO app_settings(key,value) VALUES('vault_reunlock_days',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", tc.value)
				}
				browser := uuid.New()
				key, err := f.v.UnlockAndMint(ctx, vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveCreate,
					&vault.BrowserMintRequest{HTTPS: true, BrowserID: browser, SessionExpiresAt: time.Now().Add(45 * 24 * time.Hour)})
				if err != nil || key == nil {
					t.Fatalf("admin-bounded mint: %v", err)
				}
				t.Cleanup(key.Destroy)
				var created, expires time.Time
				if err := pool.QueryRow(ctx, "SELECT created_at,expires_at FROM vault_browser_slots WHERE user_id=$1 AND browser_id=$2", f.id, browser).Scan(&created, &expires); err != nil {
					t.Fatal(err)
				}
				if expires.Sub(created) != time.Duration(tc.days)*24*time.Hour {
					t.Fatal("admin TTL or absent-setting default did not bound the minted slot")
				}
			})
		}
	})
	t.Run("durable lock and stale interactive snapshot", func(t *testing.T) {
		f := bsSeed(t, pool)
		browser := uuid.New()
		key := f.mint(t, browser)
		before := f.user(t)
		stale := f.session(t, browser)
		if err := f.v.LockAndAcknowledge(ctx, f.id); err != nil {
			t.Fatal(err)
		}
		after := f.user(t)
		if f.v.Unlocked(f.id) || f.count(t) != 0 || after.VaultEpoch != before.VaultEpoch || after.VaultSlotGeneration <= before.VaultSlotGeneration {
			t.Fatal("lock did not fence slots independently of epoch")
		}
		if err := f.v.Restore(ctx, stale, key); err == nil {
			t.Fatal("stale restore")
		}
		if k, err := f.v.UnlockAndMint(ctx, vault.Snapshot(before), vlLoginPhrase, vault.InteractiveExisting, &vault.BrowserMintRequest{HTTPS: true, BrowserID: browser, SessionExpiresAt: time.Now().Add(time.Hour)}); err == nil || k != nil || f.v.Unlocked(f.id) {
			t.Fatal("stale interactive auth survived lock")
		}
		f.mint(t, browser)
		f.v.Lock(f.id)
		if f.count(t) != 0 || f.v.Unlocked(f.id) {
			t.Fatal("legacy Lock allowed restore")
		}
	})
	t.Run("logout password and deactivation storage revocation", func(t *testing.T) {
		f := bsSeed(t, pool)
		browser := uuid.New()
		f.mint(t, browser)
		version, err := f.v.Logout(ctx, f.id)
		if err != nil || version != f.user(t).TokenVersion || !f.v.Unlocked(f.id) || f.count(t) != 0 {
			t.Fatalf("logout cache/version/slots: %v", err)
		}
		f.mint(t, browser)
		q := store.New(pool)
		hash := f.user(t).PasswordHash
		if err := q.UpdatePassword(ctx, store.UpdatePasswordParams{ID: f.id, PasswordHash: hash}); err != nil || f.count(t) != 0 {
			t.Fatalf("direct password query: %v", err)
		}
		f.mint(t, browser)
		if _, err := q.BumpTokenVersion(ctx, f.id); err != nil || f.count(t) != 0 {
			t.Fatalf("direct token bump: %v", err)
		}
		f.mint(t, browser)
		if err := f.v.UpdatePassword(ctx, store.UpdatePasswordParams{ID: f.id, PasswordHash: hash}); err != nil || f.count(t) != 0 {
			t.Fatalf("typed password seam: %v", err)
		}
		f.mint(t, browser)
		user, err := f.v.SetUserActive(ctx, store.SetUserActiveParams{ID: f.id, IsActive: false})
		if err != nil || user.IsActive || f.count(t) != 0 || f.v.Unlocked(f.id) {
			t.Fatalf("typed deactivation: %v", err)
		}
		if key, err := f.v.UnlockAndMint(ctx, vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveExisting, nil); err == nil || key != nil {
			t.Fatal("inactive unlock")
		}
	})
	t.Run("preference off and mode ABA", func(t *testing.T) {
		f := bsSeed(t, pool)
		browser := uuid.New()
		key := f.mint(t, browser)
		before := f.user(t)
		if err := f.v.SetAutoReunlock(ctx, vault.Snapshot(before), false); err != nil || f.count(t) != 0 {
			t.Fatalf("optout: %v", err)
		}
		if k, err := f.v.UnlockAndMint(ctx, vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveExisting, &vault.BrowserMintRequest{HTTPS: true, BrowserID: browser, SessionExpiresAt: time.Now().Add(time.Hour)}); err != nil || k != nil {
			t.Fatal("optout minted")
		}
		if err := f.v.SetAutoReunlock(ctx, vault.Snapshot(f.user(t)), true); err != nil || f.count(t) != 0 {
			t.Fatal("optin minted")
		}
		f.mint(t, browser)
		stale := f.session(t, browser)
		if err := f.v.Disable(ctx, f.id); err != nil || f.count(t) != 0 {
			t.Fatalf("off revocation: %v", err)
		}
		if k, err := f.v.UnlockAndMint(ctx, vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveExisting, nil); err != nil || k != nil {
			t.Fatal("Off unlock")
		}
		if err := f.v.Enable(ctx, f.id, vlLoginPhrase); err != nil {
			t.Fatal(err)
		}
		if err := f.cold().Restore(ctx, stale, key); err == nil {
			t.Fatal("mode ABA revived slot")
		}
	})
	t.Run("cap replacement expiry sweep and setting validation", func(t *testing.T) {
		f := bsSeed(t, pool)
		t.Cleanup(func() { vlExec(t, pool, "DELETE FROM app_settings WHERE key='vault_reunlock_days'") })
		vlExec(t, pool, "DELETE FROM app_settings WHERE key='vault_reunlock_days'")
		browsers := make([]uuid.UUID, 33)
		for i := range browsers {
			browsers[i] = uuid.New()
			f.mint(t, browsers[i])
		}
		if f.count(t) != 32 {
			t.Fatal("cap")
		}
		var firstPresent bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM vault_browser_slots WHERE user_id=$1 AND browser_id=$2)", f.id, browsers[0]).Scan(&firstPresent); err != nil || firstPresent {
			t.Fatal("oldest not evicted")
		}
		f.mint(t, browsers[32])
		if f.count(t) != 32 {
			t.Fatal("same browser replacement")
		}
		vlExec(t, pool, "UPDATE vault_browser_slots SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE user_id=$1", f.id)
		f.mint(t, uuid.New())
		if f.count(t) != 1 {
			t.Fatal("expired slots count against cap")
		}
		vlExec(t, pool, "UPDATE vault_browser_slots SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE user_id=$1", f.id)
		q := store.New(pool)
		n, err := q.DeleteExpiredVaultBrowserSlots(ctx, store.DeleteExpiredVaultBrowserSlotsParams{ObservedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, BatchSize: 1})
		if err != nil || n != 1 {
			t.Fatalf("bounded expiry delete: %d %v", n, err)
		}
		// The bounded global sweep can choose an earlier user's expired row.
		// Rejected minting must preserve this owner's population, whatever it is.
		beforeRejectedMint := f.count(t)
		for _, value := range []string{"0", "31", "invalid", ""} {
			vlExec(t, pool, "INSERT INTO app_settings(key,value) VALUES('vault_reunlock_days',$1) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", value)
			k, err := f.v.UnlockAndMint(ctx, vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveExisting, &vault.BrowserMintRequest{HTTPS: true, BrowserID: uuid.New(), SessionExpiresAt: time.Now().Add(time.Hour)})
			if err == nil || k != nil || f.count(t) != beforeRejectedMint {
				t.Fatalf("invalid DB setting %q changed slots", value)
			}
		}
		vlExec(t, pool, "DELETE FROM app_settings WHERE key='vault_reunlock_days'")
	})
	t.Run("rollback and overflow fail closed", func(t *testing.T) {
		f := bsSeed(t, pool)
		f.mint(t, uuid.New())
		before := f.user(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.New(tx).RevokeVaultBrowserSlots(ctx, f.id); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if f.count(t) != 1 || f.user(t).VaultSlotGeneration != before.VaultSlotGeneration {
			t.Fatal("revocation escaped rollback")
		}
		vlExec(t, pool, "UPDATE users SET vault_slot_generation=9223372036854775807 WHERE id=$1", f.id)
		if err := f.v.LockWithContext(ctx, f.id); err == nil || f.v.Unlocked(f.id) {
			t.Fatal("overflow lock success")
		}
		if _, err := store.New(pool).BumpTokenVersion(ctx, f.id); err == nil {
			t.Fatal("auth revocation overflow succeeded")
		}
	})
}

// bsBarrier blocks one slot read inside the owner's locked restore transaction.
// The wait is bounded by the request context; sibling work proceeds only after
// the test explicitly releases the barrier.
type bsBarrier struct {
	vault.BrowserStore
	once    *sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *bsBarrier) GetVaultBrowserSlot(ctx context.Context, arg store.GetVaultBrowserSlotParams) (store.VaultBrowserSlot, error) {
	slot, err := b.BrowserStore.GetVaultBrowserSlot(ctx, arg)
	b.once.Do(func() {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
	if ctx.Err() != nil {
		return store.VaultBrowserSlot{}, ctx.Err()
	}
	return slot, err
}
func bsWrapped(f bsFixture, wrap func(vault.BrowserStore) vault.BrowserStore, commitError bool, opts ...vault.Option) *vault.Vault {
	return bsWrappedAfterCommit(f, wrap, commitError, nil, opts...)
}

func bsWrappedAfterCommit(f bsFixture, wrap func(vault.BrowserStore) vault.BrowserStore, commitError bool, afterCommit func(context.Context) error, opts ...vault.Option) *vault.Vault {
	q := store.New(f.pool)
	return vault.New(f.master, q, func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		tq := q.WithTx(tx)
		if err := fn(vault.WithStateReader(ctx, tq), wrap(tq)); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return &vault.CommitError{Err: err}
		}
		if commitError {
			return &vault.CommitError{Err: errors.New("injected lost commit acknowledgment")}
		}
		if afterCommit != nil {
			if err := afterCommit(ctx); err != nil {
				return &vault.CommitError{Err: err}
			}
		}
		return nil
	}, opts...)
}
func bsWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("barrier deadline")
	}
}
func TestVaultBrowserRestoreLockRaceLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	f := bsSeed(t, pool)
	browser := uuid.New()
	key := f.mint(t, browser)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entered, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once, waitOnce sync.Once
	cold := bsWrapped(f, func(q vault.BrowserStore) vault.BrowserStore {
		return &bsBarrier{BrowserStore: q, once: &once, entered: entered, release: release}
	}, false,
		vault.WithLifecycleWaitObserverForTest(func(uuid.UUID) { waitOnce.Do(func() { close(waiting) }) }))
	restored, locked := make(chan error, 1), make(chan error, 1)
	go func() { restored <- cold.Restore(ctx, f.session(t, browser), key) }()
	bsWait(t, entered)
	stale := vault.Snapshot(f.user(t))
	go func() { locked <- cold.LockWithContext(ctx, f.id) }()
	bsWait(t, waiting)
	close(release)
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if cold.Unlocked(f.id) || f.count(t) != 0 {
		t.Fatal("read-before-lock restore survived durable revoke")
	}
	if k, err := cold.UnlockAndMint(ctx, stale, vlLoginPhrase, vault.InteractiveExisting, nil); err == nil || k != nil || cold.Unlocked(f.id) {
		t.Fatal("stale auth republished after lock")
	}
}

// bsPublicationRace pauses only the first successful commit, after its DB
// fences are released. Sibling transactions remain free to complete. Operation
// waits and cleanup joins are bounded separately; cleanup cancels and releases
// publication before joining work.
type bsPublicationRace struct {
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
	waiting chan struct{}
	unpause sync.Once
	work    sync.WaitGroup
}

func bsNewPublicationRace(t *testing.T, f bsFixture) (*vault.Vault, *bsPublicationRace) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	r := &bsPublicationRace{
		ctx: ctx, entered: make(chan struct{}), release: make(chan struct{}),
		waiting: make(chan struct{}, 1),
	}
	t.Cleanup(func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		r.cleanup(cancel, timer.C, func(message string) { t.Error(message) })
	})
	var paused atomic.Bool
	v := bsWrappedAfterCommit(f, func(q vault.BrowserStore) vault.BrowserStore { return q }, false,
		func(ctx context.Context) error {
			if !paused.CompareAndSwap(false, true) {
				return nil
			}
			close(r.entered)
			select {
			case <-r.release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		vault.WithLifecycleWaitObserverForTest(func(id uuid.UUID) {
			if id == f.id {
				select {
				case r.waiting <- struct{}{}:
				default:
				}
			}
		}))
	return v, r
}

func (r *bsPublicationRace) cleanup(cancel context.CancelFunc, deadline <-chan time.Time, report func(string)) {
	cancel()
	r.resume()
	joined := make(chan struct{})
	go func() {
		r.work.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-deadline:
		report("publication race cleanup join deadline")
	}
}

func TestVaultBrowserPublicationCleanupDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &bsPublicationRace{ctx: ctx, release: make(chan struct{})}
	stopWorker, workerStarted, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cleanupDone := make(chan struct{})
	cleanupStarted := false
	r.work.Add(1)
	go func() {
		defer r.work.Done()
		close(workerStarted)
		<-stopWorker // Deliberately ignore request cancellation and publication release.
		close(workerDone)
	}()
	t.Cleanup(func() {
		cancel()
		close(stopWorker)
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelJoin()
		work := []<-chan struct{}{workerDone}
		if cleanupStarted {
			work = append(work, cleanupDone)
		}
		for _, done := range work {
			select {
			case <-done:
			case <-joinCtx.Done():
				t.Error("cleanup guard's controlled work did not stop")
				return
			}
		}
	})
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	select {
	case <-workerStarted:
	case <-watchdog.C:
		t.Fatal("cleanup guard worker did not start")
	}
	expired := make(chan time.Time)
	close(expired) // An event-driven deadline; passing tests never wait for a timer.
	reported := make(chan string, 1)
	cleanupStarted = true
	go func() {
		defer close(cleanupDone)
		r.cleanup(cancel, expired, func(message string) { reported <- message })
	}()
	select {
	case message := <-reported:
		if message != "publication race cleanup join deadline" {
			t.Fatalf("unexpected cleanup failure: %s", message)
		}
	case <-watchdog.C:
		t.Error("cleanup ignored its expired deadline while work ignored cancellation")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Error("cleanup did not cancel the request")
	}
	select {
	case <-r.release:
	default:
		t.Error("cleanup did not release publication")
	}
	select {
	case <-workerDone:
		t.Error("cleanup deadline was not independent of unfinished work")
	default:
	}
}

func (r *bsPublicationRace) resume() { r.unpause.Do(func() { close(r.release) }) }

func (r *bsPublicationRace) start(fn func(context.Context) error) <-chan error {
	done := make(chan error, 1)
	r.work.Add(1)
	go func() {
		defer r.work.Done()
		done <- fn(r.ctx)
	}()
	return done
}

func (r *bsPublicationRace) join(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-r.ctx.Done():
		t.Fatal("publication race join deadline")
		return r.ctx.Err()
	}
}

// overlap accepts completion as well as contention so a missing lifecycle
// mutex reaches the final state assertions instead of an observer deadline.
// completed tells the caller whether this consumed the operation's result.
func (r *bsPublicationRace) overlap(t *testing.T, done <-chan error) (completed bool, err error) {
	t.Helper()
	select {
	case <-r.waiting:
		return false, nil
	case err := <-done:
		return true, err
	case <-r.ctx.Done():
		t.Fatal("publication race overlap deadline")
		return false, r.ctx.Err()
	}
}

func TestVaultBrowserRestorePublicationLockRaceLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	f := bsSeed(t, pool)
	browser := uuid.New()
	key := f.mint(t, browser)
	session := f.session(t, browser)
	cold, race := bsNewPublicationRace(t, f)
	restored := race.start(func(ctx context.Context) error { return cold.Restore(ctx, session, key) })
	bsWait(t, race.entered)
	locked := race.start(func(ctx context.Context) error { return cold.LockWithContext(ctx, f.id) })
	lockCompleted, lockErr := race.overlap(t, locked)
	race.resume()
	restoreErr := race.join(t, restored)
	if !lockCompleted {
		lockErr = race.join(t, locked)
	}
	if restoreErr != nil || lockErr != nil {
		t.Fatalf("restore/lock: %v / %v", restoreErr, lockErr)
	}
	if cold.Unlocked(f.id) {
		t.Fatal("restore published a cached key after durable lock")
	}
	if f.count(t) != 0 {
		t.Fatal("lock retained browser slots after restore publication")
	}
}

func TestVaultBrowserInteractiveUnlockLockRaceLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	for _, lockFirst := range []bool{false, true} {
		name := "unlock-first"
		if lockFirst {
			name = "lock-first"
		}
		t.Run(name, func(t *testing.T) {
			f := bsSeed(t, pool)
			browser := uuid.New()
			f.mint(t, browser)
			oldSnapshot := vault.Snapshot(f.user(t))
			v, race := bsNewPublicationRace(t, f)
			var minted bool
			unlock := func(ctx context.Context) error {
				key, err := v.UnlockAndMint(ctx, oldSnapshot, vlLoginPhrase, vault.InteractiveExisting,
					&vault.BrowserMintRequest{HTTPS: true, BrowserID: browser, SessionExpiresAt: time.Now().Add(time.Hour)})
				minted = key != nil
				key.Destroy()
				return err
			}
			lock := func(ctx context.Context) error { return v.LockWithContext(ctx, f.id) }
			first, second := unlock, lock
			if lockFirst {
				first, second = lock, unlock
			}
			firstDone := race.start(first)
			bsWait(t, race.entered)
			secondDone := race.start(second)
			secondCompleted, secondErr := race.overlap(t, secondDone)
			race.resume()
			firstErr := race.join(t, firstDone)
			if !secondCompleted {
				secondErr = race.join(t, secondDone)
			}
			unlockErr, lockErr := firstErr, secondErr
			if lockFirst {
				lockErr, unlockErr = firstErr, secondErr
			}
			if lockErr != nil {
				t.Fatalf("lock: %v", lockErr)
			}
			if lockFirst {
				if !errors.Is(unlockErr, vault.ErrStaleAuth) || minted {
					t.Fatalf("stale interactive snapshot minted after lock: minted=%t err=%v", minted, unlockErr)
				}
			} else if unlockErr != nil || !minted {
				t.Fatalf("unlock before lock: minted=%t err=%v", minted, unlockErr)
			}
			if v.Unlocked(f.id) {
				t.Fatal("interactive unlock left cache unlocked after lock")
			}
			if f.count(t) != 0 {
				t.Fatal("interactive unlock left browser slots after lock")
			}
		})
	}
}

func TestVaultBrowserAmbiguousCommitLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	f := bsSeed(t, pool)
	ctx := context.Background()
	ambiguous := bsWrapped(f, func(q vault.BrowserStore) vault.BrowserStore { return q }, true)
	key, err := ambiguous.UnlockAndMint(ctx, vault.Snapshot(f.user(t)), vlLoginPhrase, vault.InteractiveCreate, &vault.BrowserMintRequest{HTTPS: true, BrowserID: uuid.New(), SessionExpiresAt: time.Now().Add(time.Hour)})
	if !errors.Is(err, vault.ErrUncertainOutcome) || key != nil || ambiguous.Unlocked(f.id) {
		t.Fatalf("uncertain mint leaked publication: %v", err)
	}
	if f.count(t) != 1 {
		t.Fatal("injected committed mint not durable")
	}
	browser := uuid.New()
	valid := f.mint(t, browser)
	if err := ambiguous.Restore(ctx, f.session(t, browser), valid); !errors.Is(err, vault.ErrUncertainOutcome) || ambiguous.Unlocked(f.id) {
		t.Fatalf("uncertain restore published: %v", err)
	}
	if err := ambiguous.LockWithContext(ctx, f.id); !errors.Is(err, vault.ErrUncertainOutcome) {
		t.Fatal("uncertain revoke reported success")
	}
	if f.count(t) != 0 {
		t.Fatal("committed revoke left slots")
	}
}
