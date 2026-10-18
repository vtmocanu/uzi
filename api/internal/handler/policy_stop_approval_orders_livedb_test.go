package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Observe the generated statements on the real transaction. All SQL, arguments,
// row scanning and commits delegate unchanged; only scheduling is controlled.
type policyStopApprovalBeginner struct {
	pool           *pgxpool.Pool
	pid            chan uint32
	lockAttempt    chan struct{}
	locked         chan struct{}
	releaseLock    <-chan struct{}
	verdictScanned chan struct{}
	commitAttempt  chan struct{}
	releaseCommit  <-chan struct{}
}

func (b policyStopApprovalBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Reading connection metadata issues no SQL before SET TRANSACTION.
	select {
	case b.pid <- tx.Conn().PgConn().PID():
	case <-ctx.Done():
		_ = tx.Rollback(ctx)
		return nil, ctx.Err()
	}
	return &policyStopApprovalTx{Tx: tx, observer: b}, nil
}

type policyStopApprovalTx struct {
	pgx.Tx
	observer policyStopApprovalBeginner
}

func (tx *policyStopApprovalTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	var scanned chan struct{}
	var release <-chan struct{}
	switch {
	case strings.HasPrefix(sql, "-- name: GetRunByIDForUpdate :one"):
		if tx.observer.lockAttempt != nil {
			close(tx.observer.lockAttempt)
		}
		scanned, release = tx.observer.locked, tx.observer.releaseLock
	case strings.HasPrefix(sql, "-- name: CreateGateVerdictInput :one"):
		scanned = tx.observer.verdictScanned
	}
	return policyStopApprovalRow{Row: tx.Tx.QueryRow(ctx, sql, args...), ctx: ctx, scanned: scanned, release: release}
}

func (tx *policyStopApprovalTx) Commit(ctx context.Context) error {
	if tx.observer.commitAttempt != nil {
		close(tx.observer.commitAttempt)
	}
	if err := policyStopApprovalRelease(ctx, tx.observer.releaseCommit); err != nil {
		return err
	}
	return tx.Tx.Commit(ctx)
}

type policyStopApprovalRow struct {
	pgx.Row
	ctx     context.Context
	scanned chan struct{}
	release <-chan struct{}
}

func (r policyStopApprovalRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	if r.scanned != nil {
		close(r.scanned)
	}
	return policyStopApprovalRelease(r.ctx, r.release)
}

func policyStopApprovalRelease(ctx context.Context, release <-chan struct{}) error {
	if release == nil {
		return nil
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func policyStopApprovalActor(f *policyStopConfirmEnv, observer policyStopApprovalBeginner) *policyStopConfirmEnv {
	actor := *f
	env := *f.wallParkExtendHTTPEnv
	// Match the base fixture without copying Handler mutexes or cached state.
	env.h = &Handler{q: f.h.q}
	actor.wallParkExtendHTTPEnv = &env
	policyStopRaceService(&actor, f.race, observer)
	return &actor
}

func policyStopApprovalPID(t *testing.T, ctx context.Context, pid <-chan uint32) uint32 {
	t.Helper()
	select {
	case p := <-pid:
		return p
	case <-ctx.Done():
		t.Fatalf("waiting for owned transaction PID: %v", ctx.Err())
		return 0
	}
}

// Poll only the two owned fixture connections, once per tick until the shared
// ten-second deadline. A query failure fails this ordering; it is never success.
func policyStopApprovalBlocked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waiter, holder uint32) {
	t.Helper()
	if waiter == 0 || holder == 0 || waiter == holder || waiter > 1<<31-1 || holder > 1<<31-1 {
		t.Fatalf("invalid owned backend pair: waiter=%d holder=%d", waiter, holder)
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, "SELECT $2::int = ANY(pg_blocking_pids($1::int))", int64(waiter), int64(holder)).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("owned backend %d did not block behind %d: %v", waiter, holder, ctx.Err())
		}
	}
}

func policyStopApprovalReceipt(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var wire apitypes.RunInputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.PolicyStop == nil || wire.PolicyStop.Disposition != "accepted" || wire.PolicyStop.ScopeCeiling != 1 || wire.PolicyStop.AcceptedAt.IsZero() {
		t.Fatalf("missing accepted Stop receipt: %s", rec.Body)
	}
}

