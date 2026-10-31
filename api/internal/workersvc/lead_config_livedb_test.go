package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// lead_config_livedb_test.go covers RecordLeadConfig (PRD #3244 section 4) against a real
// Postgres: the duplicate decision under the run-row lock. Skipped unless UZI_TEST_DATABASE_URL is
// set (./e2e/run-store-it.sh); it reuses interlockLiveDB and seedMemoRun.

// leadReport is the requested-only report of the default Claude case.
func leadReport(gen int64, phase leadcfg.Phase, seq int16) leadcfg.Report {
	return leadcfg.Report{
		ClaimGeneration: gen, Phase: phase, PhaseEntry: 1, Seq: seq,
		RequestedModel: strp("opus"), RequestedModelSource: leadcfg.ModelSourceUserDefault,
		RequestedEffort: strp("medium"), RequestedEffortMode: leadcfg.EffortModeExplicit,
		RequestedEffortSource: leadcfg.EffortSourceBuiltin,
	}
}

// fencedInsertParams is the statement-1 parameter set RecordLeadConfig derives from rep, so the test
// can run the same statement on a transaction it controls.
func fencedInsertParams(run, worker uuid.UUID, rep leadcfg.Report) store.InsertRunLeadConfigFencedParams {
	return store.InsertRunLeadConfigFencedParams{
		RunID: run, WorkerID: worker, ClaimGeneration: rep.ClaimGeneration,
		Phase: string(rep.Phase), PhaseEntry: rep.PhaseEntry, Seq: rep.Seq,
		RequestedModel: pgconv.TextPtr(rep.RequestedModel), RequestedModelSource: string(rep.RequestedModelSource),
		ObservedModel: pgconv.TextPtr(rep.ObservedModel), ObservedModelEvidence: pgconv.TextPtr(rep.ObservedModelEvidence),
		RequestedEffort: pgconv.TextPtr(rep.RequestedEffort), RequestedEffortMode: string(rep.RequestedEffortMode),
		RequestedEffortSource: string(rep.RequestedEffortSource),
		ObservedEffort:        pgconv.TextPtr(rep.ObservedEffort), ObservedEffortEvidence: pgconv.TextPtr(rep.ObservedEffortEvidence),
		Truncated: rep.Truncated,
	}
}

func (e interlockLiveDB) leadConfigRows(t *testing.T, run uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM run_lead_configs WHERE run_id = $1`, run).Scan(&n); err != nil {
		t.Fatalf("count lead configs: %v", err)
	}
	return n
}

type leadRecordResult struct {
	res RecordResult
	err error
}

// concurrentRecord holds txA (READ COMMITTED) open with the fenced insert of a, starts
// svc.RecordLeadConfig(b) on the pool, waits event-driven until a backend blocked behind a lock is
// running a run_lead_configs query (10 s deadline, failing, never skipping), commits txA and returns
// the second writer's result.
func concurrentRecord(t *testing.T, e interlockLiveDB, svc *Service, wkr store.Worker, run uuid.UUID, a, b leadcfg.Report) leadRecordResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()

	txA, err := e.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	defer func() { _ = txA.Rollback(context.Background()) }()
	ins, err := store.New(txA).InsertRunLeadConfigFenced(ctx, fencedInsertParams(run, wkr.ID, a))
	if err != nil || !ins.Fenced || !ins.Inserted {
		t.Fatalf("txA insert = %+v, %v; want fenced and inserted", ins, err)
	}
	if n := e.leadConfigRows(t, run); n != 0 {
		t.Fatalf("txA's row is visible before commit (%d rows)", n)
	}

	done := make(chan leadRecordResult, 1)
	go func() {
		res, err := svc.RecordLeadConfig(context.Background(), wkr, run, b)
		done <- leadRecordResult{res, err}
	}()

	for {
		var blocked int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		        WHERE cardinality(pg_blocking_pids(pid)) > 0 AND query ILIKE '%run_lead_configs%'`).Scan(&blocked); err != nil {
			t.Fatalf("poll pg_stat_activity: %v", err)
		}
		if blocked > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the retry finished (%+v) without ever blocking behind txA: the interleaving was not reached", r)
		case <-ctx.Done():
			t.Fatal("no backend blocked on a run_lead_configs query within 10s: the concurrent interleaving was not reached")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit txA: %v", err)
	}
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		t.Fatal("the blocked retry did not finish after txA committed")
		return leadRecordResult{}
	}
}

