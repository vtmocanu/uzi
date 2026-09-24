package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_promote_livedb_test.go pins PRD #1590 M3 (D3) against a real Postgres: the
// promote_codex_account_available sweeper pass, its run -> alias -> account lock order with
// NOWAIT on the latter two, the unchanged release predicate, and both timer promoters leaving the
// cause alone. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh.

// heldCodexFix is newCodexClaimFix's queued run parked on codex_account_unavailable (its account
// quarantined) by one M2 park page that examines only this run, so the seed does not depend on
// the promotion pass under test. The returned service's Sweep covers every queued and held Codex
// run, so the fixture is examined on the first tick whatever other tests left behind.
func heldCodexFix(t *testing.T, env codexTestEnv) (*codexClaimFix, *Service) {
	t.Helper()
	fx := newCodexClaimFix(t, env, false)
	fx.setAccount(t, "coord_state = 'quarantined'")
	return fx, parkHeldCodexFix(t, env, fx)
}

// parkHeldCodexFix parks fx's queued run (its account already quarantined) with one M2 park page
// that examines only this run, and returns the Sweep-capable service.
func parkHeldCodexFix(t *testing.T, env codexTestEnv, fx *codexClaimFix) *Service {
	t.Helper()
	svc := gateSweepService(env, fx.svc)
	svc.codexPark.after, svc.codexPark.capOverride = uuidPredecessor(fx.runID), 1
	if n, err := svc.parkCodexAccountUnavailable(env.ctx); err != nil || n != 1 {
		t.Fatalf("park = (%d, %v), want (1, nil)", n, err)
	}
	svc.codexPark.capOverride = 1 << 20
	svc.codexPromote.capOverride = 1 << 20
	if r := mustRun(t, env, fx.runID); r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseCodexAccountUnavailable {
		t.Fatalf("seed: status=%s cause=%v, want a codex_account_unavailable hold", r.Status, r.RecoveryWaitCause)
	}
	return svc
}

// promoteOnly runs one promotion page that examines exactly this run (a one-row page starting at
// it), so no other held run in the shared database is touched.
func promoteOnly(t *testing.T, svc *Service, runID uuid.UUID) (int64, error) {
	t.Helper()
	svc.codexPromote.after = uuidPredecessor(runID)
	svc.codexPromote.capOverride = 1
	// Bounded, so a promoter that waited on a lock (the deadlock regression) fails promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return svc.promoteCodexAccountAvailable(ctx)
}

