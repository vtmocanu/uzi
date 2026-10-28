package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/seed"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests call the real enrollment transactions, with no mocked query or
// synchronization hook. Only positively observed PostgreSQL lock waits advance
// the race; the ticker is readiness polling, not a scheduling delay.
type enrollmentAdmissionRun struct {
	ctx       context.Context
	holder    pgx.Tx
	contender pgx.Tx
	blocker   int32
	done      chan struct{}
	result    chan error
}

func newEnrollmentAdmissionRun(t *testing.T, pool *pgxpool.Pool, key int64) *enrollmentAdmissionRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e := &enrollmentAdmissionRun{ctx: ctx}
	t.Cleanup(func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		// Release every owned barrier before joining, including on Fatal paths.
		for _, tx := range []pgx.Tx{e.holder, e.contender} {
			if tx != nil {
				if err := tx.Rollback(cleanupCtx); err != nil && err != pgx.ErrTxClosed {
					t.Errorf("rollback enrollment barrier: %v", err)
				}
			}
		}
		if e.done != nil {
			<-e.done
		}
	})
	var err error
	e.holder, err = pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&e.blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := e.holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		t.Fatal(err)
	}
	return e
}

// Construct the closure on the test goroutine: fixture helpers can Fatal there,
// while the enrollment goroutine only sends its error and always closes done.
func enrollmentAdmissionCall(t *testing.T, pool *pgxpool.Pool, path, oldPolicy string) (string, func(context.Context) error) {
	t.Helper()
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, cfg: oidcLiveCfg(), settings: settings.New(q, time.Hour)}
	// Keep the old policy cached throughout the race. A handler regression to
	// the process cache must disagree with the committed authoritative policy.
	cached, err := h.settings.VaultPolicy(context.Background())
	if err != nil || cached != oldPolicy {
		t.Fatalf("prime old policy cache: policy=%q err=%v", cached, err)
	}
	email := uuid.NewString() + "@example.com"
	switch path {
	case "password":
		return email, func(ctx context.Context) error {
			req := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil).WithContext(ctx)
			_, err := h.createUserFirstAdmin(req, email, "admission-password-hash-fixture", pgtype.Text{})
			return err
		}
	case "oidc":
		subject := uuid.NewString()
		return email, func(ctx context.Context) error {
			req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil).WithContext(ctx)
			_, err := h.createOIDCUserFirstAdmin(req, email, pgtype.Text{}, "https://idp.example.com", subject, false, false)
			return err
		}
	case "seed":
		cfg := seedEnrollmentConfig()
		return cfg.SeedEmail, func(ctx context.Context) error {
			return seed.Admin(ctx, pool, cfg)
		}
	default:
		t.Fatalf("unknown enrollment path %q", path)
		return "", nil
	}
}

func (e *enrollmentAdmissionRun) start(call func(context.Context) error) {
	e.done = make(chan struct{})
	e.result = make(chan error, 1)
	go func() {
		defer close(e.done)
		e.result <- call(e.ctx)
	}()
}

// Match the exact production statement AND the backend owning our barrier.
// Return that PID so lock ownership assertions cannot pass on a sibling backend.
func (e *enrollmentAdmissionRun) waiter(t *testing.T, pool *pgxpool.Pool, statement string) int32 {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid int32
		err := pool.QueryRow(e.ctx, `SELECT pid FROM pg_stat_activity
 WHERE datname = current_database() AND wait_event_type = 'Lock'
   AND $1::int = ANY(pg_blocking_pids(pid)) AND query = $2`, e.blocker, statement).Scan(&pid)
		if err == nil {
			return pid
		}
		if err != pgx.ErrNoRows {
			t.Fatalf("inspect enrollment waiter: %v", err)
		}
		select {
		case <-e.done:
			t.Fatalf("enrollment completed before observed lock wait: %v", <-e.result)
		case <-e.ctx.Done():
			t.Fatalf("no enrollment waiter on backend %d for %q: %v", e.blocker, statement, e.ctx.Err())
		case <-ticker.C:
		}
	}
}