// TestRecordLeadConfigConcurrentIdenticalRetryLiveDB: an identical retry that waits on the run-row
// lock held by the first writer's uncommitted insert is a duplicate success (Inserted=false, no
// error), not a false key conflict, and exactly one row exists. The variant differing only in a
// nullable field (observed_model_evidence NULL vs unknown) is a key conflict that keeps the first row.
func TestRecordLeadConfigConcurrentIdenticalRetryLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)

	t.Run("identical", func(t *testing.T) {
		run := e.seedMemoRun(t, w, memoRun{})
		x := leadReport(1, leadcfg.PhasePlan, 1)
		r := concurrentRecord(t, e, svc, wkr, run, x, x)
		if r.err != nil || r.res.Inserted {
			t.Fatalf("identical concurrent retry = %+v, %v; want a duplicate success (Inserted=false, nil)", r.res, r.err)
		}
		if n := e.leadConfigRows(t, run); n != 1 {
			t.Fatalf("rows = %d, want exactly 1", n)
		}
	})

	t.Run("differs only in a nullable field", func(t *testing.T) {
		run := e.seedMemoRun(t, w, memoRun{})
		a := leadReport(1, leadcfg.PhasePlan, 1) // observed_model_evidence NULL
		b := a
		b.ObservedModelEvidence = strp(string(leadcfg.ModelEvidenceUnknown))
		r := concurrentRecord(t, e, svc, wkr, run, a, b)
		if !errors.Is(r.err, store.ErrLeadConfigKeyConflict) {
			t.Fatalf("differing concurrent retry = %+v, %v; want ErrLeadConfigKeyConflict", r.res, r.err)
		}
		var ev *string
		if err := e.pool.QueryRow(e.ctx, `SELECT observed_model_evidence FROM run_lead_configs WHERE run_id = $1`, run).Scan(&ev); err != nil {
			t.Fatalf("read first row: %v", err)
		}
		if ev != nil {
			t.Fatalf("observed_model_evidence = %q, want the first row kept (NULL)", *ev)
		}
		if n := e.leadConfigRows(t, run); n != 1 {
			t.Fatalf("rows = %d, want exactly 1", n)
		}
	})
}

// TestRecordLeadConfigSequentialIdenticalRepostLiveDB: a row with NULL columns posted twice is a
// duplicate success the second time (IS NOT DISTINCT FROM, not =): a requested-only row (all four
// observed columns NULL) and a Claude "not sent" row (observed_effort NULL with claude_init evidence).
func TestRecordLeadConfigSequentialIdenticalRepostLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	w := e.seedWorker(t, nil)
	wkr := memoWorker(e, w)

	notSent := leadReport(1, leadcfg.PhasePlan, 2)
	notSent.ObservedModel = strp("claude-opus-fixture")
	notSent.ObservedModelEvidence = strp(string(leadcfg.ModelEvidenceClaudeInit))
	notSent.ObservedEffortEvidence = strp(string(leadcfg.EffortEvidenceClaudeInit)) // observed_effort stays NULL

	for name, rep := range map[string]leadcfg.Report{
		"requested only":  leadReport(1, leadcfg.PhasePlan, 1),
		"claude not sent": notSent,
	} {
		t.Run(name, func(t *testing.T) {
			run := e.seedMemoRun(t, w, memoRun{})
			first, err := svc.RecordLeadConfig(e.ctx, wkr, run, rep)
			if err != nil || !first.Inserted {
				t.Fatalf("first post = %+v, %v; want inserted", first, err)
			}
			second, err := svc.RecordLeadConfig(e.ctx, wkr, run, rep)
			if err != nil || second.Inserted {
				t.Fatalf("second post = %+v, %v; want a duplicate success (Inserted=false, nil)", second, err)
			}
			if n := e.leadConfigRows(t, run); n != 1 {
				t.Fatalf("rows = %d, want 1", n)
			}
		})
	}
}
