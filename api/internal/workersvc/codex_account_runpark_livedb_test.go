package workersvc

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Observe real transaction attempts and NOWAIT failures without replacing SQL.
type accountParkObserver struct {
	pool                *pgxpool.Pool
	begins, contentions int
	afterAlias          func(context.Context) error
}

func (o *accountParkObserver) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	o.begins++
	return &accountParkObservedTx{Tx: tx, observer: o}, nil
}

type accountParkObservedTx struct {
	pgx.Tx
	observer *accountParkObserver
}

func (tx *accountParkObservedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return accountParkObservedRow{Row: tx.Tx.QueryRow(ctx, sql, args...), observer: tx.observer, ctx: ctx,
		alias: strings.HasPrefix(sql, "-- name: LockCodexAliasForShareNowait ")}
}

type accountParkObservedRow struct {
	pgx.Row
	observer *accountParkObserver
	ctx      context.Context
	alias    bool
}

func (r accountParkObservedRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	if isLockNotAvailable(err) {
		r.observer.contentions++
	}
	if err == nil && r.alias && r.observer.afterAlias != nil {
		hook := r.observer.afterAlias
		r.observer.afterAlias = nil
		return hook(r.ctx)
	}
	return err
}

func runningAccountParkFixture(t *testing.T, env codexTestEnv) (*codexClaimFix, store.Worker, string) {
	t.Helper()
	fx := newCodexClaimFix(t, env, true)
	env.exec(`UPDATE runs SET status='running', health='stalled', health_reason='fixture',
  health_since=now(), recovery_wait_count=3, forge_park_count=2, disk_park_count=2
  WHERE id=$1`, fx.runID)
	env.exec(`UPDATE workers SET last_heartbeat_at=now(), protocol_capabilities=$2 WHERE id=$1`,
		fx.workerA, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1, capability.CodexAccountParkV1})
	cap := env.mintCap(t, fx.runID, fx.workerA)
	return fx, wkrRow(t, env, fx.workerA), cap
}

func reportAccountPark(t *testing.T, fx *codexClaimFix, w store.Worker) (store.Run, bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(fx.env.ctx, 2*time.Second)
	defer cancel()
	return fx.svc.SetState(ctx, w, fx.runID, StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseCodexAccountUnavailable),
		ClaimGeneration: i64Ptr(1), CheckpointContainsLatest: boolPtr(true), SessionID: strPtr("park-session"),
	})
}

func assertAccountParkCustody(t *testing.T, fx *codexClaimFix, before store.Run, typed bool) store.Run {
	t.Helper()
	r := mustRun(t, fx.env, fx.runID)
	if typed {
		fx.assertParked(t, before, fx.workerA)
	}
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.Valid != typed || r.RecoveryRetryNotBefore.Valid == typed ||
		r.FailOrigin.Valid || r.FailureReason.Valid || r.WorkerID != before.WorkerID ||
		r.ForgeParkCount != before.ForgeParkCount || r.DiskParkCount != before.DiskParkCount ||
		r.ClaimGeneration != before.ClaimGeneration || r.SessionID.String != "park-session" ||
		!r.CheckpointContainsLatest.Valid || !r.CheckpointContainsLatest.Bool {
		t.Fatalf("park fields incorrect: status=%s cause=%v retry=%v checkpoint=%v", r.Status, r.RecoveryWaitCause, r.RecoveryRetryNotBefore, r.CheckpointContainsLatest)
	}
	wantCount := before.RecoveryWaitCount
	if !typed {
		wantCount++
	}
	if r.RecoveryWaitCount != wantCount {
		t.Fatalf("count=%d want=%d", r.RecoveryWaitCount, wantCount)
	}
	assertClaimRecoveryHold(t, fx.env, fx.holdA, "open", false)
	return r
}

// A4/A9: a real running report revokes the capability while retaining source custody.
func TestRunningCodexAccountParkObservedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, cap := runningAccountParkFixture(t, env)
	setCoordState(t, env, fx.accountID, "quarantined")
	before := mustRun(t, env, fx.runID)
	_, applied, err := reportAccountPark(t, fx, w)
	if err != nil || !applied {
		t.Fatalf("report: applied=%v err=%v", applied, err)
	}
	after := assertAccountParkCustody(t, fx, before, true)
	if after.CodexClaimEpoch != before.CodexClaimEpoch+1 {
		t.Fatal("epoch not advanced once")
	}
	fake := &fakeRefreshClient{}
	fx.svc.codexRefresh = fake
	if _, err := fx.svc.CoordinatedCodexRefresh(env.ctx, w, fx.runID, cap, uuid.New(), 0); !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("refresh after park: %v", err)
	}
	if _, err := fx.svc.ReleaseCodexCredential(env.ctx, w, fx.runID, cap); !errors.Is(err, ErrCodexCapabilityEpoch) {
		t.Fatalf("release after park: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("provider calls=%d", fake.calls)
	}
	_, applied, err = reportAccountPark(t, fx, w)
	if err != nil || applied || !reflect.DeepEqual(after, mustRun(t, env, fx.runID)) {
		t.Fatalf("duplicate mutated: applied=%v err=%v", applied, err)
	}
}