func policyStopApprovalPending(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f *policyStopConfirmEnv, body string) {
	t.Helper()
	var pending bool
	if err := pool.QueryRow(ctx, `SELECT gate_binding='unbound' AND gate_revision IS NULL
 AND consumed_at IS NULL AND disposition IS NULL
 FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND body=$2`, f.id, body).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("real verdict SQL did not persist an unbound pending approval")
	}
}

// The old plain_approval_without_Stop case maps to the eligible control here;
// plain_approval_after_Stop_ACK maps to Stop_first below. Seeded source fixtures
// prove API transaction ordering only, with no worker/source-authority claim.
func TestPolicyStopApprovalOrdersLiveDB(t *testing.T) {
	t.Run("pending_feedback_refuses_ordinary_approval", func(t *testing.T) {
		f := newPolicyStopConfirmEnv(t, 1, "revise")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if f.scalar(`SELECT (gate_binding='bound' AND gate_revision=3 AND applied_at IS NULL
 AND disposition IS NULL)::text FROM run_user_inputs WHERE id=$1`, f.humanID) != "true" {
			t.Fatal("fixture must contain bound unapplied revision feedback")
		}
		before := f.snapshot()
		result := policyStopRacePOST(t, f, `{"kind":"approve_plan","body":"pending feedback"}`, "")
		policyStopRaceResponse(t, ctx, result, http.StatusConflict, "plan_revision_pending")
		if f.snapshot() != before {
			t.Fatal("pending-feedback refusal mutated full fixture, including unapplied feedback")
		}
	})

	t.Run("plain_approval_without_Stop", func(t *testing.T) {
		f := newPolicyStopConfirmNoFeedbackEnv(t)
		pool := policyStopRacePool(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sqlDone := make(chan struct{})
		actor := policyStopApprovalActor(f, policyStopApprovalBeginner{pool: pool, pid: make(chan uint32, 1), verdictScanned: sqlDone})
		result := policyStopRacePOST(t, actor, `{"kind":"approve_plan","body":"eligible control"}`, "")
		policyStopRaceWait(t, ctx, sqlDone, "real transaction-bound CreateGateVerdictInput Scan")
		rec := policyStopRaceResponse(t, ctx, result, http.StatusAccepted, "")
		var response apitypes.RunInputResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		run, err := f.race.GetRunByID(ctx, f.id)
		if err != nil {
			t.Fatal(err)
		}
		if response.ServerSide || run.Status != "paused" || len(run.PolicyStopResolution) != 0 {
			t.Fatalf("eligible approval did not enqueue normally: response=%s status=%s", rec.Body, run.Status)
		}
		policyStopApprovalPending(t, ctx, pool, f, "eligible control")
	})

	for _, feedback := range []bool{false, true} {
		name := "Stop_first_no_feedback"
		if feedback {
			name = "Stop_first_pending_feedback"
		}
		t.Run(name, func(t *testing.T) {
			var f *policyStopConfirmEnv
			if feedback {
				f = newPolicyStopConfirmEnv(t, 1, "revise")
			} else {
				f = newPolicyStopConfirmNoFeedbackEnv(t)
			}
			token := f.review().StopIfMatch
			pool := policyStopRacePool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stopLocked, releaseStop := make(chan struct{}), make(chan struct{})
			stopPID := make(chan uint32, 1)
			stopActor := policyStopApprovalActor(f, policyStopApprovalBeginner{
				pool: pool, pid: stopPID, locked: stopLocked, releaseLock: releaseStop,
			})
			stopResult := policyStopRacePOST(t, stopActor, `{"kind":"stop","body":"keep completed work"}`, token)
			policyStopRaceWait(t, ctx, stopLocked, "Stop owns run lock")
			approveAttempt, approveLocked, releaseApproval := make(chan struct{}), make(chan struct{}), make(chan struct{})
			approvePID, sqlDone := make(chan uint32, 1), make(chan struct{})
			approveActor := policyStopApprovalActor(f, policyStopApprovalBeginner{
				pool: pool, pid: approvePID, lockAttempt: approveAttempt, locked: approveLocked,
				releaseLock: releaseApproval, verdictScanned: sqlDone,
			})
			approvalResult := policyStopRacePOST(t, approveActor, `{"kind":"approve_plan","body":"delayed owner verdict"}`, "")
			// Reaching this generated lock statement follows the initial read and
			// early Stop guard, while Stop is still uncommitted.
			policyStopRaceWait(t, ctx, approveAttempt, "approval GetRunByIDForUpdate attempt")
			policyStopApprovalBlocked(t, ctx, pool,
				policyStopApprovalPID(t, ctx, approvePID), policyStopApprovalPID(t, ctx, stopPID))
			close(releaseStop)
			policyStopApprovalReceipt(t, policyStopRaceResponse(t, ctx, stopResult, http.StatusAccepted, ""))
			policyStopRaceWait(t, ctx, approveLocked, "approval fresh locked read after Stop commit")
			acceptedSnapshot := f.snapshot()
			if feedback && f.scalar(`SELECT (applied_at IS NULL AND disposition='superseded_by_stop'
 AND settled_at IS NOT NULL)::text FROM run_user_inputs WHERE id=$1`, f.humanID) != "true" {
				t.Fatal("Stop did not settle the still-unapplied feedback")
			}
			close(releaseApproval)
			policyStopRaceResponse(t, ctx, approvalResult, http.StatusConflict, "policy_stop_gate_resolved")
			if f.snapshot() != acceptedSnapshot {
				t.Fatal("delayed approval changed accepted Stop full snapshot")
			}
			select {
			case <-sqlDone:
				t.Fatal("resolved gate reached verdict SQL")
			default:
			}
		})
	}

	t.Run("approval_first_Stop_waits_for_approval_commit", func(t *testing.T) {
		f := newPolicyStopConfirmNoFeedbackEnv(t)
		token := f.review().StopIfMatch
		pool := policyStopRacePool(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sqlDone, committing, releaseApproval := make(chan struct{}), make(chan struct{}), make(chan struct{})
		approvePID := make(chan uint32, 1)
		approveActor := policyStopApprovalActor(f, policyStopApprovalBeginner{
			pool: pool, pid: approvePID, verdictScanned: sqlDone, commitAttempt: committing, releaseCommit: releaseApproval,
		})
		approvalResult := policyStopRacePOST(t, approveActor, `{"kind":"approve_plan","body":"approval first"}`, "")
		policyStopRaceWait(t, ctx, sqlDone, "real transaction-bound CreateGateVerdictInput Scan")
		policyStopRaceWait(t, ctx, committing, "approval before real Commit")
		stopAttempt, stopPID := make(chan struct{}), make(chan uint32, 1)
		stopActor := policyStopApprovalActor(f, policyStopApprovalBeginner{pool: pool, pid: stopPID, lockAttempt: stopAttempt})
		stopResult := policyStopRacePOST(t, stopActor, `{"kind":"stop","body":"keep completed work"}`, token)
		policyStopRaceWait(t, ctx, stopAttempt, "Stop GetRunByIDForUpdate attempt")
		policyStopApprovalBlocked(t, ctx, pool,
			policyStopApprovalPID(t, ctx, stopPID), policyStopApprovalPID(t, ctx, approvePID))
		close(releaseApproval)
		policyStopRaceResponse(t, ctx, approvalResult, http.StatusAccepted, "")
		policyStopApprovalReceipt(t, policyStopRaceResponse(t, ctx, stopResult, http.StatusAccepted, ""))
		acceptedSnapshot := f.snapshot()
		policyStopApprovalPending(t, ctx, pool, f, "approval first")
		if f.snapshot() != acceptedSnapshot {
			t.Fatal("approval changed full snapshot after Stop ACK")
		}
	})
}

// A delayed direct SQL caller has the same guarded query as the successful
// control. Neither branch fabricates a verdict row or bypasses generated SQL.
func TestPolicyStopDelayedPlainVerdictSQLLiveDB(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "without_Stop"
		if stopped {
			name = "after_Stop_ACK"
		}
		t.Run(name, func(t *testing.T) {
			f := newPolicyStopConfirmNoFeedbackEnv(t)
			pool := policyStopRacePool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Allocate the same caller parameters before the owner decision.
			params := store.CreateGateVerdictInputParams{
				RunID: f.id, Body: pgtype.Text{String: "direct delayed approval", Valid: true},
			}
			if stopped {
				f.accept(f.review().StopIfMatch)
			}
			before := f.snapshot()
			_, err := store.New(pool).CreateGateVerdictInput(ctx, params)
			if stopped {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("direct verdict after Stop: %v, want ErrNoRows", err)
				}
				if f.snapshot() != before {
					t.Fatal("direct delayed verdict SQL mutated accepted Stop full snapshot")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				policyStopApprovalPending(t, ctx, pool, f, "direct delayed approval")
			}
		})
	}
}