// sealedLogin seals a fresh login blob the way the reconciler's master path does.
func sealedLogin(t *testing.T, env codexTestEnv, access string) []byte {
	t.Helper()
	raw, err := json.Marshal(codexLoginBlob{AccessToken: access, RefreshToken: codexToken("refresh")}) //nolint:gosec // G117: synthetic fixture
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := env.box.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// accountGeneration reads the account's current generation.
func accountGeneration(t *testing.T, env codexTestEnv, accountID uuid.UUID) int64 {
	t.Helper()
	var gen int64
	if err := env.pool.QueryRow(env.ctx, `SELECT generation FROM codex_provider_account WHERE id = $1`, accountID).Scan(&gen); err != nil {
		t.Fatal(err)
	}
	return gen
}

// relogin runs RefreshCodexAccountLogin's quarantined arm on q (the reconciler's restore after a
// verified re-login) with a new access token, and returns the new generation.
func relogin(t *testing.T, env codexTestEnv, q *store.Queries, fx *codexClaimFix, access string) int64 {
	t.Helper()
	from := accountGeneration(t, env, fx.accountID)
	n, err := q.RefreshCodexAccountLogin(env.ctx, store.RefreshCodexAccountLoginParams{
		Sealed: sealedLogin(t, env, access), SealedWith: store.SealedWithMaster,
		ID: fx.accountID, UserID: fx.userID, FromGeneration: from,
	})
	if err != nil || n != 1 {
		t.Fatalf("RefreshCodexAccountLogin = (%d, %v), want 1 row", n, err)
	}
	return from + 1
}

// assertPromoted checks the promotion field set against the held row.
func assertPromoted(t *testing.T, env codexTestEnv, runID uuid.UUID, held store.Run) {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "queued" {
		t.Fatalf("status = %s, want queued", r.Status)
	}
	if r.StartedAt.Valid || r.BudgetPausedSeconds != 0 {
		t.Fatalf("started_at=%v paused=%d, want a fresh wall", r.StartedAt, r.BudgetPausedSeconds)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch != held.CodexClaimEpoch+1 {
		t.Fatalf("hash=%x epoch=%d (held %d), want no capability and epoch+1", r.CodexCapHash, r.CodexClaimEpoch, held.CodexClaimEpoch)
	}
	if r.Health != "ok" || r.HealthReason.Valid || r.HealthSince.Valid {
		t.Fatalf("health=%s/%v/%v, want ok", r.Health, r.HealthReason, r.HealthSince)
	}
	if r.FailOrigin.Valid || r.ClaimGeneration != held.ClaimGeneration || r.WorkerID != held.WorkerID {
		t.Fatalf("origin=%v gen=%d worker=%v, want no failure and generation/affinity kept", r.FailOrigin, r.ClaimGeneration, r.WorkerID)
	}
}

// assertStillHeld checks the run is untouched since held.
func assertStillHeld(t *testing.T, env codexTestEnv, runID uuid.UUID, held store.Run) {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseCodexAccountUnavailable ||
		r.CodexClaimEpoch != held.CodexClaimEpoch || !r.UpdatedAt.Time.Equal(held.UpdatedAt.Time) {
		t.Fatalf("status=%s cause=%v epoch=%d (held %d) updated %v (held %v), want the hold untouched",
			r.Status, r.RecoveryWaitCause, r.CodexClaimEpoch, held.CodexClaimEpoch, r.UpdatedAt.Time, held.UpdatedAt.Time)
	}
}

// TestCodexAccountPromoteHealthyAccountLiveDB: a held run whose account is re-logged in (the
// quarantine cleared, a new login installed) reaches queued on the next Sweep under the unchanged
// predicate, with a fresh wall, a revoked capability, epoch+1 and health ok, and the next claim
// delivers the CURRENT credential (the re-login's token and generation), not the old one.
func TestCodexAccountPromoteHealthyAccountLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	// Dirty the held row so each promotion column is observable.
	env.exec(`UPDATE runs SET codex_cap_hash = $2, started_at = now(), budget_paused_seconds = 42,
		health = 'stalled', health_reason = 'x', health_since = now() WHERE id = $1`, fx.runID, bytes.Repeat([]byte{0xcd}, 32))
	held := mustRun(t, env, fx.runID)
	access := codexToken("access-new")
	gen := relogin(t, env, env.q, fx, access)

	res, err := svc.Sweep(env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.CodexAccountPromoted < 1 {
		t.Fatalf("CodexAccountPromoted = %d, want >= 1", res.CodexAccountPromoted)
	}
	assertPromoted(t, env, fx.runID, held)

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil {
		t.Fatalf("Claim = (%v, %v), want a Codex payload", payload != nil, err)
	}
	c := payload.Secrets.Codex
	if c.AccessToken != access || c.Generation == nil || *c.Generation != gen {
		t.Fatalf("claim delivered token match=%v generation=%v, want the re-login's token at generation %d",
			c.AccessToken == access, c.Generation, gen)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "claimed" || r.ClaimGeneration != held.ClaimGeneration+1 {
		t.Fatalf("status=%s gen=%d, want claimed at generation %d", r.Status, r.ClaimGeneration, held.ClaimGeneration+1)
	}
}

// TestCodexAccountPromoteStaysHeldLiveDB: a still-quarantined account, a re-login in flight
// (alias PATCHed to staging, link cleared), and a same-identity relink (material ahead; D5
// re-admission is M4) all fail the predicate, so the run stays held, untouched, across ticks.
// Each tick runs a one-row promotion page and then a full Sweep whose promotion page starts at
// the fixture run; an afterList hook counts the fixture's examinations, so both legs are proven
// to have decided this run rather than passed it by.
func TestCodexAccountPromoteStaysHeldLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, fx *codexClaimFix)
	}{
		{"still quarantined", func(*testing.T, *codexClaimFix) {}},
		{"re-login in flight", func(t *testing.T, fx *codexClaimFix) {
			n, err := fx.env.q.BumpCodexMaterialRevision(fx.env.ctx, store.BumpCodexMaterialRevisionParams{
				UserSecretID: fx.aliasID, UserID: fx.userID, Status: "staging",
			})
			if err != nil || n != 1 {
				t.Fatalf("BumpCodexMaterialRevision = (%d, %v)", n, err)
			}
			fx.setAccount(t, "coord_state = 'idle'")
		}},
		{"same-identity relink awaiting re-admission", func(t *testing.T, fx *codexClaimFix) {
			fx.env.exec(`UPDATE codex_credential_state SET material_revision = material_revision + 1
				WHERE user_secret_id = $1`, fx.aliasID)
			fx.setAccount(t, "coord_state = 'idle'")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			tc.mutate(t, fx)
			held := mustRun(t, env, fx.runID)
			examined := 0
			svc.codexPromoteHooks = &codexPromoteTestHooks{afterList: func(_ context.Context, id uuid.UUID) {
				if id == fx.runID {
					examined++
				}
			}}
			for tick := 0; tick < 3; tick++ {
				if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 0 {
					t.Fatalf("tick %d: promote = (%d, %v), want (0, nil)", tick, n, err)
				}
				assertStillHeld(t, env, fx.runID, held)
				// The Sweep's page starts at this run (other held runs in the shared database
				// may follow it), so the tick decides the fixture too.
				svc.codexPromote.after = uuidPredecessor(fx.runID)
				svc.codexPromote.capOverride = 1 << 20
				if _, err := svc.Sweep(env.ctx); err != nil {
					t.Fatalf("tick %d Sweep: %v", tick, err)
				}
				assertStillHeld(t, env, fx.runID, held)
				if examined != 2*(tick+1) {
					t.Fatalf("tick %d: fixture examined %d times, want %d (once by the page, once by Sweep)",
						tick, examined, 2*(tick+1))
				}
			}
		})
	}
}