// A6a/A10: server classification, never the worker's word, selects typed versus NULL.
func TestRunningCodexAccountParkFallbacksLiveDB(t *testing.T) {
	for _, state := range []string{"idle", "different-identity", "chat"} {
		t.Run(state, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, w, _ := runningAccountParkFixture(t, env)
			if state == "different-identity" {
				other := env.seedLinkedSubscription(t, fx.userID, "other-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
				alias, err := env.q.GetCodexCredentialState(env.ctx, store.GetCodexCredentialStateParams{UserSecretID: other, UserID: fx.userID})
				if err != nil {
					t.Fatal(err)
				}
				env.exec("UPDATE codex_credential_state SET provider_account_id=$2 WHERE user_secret_id=$1", fx.aliasID, alias.ProviderAccountID)
			}
			if state == "chat" {
				setCoordState(t, env, fx.accountID, "quarantined")
				env.exec("UPDATE runs SET kind='chat', repo_id=NULL, issue_iid=NULL, branch=NULL WHERE id=$1", fx.runID)
			}
			before := mustRun(t, env, fx.runID)
			start := time.Now()
			_, applied, err := reportAccountPark(t, fx, w)
			if err != nil || !applied || time.Since(start) >= 2*time.Second {
				t.Fatalf("fallback: applied=%v err=%v duration=%s", applied, err, time.Since(start))
			}
			assertAccountParkCustody(t, fx, before, false)
		})
	}
}

func TestRunningCodexAccountParkFencesLiveDB(t *testing.T) {
	for _, fence := range []string{"stale", "released", "missing", "nonrunning", "stop"} {
		t.Run(fence, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, w, _ := runningAccountParkFixture(t, env)
			w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.CredentialSwitchV1)
			setCoordState(t, env, fx.accountID, "quarantined")
			req := StateRequest{State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseCodexAccountUnavailable), ClaimGeneration: i64Ptr(1)}
			switch fence {
			case "stale":
				req.ClaimGeneration = i64Ptr(0)
			case "released":
				env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", fx.runID)
			case "missing":
				req.ClaimGeneration = nil
			case "nonrunning":
				env.exec("UPDATE runs SET status='paused' WHERE id=$1", fx.runID)
			case "stop":
				env.exec("UPDATE runs SET stop_kind='cancelled' WHERE id=$1", fx.runID)
			}
			before := mustRun(t, env, fx.runID)
			_, applied, err := fx.svc.SetState(env.ctx, w, fx.runID, req)
			if fence == "stop" {
				if err != nil || !applied || mustRun(t, env, fx.runID).Status != "cancelled" {
					t.Fatalf("stop: %v %v", applied, err)
				}
			} else {
				want := ErrStaleClaim
				if fence == "missing" {
					want = ErrInvalidState
				}
				if fence == "nonrunning" {
					want = nil
				}
				if applied || (want == nil && err != nil) || (want != nil && !errors.Is(err, want)) || !reflect.DeepEqual(before, mustRun(t, env, fx.runID)) {
					t.Fatalf("fence: applied=%v err=%v want=%v", applied, err, want)
				}
			}
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
		})
	}
}