func (e *enrollmentAdmissionRun) committedUser(t *testing.T, pool *pgxpool.Pool, email, mode string) {
	t.Helper()
	select {
	case err := <-e.result:
		if err != nil {
			t.Fatalf("enrollment: %v", err)
		}
	case <-e.ctx.Done():
		t.Fatalf("enrollment did not finish: %v", e.ctx.Err())
	}
	<-e.done
	// Read through the public store interface outside the enrollment TX: the
	// account and its policy choice must be visible after a completed commit.
	user, err := store.New(pool).GetUserByEmail(e.ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if user.VaultMode != mode || user.VaultEpoch != 0 || !user.IsAdmin {
		t.Fatalf("committed enrollment mode=%q epoch=%d admin=%v; want mode=%q epoch=0 admin=true",
			user.VaultMode, user.VaultEpoch, user.IsAdmin, mode)
	}
}

func TestVaultEnrollmentAdmissionCommittedPolicyLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	for _, path := range []string{"password", "oidc", "seed"} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name, before, after, mode string
			}{
				{"required_to_optional", "required", "optional", store.VaultModeUnprotected},
				{"optional_to_required", "optional", "required", store.VaultModeProtected},
				{"optional_to_missing", "optional", "", store.VaultModeProtected},
				{"optional_to_invalid", "optional", "invalid", store.VaultModeProtected},
			} {
				t.Run(tc.name, func(t *testing.T) {
					enrollmentPolicy(t, pool, tc.before)
					email, call := enrollmentAdmissionCall(t, pool, path, tc.before)
					e := newEnrollmentAdmissionRun(t, pool, store.SettingsMutationLockKey)
					// Deliberately bypass settings validation for malformed persisted
					// policy, exactly as the missing/invalid fail-closed contract needs.
					if tc.after == "" {
						if _, err := e.holder.Exec(e.ctx, "DELETE FROM app_settings WHERE key='vault_policy'"); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := e.holder.Exec(e.ctx, "UPDATE app_settings SET value=$1 WHERE key='vault_policy'", tc.after); err != nil {
							t.Fatal(err)
						}
					}
					e.start(call)
					e.waiter(t, pool, "SELECT pg_advisory_xact_lock_shared($1)")
					if err := e.holder.Commit(e.ctx); err != nil {
						t.Fatal(err)
					}
					// A separate post-admission READ COMMITTED statement sees this
					// commit; a snapshot acquired in the lock statement sees before.
					e.committedUser(t, pool, email, tc.mode)
				})
			}
		})
	}
}

func TestVaultEnrollmentAdmissionBeforeRegistrationHeldThroughCommitLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	for _, path := range []string{"password", "oidc", "seed"} {
		t.Run(path, func(t *testing.T) {
			enrollmentPolicy(t, pool, "optional")
			email, call := enrollmentAdmissionCall(t, pool, path, "optional")
			e := newEnrollmentAdmissionRun(t, pool, store.RegistrationLockKey)
			e.start(call)
			pid := e.waiter(t, pool, "SELECT pg_advisory_xact_lock($1)")

			key := uint64(store.SettingsMutationLockKey)
			var granted bool
			err := pool.QueryRow(e.ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory'
   AND mode='ShareLock' AND granted
   AND classid::bigint=$2 AND objid::bigint=$3 AND objsubid=1
   AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
)`, pid, int64(key>>32), int64(key&0xffffffff)).Scan(&granted)
			if err != nil {
				t.Fatal(err)
			}
			if !granted {
				t.Fatalf("registration waiter backend %d lacks granted shared settings admission", pid)
			}

			e.contender, err = pool.BeginTx(e.ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var acquired bool
			if err := e.contender.QueryRow(e.ctx, "SELECT pg_try_advisory_xact_lock($1)", store.SettingsMutationLockKey).Scan(&acquired); err != nil {
				t.Fatal(err)
			}
			if acquired {
				t.Fatal("exclusive settings contender entered during enrollment transaction")
			}
			if err := e.holder.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			e.committedUser(t, pool, email, store.VaultModeUnprotected)
			if err := e.contender.QueryRow(e.ctx, "SELECT pg_try_advisory_xact_lock($1)", store.SettingsMutationLockKey).Scan(&acquired); err != nil {
				t.Fatal(err)
			}
			if !acquired {
				t.Fatal("committed enrollment retained shared settings admission")
			}
		})
	}
}
