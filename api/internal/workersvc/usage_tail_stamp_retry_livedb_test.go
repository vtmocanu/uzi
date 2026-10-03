package workersvc

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

// failFirstCommitBeginner delegates to the pool, but the FIRST transaction it begins fails at
// Commit after its statements ran (the transaction is rolled back), as a lost connection or a
// serialization failure at commit would.
type failFirstCommitBeginner struct {
	pool interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	}
	begun atomic.Int32
}

type failCommitTx struct{ pgx.Tx }

var errInjectedCommit = errors.New("injected commit failure")

func (b *failFirstCommitBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if b.begun.Add(1) == 1 {
		return failCommitTx{Tx: tx}, nil
	}
	return tx, nil
}

func (failCommitTx) Commit(ctx context.Context) error { return errInjectedCommit }

func (t failCommitTx) Rollback(ctx context.Context) error { return t.Tx.Rollback(ctx) }

// A database error in the stamp transaction fails the append (the worker re-delivers) instead of
// being dropped, and the re-delivery converges: the coverage stamp lands, the tail excludes the
// covered messages and the metered total is not doubled.
func TestUsageTailStampWriteFailureFailsAppendAndRedeliveryConvergesLiveDB(t *testing.T) {
	e := setupUsageTail(t)
	a := e.leg()
	// Records first: RecordRunUsage and RunUsageTail also Begin on the beginner.
	e.mustPost(t, nil, um(a, "m1", 1), um(a, "m2", 2), um(a, "m3", 3))

	bb := &failFirstCommitBeginner{pool: e.pool}
	e.svc.SetTxBeginner(bb)
	batch := []IncomingMessage{stampedInit(1, a, "S1"), stampedResult(2, a, 2, false, "claude-sonnet-5-5", 123)}

	err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, batch, nil)
	if !errors.Is(err, errInjectedCommit) {
		t.Fatalf("first delivery: err = %v, want the injected stamp commit failure to fail the append", err)
	}
	// The rolled-back stamp tx left the leg without its identity: the tail is unresolved.
	if d := e.tail(t); d == nil || !slices.Contains(d.CoverageReasons, "unresolved") {
		t.Fatalf("tail after the failed stamp commit = %+v, want an unresolved leg", d)
	}

	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, batch, nil); err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	// Ordinals 1 and 2 are covered; only ordinal 3 (input 4) is in the tail.
	if d := e.tail(t); d == nil || d.InputTokens != 4 {
		t.Fatalf("tail after re-delivery = %+v, want only ordinal 3 (input 4)", d)
	}
	tot, err := e.svc.RunUsageTotal(e.ctx, e.runID)
	if err != nil {
		t.Fatal(err)
	}
	if tot.InputTokens != 123 {
		t.Fatalf("metered input = %d, want 123 (not doubled by the re-delivery)", tot.InputTokens)
	}
}