// A6b/A8: account-first re-login cannot deadlock the run/alias/account NOWAIT order.
func TestRunningCodexAccountParkContentionLiveDB(t *testing.T) {
	for _, lock := range []string{"account", "alias", "account-then-alias"} {
		t.Run(lock, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, w, _ := runningAccountParkFixture(t, env)
			setCoordState(t, env, fx.accountID, "quarantined")
			before := mustRun(t, env, fx.runID)
			tx, err := env.pool.Begin(env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(env.ctx) }()
			if lock != "alias" {
				if _, err := tx.Exec(env.ctx, "SELECT id FROM codex_provider_account WHERE id=$1 FOR UPDATE", fx.accountID); err != nil {
					t.Fatal(err)
				}
			}
			if lock != "account" {
				if _, err := tx.Exec(env.ctx, "SELECT user_secret_id FROM codex_credential_state WHERE user_secret_id=$1 FOR UPDATE", fx.aliasID); err != nil {
					t.Fatal(err)
				}
			}
			observer := &accountParkObserver{pool: env.pool}
			fx.svc.txBeginner = observer
			start := time.Now()
			_, applied, err := reportAccountPark(t, fx, w)
			duration := time.Since(start)
			if err != nil || !applied || duration > time.Duration(finishRunClaimAttempts)*finishRunClaimRetryDelay+time.Second ||
				duration < time.Duration(finishRunClaimAttempts-1)*finishRunClaimRetryDelay {
				t.Fatalf("bounded fallback: applied=%v err=%v duration=%s", applied, err, duration)
			}
			if observer.begins != finishRunClaimAttempts+1 || observer.contentions != finishRunClaimAttempts {
				t.Fatalf("attempts=%d contentions=%d", observer.begins, observer.contentions)
			}
			assertAccountParkCustody(t, fx, before, false)
			if err := tx.Commit(env.ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A8 positively observes the account-first writer waiting for the park's alias lock.
// Both operations share a two-second context; cancellation joins the writer on every exit.
func TestRunningCodexAccountParkReloginOverlapLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, _ := runningAccountParkFixture(t, env)
	setCoordState(t, env, fx.accountID, "quarantined")
	before := mustRun(t, env, fx.runID)
	ctx, cancel := context.WithTimeout(env.ctx, 2*time.Second)
	defer cancel()
	writer, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(env.ctx) }()
	if _, err := writer.Exec(ctx, "SELECT id FROM codex_provider_account WHERE id=$1 FOR UPDATE", fx.accountID); err != nil {
		t.Fatal(err)
	}
	startWriter := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		select {
		case <-startWriter:
		case <-ctx.Done():
			done <- ctx.Err()
			return
		}
		_, err := writer.Exec(ctx, "UPDATE codex_credential_state SET status='staging', material_revision=material_revision+1, provider_account_id=NULL WHERE user_secret_id=$1", fx.aliasID)
		done <- err
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	observed := false
	observer := &accountParkObserver{pool: env.pool}
	observer.afterAlias = func(ctx context.Context) error {
		close(startWriter)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting bool
			if err := env.pool.QueryRow(ctx, "SELECT COALESCE((SELECT wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0 FROM pg_stat_activity WHERE pid=$1),false)", writer.Conn().PgConn().PID()).Scan(&waiting); err != nil {
				return err
			}
			if waiting {
				observed = true
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
	fx.svc.txBeginner = observer
	_, applied, err := fx.svc.SetState(ctx, w, fx.runID, StateRequest{State: "recovery_wait",
		RecoveryCause: strPtr(recoveryCauseCodexAccountUnavailable), ClaimGeneration: i64Ptr(1),
		CheckpointContainsLatest: boolPtr(true), SessionID: strPtr("park-session")})
	if err != nil || !applied || !observed {
		t.Fatalf("overlap: applied=%v observed=%v err=%v", applied, observed, err)
	}
	if err := <-done; err != nil {
		joined = true
		t.Fatal(err)
	}
	joined = true
	assertAccountParkCustody(t, fx, before, false)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// A5: the running staging hold follows the real Sweep and claim resume path.
func TestRunningCodexAccountParkSameIdentitySweepLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, _ := runningAccountParkFixture(t, env)
	setCoordState(t, env, fx.accountID, "quarantined")
	material := startRelogin(t, env, fx, "staging")
	before := mustRun(t, env, fx.runID)
	_, applied, err := reportAccountPark(t, fx, w)
	if err != nil || !applied {
		t.Fatalf("staging report: %v %v", applied, err)
	}
	assertAccountParkCustody(t, fx, before, true)
	access := codexToken("relogin-access")
	relogin(t, env, env.q, fx, access)
	linkAlias(t, env, fx, fx.accountID, material)
	svc := gateSweepService(env, fx.svc)
	svc.codexPark.after, svc.codexPark.capOverride = uuidPredecessor(fx.runID), 1
	svc.codexPromote.after, svc.codexPromote.capOverride = uuidPredecessor(fx.runID), 1
	result, err := svc.Sweep(env.ctx)
	if err != nil || result.CodexAccountPromoted != 1 || result.CodexAccountReadmitted != 1 {
		t.Fatalf("Sweep: %+v %v", result, err)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	payload, err := svc.Claim(env.ctx, w, nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil || payload.Secrets.Codex.AccessToken != access ||
		mustRun(t, env, fx.runID).ClaimGeneration != 2 {
		t.Fatalf("resume: payload=%v err=%v", payload != nil, err)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
}

// The existing untyped SQL deliberately refuses these kinds (A6a), even with a capable reporter.
func TestRunningCodexAccountParkExcludedKindsLiveDB(t *testing.T) {
	for _, kind := range []string{"judge", "job", "cross_check"} {
		t.Run(kind, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, w, _ := runningAccountParkFixture(t, env)
			// Use a distinct repo-backed parent, as real judge/checker children do.
			parent := uuid.New()
			env.exec("INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status) VALUES ($1,$2,$3,'issue',2,'parent','fixture','completed')", parent, fx.userID, mustRun(t, env, fx.runID).RepoID)
			switch kind {
			case "judge":
				env.exec("UPDATE runs SET kind='judge', repo_id=NULL, issue_iid=NULL, branch=NULL, target_run_id=$2 WHERE id=$1", fx.runID, parent)
			case "job":
				env.exec("UPDATE runs SET kind='job', repo_id=NULL, issue_iid=NULL, branch=NULL, job_type='research' WHERE id=$1", fx.runID)
			case "cross_check":
				env.exec("UPDATE runs SET kind='cross_check', issue_iid=NULL, target_run_id=$2, report_only=true, budget_wall_seconds=600 WHERE id=$1", fx.runID, parent)
			}
			before := mustRun(t, env, fx.runID)
			_, applied, err := reportAccountPark(t, fx, w)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal(err)
			}
			if applied || !reflect.DeepEqual(before, mustRun(t, env, fx.runID)) {
				t.Fatal("excluded kind parked")
			}
		})
	}
}
