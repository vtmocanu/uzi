package workersvc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Existing public transaction seam: expose the backend identity and pause ONLY
// before COMMIT so the test can prove the authenticator remains locked.
type latePolicyBeginner struct {
	TxBeginner
	started    chan uint32
	committing chan struct{}
	release    chan struct{}
}
type latePolicyTx struct {
	pgx.Tx
	b *latePolicyBeginner
}

func (b *latePolicyBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	b.started <- tx.Conn().PgConn().PID()
	return &latePolicyTx{Tx: tx, b: b}, nil
}
func (tx *latePolicyTx) Commit(ctx context.Context) error {
	close(tx.b.committing)
	select {
	case <-tx.b.release:
		return tx.Tx.Commit(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func latePolicyBlocked(t *testing.T, e usageTailEnv, ctx context.Context, waiter, blocker uint32) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := e.pool.QueryRow(ctx, "SELECT $2::int = ANY(pg_blocking_pids($1::int))", int64(waiter), int64(blocker)).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("expected database lock wait: ", ctx.Err())
		case <-tick.C:
		}
	}
}

func TestPolicyLateCustodyDiscardOrderingsLiveDB(t *testing.T) {
	for _, discardFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("discard_first_%t", discardFirst), func(t *testing.T) {
			e := setupPolicyHold(t)
			hold := latePolicyHold(t, e)
			req := policyRequest()
			latePolicyExec(t, e, "UPDATE runs SET claim_generation=2,status='running',released_worker_id=NULL,released_worker_nonce=NULL WHERE id=$1", e.runID)
			capture := uuid.New()
			latePolicyExec(t, e, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
 VALUES($1,$2,$3,$4,'policy-late-fixture',$5,$6,'preparing')`, capture, hold, e.runID, e.userID, strings.Repeat("a", 40), uuid.NewString())
			ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
			defer cancel()
			b := &latePolicyBeginner{TxBeginner: e.pool, started: make(chan uint32, 1), committing: make(chan struct{}), release: make(chan struct{})}
			e.svc.SetTxBeginner(b)
			released := false
			defer func() {
				if !released {
					close(b.release)
				}
			}()
			discard, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = discard.Rollback(context.Background()) }()
			dq := store.New(discard)
			discardPID := discard.Conn().PgConn().PID()
			doDiscard := func() error {
				n, err := dq.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: hold, RunID: e.runID, UserID: e.userID, ReleaseEvidence: pgconv.TextOrNull("owner_discard")})
				if err != nil {
					return err
				}
				if n != 1 {
					return fmt.Errorf("discarded %d holds", n)
				}
				_, err = dq.DiscardNonReadyCapturesForHold(ctx, hold)
				return err
			}
			if discardFirst {
				if err := doDiscard(); err != nil {
					t.Fatal(err)
				}
			}
			type result struct {
				d   PolicyHoldDisposition
				err error
			}
			reported := make(chan result, 1)
			go func() { _, d, err := e.svc.ReportPolicyHold(ctx, e.wkr, e.runID, req); reported <- result{d, err} }()
			var reportPID uint32
			select {
			case reportPID = <-b.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if discardFirst {
				latePolicyBlocked(t, e, ctx, reportPID, discardPID)
				if err := discard.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				close(b.release)
				released = true
				select {
				case r := <-reported:
					if r.err != nil || string(r.d) != "unauthenticated_historical_outcome" {
						t.Fatalf("discard first: %+v", r)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			} else {
				select {
				case <-b.committing:
				case r := <-reported:
					t.Fatalf("report did not reach metadata commit: %+v", r)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				discarded := make(chan error, 1)
				go func() { discarded <- doDiscard() }()
				latePolicyBlocked(t, e, ctx, discardPID, reportPID)
				close(b.release)
				released = true
				select {
				case r := <-reported:
					if r.err != nil || string(r.d) != "recorded" {
						t.Fatalf("report first: %+v", r)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case err := <-discarded:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if err := discard.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state string
			if err := e.pool.QueryRow(ctx, "SELECT state FROM recovery_captures WHERE id=$1", capture).Scan(&state); err != nil || state != "discarded" {
				t.Fatalf("capture state=%s err=%v", state, err)
			}
			want := int32(1)
			if discardFirst {
				want = 0
			}
			if r := mustRun(t, e.codexTestEnv, e.runID); r.PolicyRefusalCount != want || r.Status != "running" {
				t.Fatalf("count=%d status=%s", r.PolicyRefusalCount, r.Status)
			}
			// Even the duplicate must authenticate AFTER the actual shared discard commits.
			e.svc.SetTxBeginner(e.pool)
			_, d, err := e.svc.ReportPolicyHold(ctx, e.wkr, e.runID, req)
			if err != nil || string(d) != "unauthenticated_historical_outcome" {
				t.Fatalf("post-discard duplicate=%s err=%v", d, err)
			}
		})
	}
}