// TestCodexAccountPromoteRelinkRaceLiveDB: the account is healthy when the pass lists the run,
// then the alias is relinked to a DIFFERENT identity, or its material is bumped by a re-login.
// The race is injected in afterList, which runs BEFORE the run's transaction opens, so the write
// has committed by the time the promoter locks anything: this pins only that the in-transaction
// re-read (under the run, alias and account locks) sees a change made after the page was listed,
// and the run is not promoted. It does not exercise a writer racing the open transaction; that
// is TestCodexAccountPromoteRelinkWaitsOnAliasLockLiveDB. A control run without the race is
// promoted, so the refusal is the race's.
func TestCodexAccountPromoteRelinkRaceLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		race func(t *testing.T, fx *codexClaimFix)
	}{
		{"none (control)", nil},
		{"relink to a different identity", func(t *testing.T, fx *codexClaimFix) {
			other := fx.env.seedLinkedSubscription(t, fx.userID, "codex-other-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
			fx.env.exec(`UPDATE codex_credential_state s SET provider_account_id = o.provider_account_id
				FROM codex_credential_state o WHERE s.user_secret_id = $1 AND o.user_secret_id = $2`, fx.aliasID, other)
		}},
		{"material bump", func(t *testing.T, fx *codexClaimFix) {
			n, err := fx.env.q.BumpCodexMaterialRevision(fx.env.ctx, store.BumpCodexMaterialRevisionParams{
				UserSecretID: fx.aliasID, UserID: fx.userID, Status: "staging",
			})
			if err != nil || n != 1 {
				t.Fatalf("BumpCodexMaterialRevision = (%d, %v)", n, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			fx.setAccount(t, "coord_state = 'idle'")
			held := mustRun(t, env, fx.runID)
			raced := 0
			svc.codexPromoteHooks = &codexPromoteTestHooks{afterList: func(_ context.Context, id uuid.UUID) {
				if id == fx.runID && tc.race != nil {
					raced++
					tc.race(t, fx)
				}
			}}
			n, err := promoteOnly(t, svc, fx.runID)
			if err != nil {
				t.Fatalf("promote: %v", err)
			}
			if tc.race == nil {
				if n != 1 {
					t.Fatalf("control promoted %d, want 1", n)
				}
				assertPromoted(t, env, fx.runID, held)
				return
			}
			if raced != 1 || n != 0 {
				t.Fatalf("raced=%d promoted=%d, want the race injected once and nothing promoted", raced, n)
			}
			assertStillHeld(t, env, fx.runID, held)
		})
	}
}

// TestCodexAccountPromoteCauseChangedAfterListLiveDB: a run whose cause changes between the list
// and its transaction (here to a timer cause) is not promoted by this pass; the run-lock and the
// UPDATE fence on the cause, not only the list.
func TestCodexAccountPromoteCauseChangedAfterListLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	fx.setAccount(t, "coord_state = 'idle'")
	svc.codexPromoteHooks = &codexPromoteTestHooks{afterList: func(_ context.Context, id uuid.UUID) {
		if id == fx.runID {
			env.exec(`UPDATE runs SET recovery_wait_cause = 'provider_outage' WHERE id = $1`, id)
		}
	}}
	if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 0 {
		t.Fatalf("promote = (%d, %v), want (0, nil)", n, err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "recovery_wait" || r.RecoveryWaitCause.String != "provider_outage" {
		t.Fatalf("status=%s cause=%v, want the timer hold untouched", r.Status, r.RecoveryWaitCause)
	}
}

// waitForLockWait polls until backend pid is blocked on a heavyweight lock.
func waitForLockWait(t *testing.T, env codexTestEnv, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ev *string
		if err := env.pool.QueryRow(env.ctx, `SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&ev); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if ev != nil && *ev == "Lock" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d never blocked on the alias lock", pid)
}

// TestCodexAccountPromoteDeadlockInterleavingLiveDB is D3's deadlock-avoidance regression. A
// re-login transaction restores the account (RefreshCodexAccountLogin, holding the account row)
// and then relinks the alias (LinkCodexCredentialState), the reconciler's account-then-alias
// order. The promoter, holding the run and the alias FOR SHARE, requests the account while the
// re-login is blocked on the alias. The promoter's account NOWAIT fails with 55P03, it rolls back
// with the run still held (no premature promotion), the re-login's alias write proceeds and
// commits, and the next tick promotes. All within a bounded time, with no deadlock. With a
// blocking account lock the two would deadlock and Postgres would abort one with 40P01.
func TestCodexAccountPromoteDeadlockInterleavingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	held := mustRun(t, env, fx.runID)
	var material int64
	if err := env.pool.QueryRow(env.ctx, `SELECT material_revision FROM codex_credential_state WHERE user_secret_id = $1`,
		fx.aliasID).Scan(&material); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	defer cancel()

	reloginTx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloginTx.Rollback(env.ctx) }()
	var pid int32
	if err := reloginTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	relogin(t, env, store.New(reloginTx), fx, codexToken("access-new"))

	linkDone := make(chan error, 1)
	svc.codexPromoteHooks = &codexPromoteTestHooks{afterAliasLock: func(_ context.Context, id uuid.UUID) {
		if id != fx.runID {
			return
		}
		go func() {
			n, err := store.New(reloginTx).LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
				ProviderAccountID: pgconv.UUID(fx.accountID), UserSecretID: fx.aliasID, UserID: fx.userID, MaterialRevision: material,
			})
			if err == nil && n != 1 {
				err = errors.New("link matched no row")
			}
			linkDone <- err
		}()
		waitForLockWait(t, env, pid) // the re-login now waits on the promoter's alias lock
	}}

	n, err := promoteOnly(t, svc, fx.runID)
	if err != nil || n != 0 {
		t.Fatalf("contended tick: promote = (%d, %v), want (0, nil): a 55P03 leaves the run held", n, err)
	}
	assertStillHeld(t, env, fx.runID, held)
	select {
	case err := <-linkDone:
		if err != nil {
			t.Fatalf("re-login alias link: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("re-login alias link never completed")
	}
	if err := reloginTx.Commit(ctx); err != nil {
		t.Fatalf("re-login commit: %v", err)
	}
	svc.codexPromoteHooks = nil
	if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 1 {
		t.Fatalf("next tick: promote = (%d, %v), want (1, nil)", n, err)
	}
	assertPromoted(t, env, fx.runID, held)
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("interleaving took %v, want bounded", el)
	}
}

// TestCodexAccountHoldCancelLiveDB: the owner's cancel ends a held run server-side, and a later
// promotion pass leaves the cancelled run alone.
func TestCodexAccountHoldCancelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	if _, err := svc.SubmitInput(env.ctx, fx.userID, fx.runID, "cancel", "", nil); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", r.Status)
	}
	fx.setAccount(t, "coord_state = 'idle'")
	// The cancelled run is no longer listed, so the page may hold another test's held run: only
	// the error and this run's status are asserted, not the count.
	if _, err := promoteOnly(t, svc, fx.runID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "cancelled" {
		t.Fatalf("status after promote = %s, want cancelled", r.Status)
	}
}

// TestTimerPromotersSkipCodexAccountHoldLiveDB: neither timer promoter touches the cause, even
// with a recovery_retry_not_before long past (both writers write NULL; this forces the stamp so
// only the cause fence can keep the run held).
func TestTimerPromotersSkipCodexAccountHoldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, _ := heldCodexFix(t, env)
	fx.setAccount(t, "coord_state = 'idle'")
	env.exec(`UPDATE runs SET recovery_retry_not_before = now() - interval '1 hour' WHERE id = $1`, fx.runID)
	held := mustRun(t, env, fx.runID)
	rows, err := env.q.PromoteRecoveryWaitRuns(env.ctx, pgconv.Time(time.Now()))
	if err != nil {
		t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
	}
	for _, r := range rows {
		if r.ID == fx.runID {
			t.Fatal("the timer promoter promoted a codex_account_unavailable hold")
		}
	}
	n, err := env.q.PromoteRecoveryWaitRunNow(env.ctx, store.PromoteRecoveryWaitRunNowParams{ID: fx.runID, UserID: fx.userID})
	if err != nil || n != 0 {
		t.Fatalf("PromoteRecoveryWaitRunNow = (%d, %v), want (0, nil)", n, err)
	}
	assertStillHeld(t, env, fx.runID, held)
}

// TestCodexAccountPromoteAccountLockNowaitLiveDB pins that the promoter really takes the account
// FOR SHARE NOWAIT lock. The account is healthy and the alias free, so the predicate would pass;
// only another transaction's FOR UPDATE on the account row stands between the run and queued.
// The contended tick must pass the alias lock (afterAliasLock fires), come back promptly with
// nothing promoted (55P03, not a wait for the row), and the next tick, after that transaction
// ends, promotes. Without the account lock the contended tick promotes on the unlocked re-read;
// with a blocking lock it waits for the row until promoteOnly's deadline and reports the error.
func TestCodexAccountPromoteAccountLockNowaitLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	fx.setAccount(t, "coord_state = 'idle'")
	held := mustRun(t, env, fx.runID)

	lockTx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(env.ctx) }()
	if _, err := lockTx.Exec(env.ctx, `SELECT 1 FROM codex_provider_account WHERE id = $1 FOR UPDATE`, fx.accountID); err != nil {
		t.Fatalf("lock account: %v", err)
	}
	aliasLocked := 0
	svc.codexPromoteHooks = &codexPromoteTestHooks{afterAliasLock: func(_ context.Context, id uuid.UUID) {
		if id == fx.runID {
			aliasLocked++
		}
	}}
	start := time.Now()
	n, err := promoteOnly(t, svc, fx.runID)
	if err != nil || n != 0 {
		t.Fatalf("contended tick: promote = (%d, %v), want (0, nil): the account lock is held", n, err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("contended tick took %v, want a prompt NOWAIT refusal", el)
	}
	if aliasLocked != 1 {
		t.Fatalf("alias lock reached %d times, want 1: the hold must come from the account lock", aliasLocked)
	}
	assertStillHeld(t, env, fx.runID, held)

	if err := lockTx.Rollback(env.ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 1 {
		t.Fatalf("next tick: promote = (%d, %v), want (1, nil)", n, err)
	}
	assertPromoted(t, env, fx.runID, held)
}

// TestCodexAccountPromoteRelinkWaitsOnAliasLockLiveDB is the other half of the relink race: a
// LinkCodexCredentialState that starts while the promoter's transaction holds the alias FOR SHARE
// (here, relinking the alias to a DIFFERENT identity) blocks on that row until the promoter's
// transaction ends. The promoter decides on the state it locked (the frozen identity, healthy),
// promotes and commits; only then does the relink land. The next claim re-runs the full
// predicate against the relinked alias, so the promotion never releases a credential by itself.
func TestCodexAccountPromoteRelinkWaitsOnAliasLockLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, svc := heldCodexFix(t, env)
	fx.setAccount(t, "coord_state = 'idle'")
	held := mustRun(t, env, fx.runID)
	other := env.seedLinkedSubscription(t, fx.userID, "codex-other-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	var otherAccount uuid.UUID
	var material int64
	if err := env.pool.QueryRow(env.ctx, `SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1`,
		other).Scan(&otherAccount); err != nil {
		t.Fatal(err)
	}
	if err := env.pool.QueryRow(env.ctx, `SELECT material_revision FROM codex_credential_state WHERE user_secret_id = $1`,
		fx.aliasID).Scan(&material); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	defer cancel()
	linkTx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = linkTx.Rollback(env.ctx) }()
	var pid int32
	if err := linkTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	linkDone := make(chan error, 1)
	blocked := false
	svc.codexPromoteHooks = &codexPromoteTestHooks{afterAliasLock: func(_ context.Context, id uuid.UUID) {
		if id != fx.runID {
			return
		}
		go func() {
			n, err := store.New(linkTx).LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
				ProviderAccountID: pgconv.UUID(otherAccount), UserSecretID: fx.aliasID, UserID: fx.userID, MaterialRevision: material,
			})
			if err == nil && n != 1 {
				err = errors.New("link matched no row")
			}
			linkDone <- err
		}()
		waitForLockWait(t, env, pid) // the relink now waits on the promoter's alias lock
		select {
		case <-linkDone:
			t.Error("the relink completed while the promoter held the alias")
		default:
			blocked = true
		}
	}}

	n, err := promoteOnly(t, svc, fx.runID)
	if err != nil || n != 1 || !blocked {
		t.Fatalf("promote = (%d, %v), relink blocked %v; want (1, nil) with the relink blocked", n, err, blocked)
	}
	assertPromoted(t, env, fx.runID, held)
	select {
	case err := <-linkDone:
		if err != nil {
			t.Fatalf("relink after the promoter's commit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the relink never completed after the promoter's transaction ended")
	}
	if err := linkTx.Commit(ctx); err != nil {
		t.Fatalf("relink commit: %v", err)
	}
	var linked uuid.UUID
	if err := env.pool.QueryRow(env.ctx, `SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1`,
		fx.aliasID).Scan(&linked); err != nil || linked != otherAccount {
		t.Fatalf("alias links %s (%v), want the relinked account %s", linked, err, otherAccount)
	}
}

// TestCodexAccountPromoteSurvivorRecoveryLiveDB is the PRD's survivor leg: a held run whose account
// was quarantined with verified recovery material at its current generation (the protect-before-
// park path of a failed refresh). One Sweep tick runs the survivor pass (PromoteCodexRecovery
// installs the material, generation+1, quarantine cleared, credential_revision unchanged) and
// then, in the same tick, the account promotion pass, so the run is queued after that one tick.
// The next claim delivers the recovered login's access token at the new generation, and none of
// the refresh material or the pre-quarantine token.
func TestCodexAccountPromoteSurvivorRecoveryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	gen := accountGeneration(t, env, fx.accountID)
	acct := env.mustAccount(t, fx.userID, fx.accountID)

	// Protect recovery material under a live lease; SetCodexRecoverySlot quarantines the account.
	recAccess, recRefresh := codexToken("recovery-access"), codexToken("recovery-refresh")
	op := uuid.New()
	if n, err := env.q.AcquireCodexRefreshLease(env.ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		ID: fx.accountID, UserID: fx.userID, FromGeneration: gen,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d, %v), want (1, nil)", n, err)
	}
	raw, err := json.Marshal(codexLoginBlob{AccessToken: recAccess, RefreshToken: recRefresh}) //nolint:gosec // G117: synthetic recovery fixture
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := env.box.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := env.q.SetCodexRecoverySlot(env.ctx, store.SetCodexRecoverySlotParams{
		Sealed: sealed, Gen: gen, ID: fx.accountID, UserID: fx.userID,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot = (%d, %v), want (1, nil)", n, err)
	}
	svc := parkHeldCodexFix(t, env, fx)
	held := mustRun(t, env, fx.runID)

	// The recovery blob verifies as this account; every other account's leftover recovery
	// material in the shared database defers, so this tick changes no foreign account.
	svc.SetCodexRefresh(&fakeRefreshClient{
		identityByToken: map[string]codexauth.Identity{
			recAccess: {ProviderUserID: acct.ProviderUserID, WorkspaceAccountID: acct.WorkspaceAccountID},
		},
		discoverDefaultErr: codexauth.ErrIdentityIncomplete,
	})
	res, err := svc.Sweep(env.ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	after := env.mustAccount(t, fx.userID, fx.accountID)
	if after.CoordState != "idle" || after.Generation != gen+1 || len(after.RecoverySealed) != 0 ||
		after.CredentialRevision != acct.CredentialRevision {
		t.Fatalf("account coord=%s gen=%d recovery=%d rev=%d, want the recovery promoted (idle, gen %d, slot empty, rev %d)",
			after.CoordState, after.Generation, len(after.RecoverySealed), after.CredentialRevision, gen+1, acct.CredentialRevision)
	}
	if res.CodexRefreshRecovered < 1 || res.CodexAccountPromoted < 1 {
		t.Fatalf("recovered=%d promoted=%d, want both >= 1 in the one tick", res.CodexRefreshRecovered, res.CodexAccountPromoted)
	}
	assertPromoted(t, env, fx.runID, held)

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil {
		t.Fatalf("Claim = (%v, %v), want a Codex payload", payload != nil, err)
	}
	c := payload.Secrets.Codex
	if c.AccessToken != recAccess || c.Generation == nil || *c.Generation != gen+1 {
		t.Fatalf("claim token match=%v generation=%v, want the recovered token at generation %d",
			c.AccessToken == recAccess, c.Generation, gen+1)
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"recovery refresh token": recRefresh, "pre-quarantine access token": fx.access} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("the claim payload carries the %s", name)
		}
	}
}
