package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Snapshot every durable row touched by reconciliation, including timestamps.
// Sequence gaps on a rolled-back INSERT are intentionally not row changes.
func reconciliationRaceSnapshot(t *testing.T, f *gateFix, id uuid.UUID) string {
	t.Helper()
	var raw []byte
	err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
 'run', (SELECT to_jsonb(r) FROM runs r WHERE id=$1),
 'inputs', (SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY id),'[]') FROM run_user_inputs i WHERE run_id=$1),
 'permits', (SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY id),'[]') FROM run_completion_permits p WHERE run_id=$1),
 'retention', (SELECT coalesce(jsonb_agg(to_jsonb(c)),'[]') FROM checkpoint_retentions c WHERE run_id=$1),
 'audit', (SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY id),'[]') FROM run_milestone_reconciliations a WHERE run_id=$1))`, id).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func reconciliationRaceUnchanged(t *testing.T, f *gateFix, id uuid.UUID, before string) {
	t.Helper()
	if after := reconciliationRaceSnapshot(t, f, id); before != after {
		t.Fatalf("refused/rolled-back reconciliation changed rows:\nbefore=%s\nafter=%s", before, after)
	}
}

func reconciliationRaceDeadline(t *testing.T, f *gateFix) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	f.ctx = ctx
	t.Cleanup(cancel)
}

func reconciliationRaceDigest(t *testing.T, f *gateFix, id uuid.UUID) string {
	t.Helper()
	d, err := ResetStateDigest(reconciliationRow(t, f, id))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMilestoneReconciliationExactDigestCASLiveDB(t *testing.T) {
	for _, change := range []struct{ name, sql string }{
		{"completed", `UPDATE runs SET milestones_completed='["M1"]' WHERE id=$1`},
		{"scope", `UPDATE runs SET scope_ceiling=2 WHERE id=$1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			reconciliationRaceDeadline(t, f)
			f.exec(change.sql, id)
			if d := reconciliationRaceDigest(t, f, id); d == opts.ExpectedResetStateDigest {
				t.Fatal("authoritative state change did not change reset digest")
			}
			before := reconciliationRaceSnapshot(t, f, id)
			_, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
			if !errors.Is(err, ErrReconciliationConflict) {
				t.Fatalf("old digest accepted: %v", err)
			}
			reconciliationRaceUnchanged(t, f, id, before)
			opts.ExpectedResetStateDigest = reconciliationRaceDigest(t, f, id)
			if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
				t.Fatalf("fresh exact digest refused: %v", err)
			}
			assertReconciliationProgressEmpty(t, reconciliationRow(t, f, id))
		})
	}
	t.Run("historical disposition excluded", func(t *testing.T) {
		f, id, opts := newMilestoneReconciliationFix(t)
		reconciliationRaceDeadline(t, f)
		f.exec(`INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'stop','historical')`, id)
		f.exec(`UPDATE run_user_inputs SET disposition='superseded' WHERE run_id=$1 AND body='historical'`, id)
		if d := reconciliationRaceDigest(t, f, id); d != opts.ExpectedResetStateDigest {
			t.Fatal("historical input disposition changed reset digest")
		}
		if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
			t.Fatalf("unchanged digest refused: %v", err)
		}
	})
}

func TestMilestoneReconciliationRefusalRowsLiveDB(t *testing.T) {
	for _, name := range []string{"absent reason", "wrong completion revision", "payload", "checkpoint ref"} {
		t.Run(name, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			reconciliationRaceDeadline(t, f)
			want := ErrReconciliationConflict
			switch name {
			case "absent reason":
				opts.Reconciliation.Reason = ""
			case "wrong completion revision":
				revision := 2
				opts.Reconciliation.ExpectedCompletionRevision = &revision
			case "payload":
				f.exec(`UPDATE runs SET gate_presented_payload=jsonb_set(gate_presented_payload,'{plan_md}','"different plan"') WHERE id=$1`, id)
				// Fresh consent isolates payload validation from the reset digest CAS.
				opts.ExpectedResetStateDigest = reconciliationRaceDigest(t, f, id)
			case "checkpoint ref":
				want = ErrReconciliationNotReady
				f.svc.SetListRefTipsFn(func(_ context.Context, _ pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
					return map[string]string{refs[0]: "3333333333333333333333333333333333333333"}, nil
				})
			}
			before := reconciliationRaceSnapshot(t, f, id)
			_, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
			if !errors.Is(err, want) {
				t.Fatalf("refusal=%v, want %v", err, want)
			}
			reconciliationRaceUnchanged(t, f, id, before)
		})
	}
}

func TestMilestoneReconciliationEmptyReplacementLiveDB(t *testing.T) {
	for _, value := range []string{"[]", "SQL NULL", `[{"id":"M1"}]`} {
		t.Run(value, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			reconciliationRaceDeadline(t, f)
			// Keep the presented digest internally consistent so malformed/absent
			// candidates reach list validation rather than fail at payload binding.
			r := reconciliationRow(t, f, id)
			p, ok, err := decodeGatePresentedPayload(r.GatePresentedPayload)
			if err != nil || !ok {
				t.Fatalf("decode payload: %v %v", ok, err)
			}
			raw := value
			if value == "SQL NULL" {
				raw = "null"
			}
			p.Milestones = json.RawMessage(raw)
			payload, digest, err := p.digest()
			if err != nil {
				t.Fatal(err)
			}
			var candidate any = []byte(raw)
			if value == "SQL NULL" {
				candidate = nil
			}
			f.exec(`UPDATE runs SET milestones_candidate=$2,gate_presented_payload=$3,gate_payload_digest=$4 WHERE id=$1`, id, candidate, payload, digest)
			opts.ExpectedResetStateDigest = reconciliationRaceDigest(t, f, id)
			before := reconciliationRaceSnapshot(t, f, id)
			_, err = f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
			if value != "[]" {
				if !errors.Is(err, ErrReconciliationConflict) {
					t.Fatalf("malformed replacement accepted: %v", err)
				}
				reconciliationRaceUnchanged(t, f, id, before)
				return
			}
			if err != nil {
				t.Fatalf("explicit empty replacement refused: %v", err)
			}
			after := reconciliationRow(t, f, id)
			assertReconciliationProgressEmpty(t, after)
			if !jsonEqual(t, after.MilestonesFrozen, "[]") || after.ContractRevision.Int32 != 2 || !after.ClaimReleasedAt.Valid {
				t.Fatalf("empty replacement not installed/released: %+v", after)
			}
			var contract struct {
				Revision int               `json:"revision"`
				Criteria []json.RawMessage `json:"criteria"`
			}
			if err := json.Unmarshal(after.CompletionContract, &contract); err != nil || contract.Revision != 2 || len(contract.Criteria) != 0 {
				t.Fatalf("empty contract=%s err=%v", after.CompletionContract, err)
			}
		})
	}
}

func TestMilestoneReconciliationAuditRollbackLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	reconciliationRaceDeadline(t, f)
	f.exec(`INSERT INTO run_completion_permits(run_id,contract_revision,branch,head,issued_by_worker_id)
 VALUES($1,1,'agent/old',$2,$3)`, id, retentionTestTip, f.wkr.ID)
	f.exec(`INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'stop','pending stop')`, id)
	// Unique trigger/function, scoped to this run; no shared fixture edits.
	name := "reconcile_race_" + fmt.Sprintf("%x", id[:])
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.run_id='%s'::uuid THEN RAISE EXCEPTION 'reconciliation race audit failure' USING ERRCODE='P0001'; END IF; RETURN NEW; END $$`, name, id))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := f.pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON run_milestone_reconciliations; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
			t.Errorf("trigger cleanup: %v", err)
		}
	})
	f.exec("CREATE TRIGGER " + name + " BEFORE INSERT ON run_milestone_reconciliations FOR EACH ROW EXECUTE FUNCTION " + name + "()")
	before := reconciliationRaceSnapshot(t, f, id)
	_, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "P0001" || pe.Message != "reconciliation race audit failure" {
		t.Fatalf("expected audit trigger failure, got %v", err)
	}
	reconciliationRaceUnchanged(t, f, id, before)
	f.exec("DROP TRIGGER " + name + " ON run_milestone_reconciliations")
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatalf("retry after removing trigger: %v", err)
	}
	assertReconciliationProgressEmpty(t, reconciliationRow(t, f, id))
}

// A transaction-local commit barrier holds the real operation's locks after
// its writes. Cancellation always releases it; no production hook is altered.
type reconciliationRaceBeginner struct {
	conn    *pgx.Conn
	ready   chan struct{}
	release chan struct{}
}
type reconciliationRaceTx struct {
	pgx.Tx
	barrier *reconciliationRaceBeginner
}

func (b *reconciliationRaceBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &reconciliationRaceTx{Tx: tx, barrier: b}, nil
}
func (tx *reconciliationRaceTx) Commit(ctx context.Context) error {
	close(tx.barrier.ready)
	select {
	case <-tx.barrier.release:
		return tx.Tx.Commit(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func reconciliationRaceWait(t *testing.T, ctx context.Context, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatalf("commit barrier not reached: %v", ctx.Err())
	}
}

func reconciliationRaceBlocked(t *testing.T, f *gateFix, pid uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(f.ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-f.ctx.Done():
			t.Fatalf("connection %d never waited for lock: %v", pid, f.ctx.Err())
		}
	}
}

// Each racing request gets an independent runtime service and the fixture's
// configured dependencies; no mutex, queue or generation state is copied.
func reconciliationRaceService(parent *Service, q Store) *Service {
	s := New(q, parent.box, parent.p)
	s.now = parent.now
	s.SetBackground(parent.background)
	s.SetRetentionLockPool(parent.retentionPool)
	s.SetForgeBaseURLAllowed(parent.forgeBaseURLAllowed)
	s.SetListRefTipsFn(parent.listRefTipsFn)
	s.SetCapabilitySettings(parent.capabilitySettings)
	return s
}

func TestMilestoneReconciliationSerializationLiveDB(t *testing.T) {
	for _, ownerFirst := range []bool{true, false} {
		name := "report before owner digest"
		if ownerFirst {
			name = "owner before stale report"
		}
		t.Run(name, func(t *testing.T) {
			f, id, opts := newMilestoneReconciliationFix(t)
			reconciliationRaceDeadline(t, f)
			f.exec(`UPDATE runs SET milestones_completed='["M1"]' WHERE id=$1`, id)
			opts.ExpectedResetStateDigest = reconciliationRaceDigest(t, f, id)
			presented := reconciliationRow(t, f, id)
			ownerConn, err := f.pool.Acquire(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(ownerConn.Release)
			reportConn, err := f.pool.Acquire(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reportConn.Release)
			// Both public operations must override a stricter connection default.
			for _, conn := range []*pgx.Conn{ownerConn.Conn(), reportConn.Conn()} {
				if _, err := conn.Exec(f.ctx, "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := conn.Exec(context.Background(), "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
						t.Errorf("restore connection isolation: %v", err)
					}
				})
			}
			ownerSvc := reconciliationRaceService(f.svc, store.New(ownerConn))
			reportSvc := reconciliationRaceService(f.svc, store.New(reportConn))
			ownerSvc.SetTxBeginner(ownerConn)
			reportSvc.SetTxBeginner(reportConn)
			barrier := &reconciliationRaceBeginner{ready: make(chan struct{}), release: make(chan struct{})}
			if ownerFirst {
				barrier.conn = ownerConn.Conn()
				ownerSvc.SetTxBeginner(barrier)
			} else {
				barrier.conn = reportConn.Conn()
				reportSvc.SetTxBeginner(barrier)
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
			// Drain both goroutines before releasing their connections, even on fatal.
			var wg sync.WaitGroup
			defer func() { release(); wg.Wait() }()
			ownerDone, reportDone := make(chan error, 1), make(chan error, 1)
			owner := func() {
				defer wg.Done()
				_, err := ownerSvc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts)
				ownerDone <- err
			}
			report := func() {
				defer wg.Done()
				req := StateRequest{State: "running", ClaimGeneration: i64(1), MilestonesCompleted: &[]string{"M2"}}
				if !ownerFirst {
					// Keep gate/status unchanged: only authoritative session state
					// moves, so refusal must bind to the newly locked reset digest.
					pid := uuid.UUID(presented.GatePresentationID.Bytes)
					req = gateReq(i64(1), &pid)
					req.Milestones = &[]Milestone{{ID: "M1", Title: "Replacement one"}, {ID: "M2", Title: "Replacement two"}}
					req.GateCheckpointWitness = &apitypes.GateCheckpointWitness{SourceHead: "1111111111111111111111111111111111111111", PublishedCheckpoint: retentionTestTip}
					session := "reconciliation-race-new-session"
					req.SessionID = &session
				}
				_, applied, _, err := reportSvc.SetStateReport(f.ctx, f.wkr, id, req)
				if err == nil && !applied {
					err = errors.New("report silently not applied")
				}
				reportDone <- err
			}
			wg.Add(1)
			if ownerFirst {
				go owner()
			} else {
				go report()
			}
			reconciliationRaceWait(t, f.ctx, barrier.ready)
			wg.Add(1)
			if ownerFirst {
				go report()
				reconciliationRaceBlocked(t, f, reportConn.Conn().PgConn().PID())
			} else {
				go owner()
				reconciliationRaceBlocked(t, f, ownerConn.Conn().PgConn().PID())
			}
			release()
			wg.Wait()
			ownerErr, reportErr := <-ownerDone, <-reportDone
			after := reconciliationRow(t, f, id)
			if ownerFirst {
				if ownerErr != nil || !errors.Is(reportErr, ErrStaleClaim) {
					t.Fatalf("owner=%v stale report=%v", ownerErr, reportErr)
				}
				assertReconciliationProgressEmpty(t, after)
				if after.Status != "queued" || !after.ClaimReleasedAt.Valid || after.ContractRevision.Int32 != 2 {
					t.Fatalf("owner result overwritten: %+v", after)
				}
			} else {
				if reportErr != nil || !errors.Is(ownerErr, ErrReconciliationConflict) {
					t.Fatalf("report=%v owner digest=%v", reportErr, ownerErr)
				}
				if !jsonEqual(t, after.MilestonesCompleted, `["M1"]`) || after.SessionID.String != "reconciliation-race-new-session" ||
					after.Status != "awaiting_approval" || after.GateRevision != 1 || after.ClaimReleasedAt.Valid ||
					after.ContractRevision.Int32 != 1 || !jsonEqual(t, after.MilestonesFrozen, reconciliationOldList) {
					t.Fatalf("owner replaced committed report: %+v", after)
				}
				if reconciliationRaceDigest(t, f, id) == opts.ExpectedResetStateDigest {
					t.Fatal("report did not invalidate owner digest")
				}
				var approvals, audits int
				if err := f.pool.QueryRow(f.ctx, `SELECT
 (SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'),
 (SELECT count(*) FROM run_milestone_reconciliations WHERE run_id=$1)`, id).Scan(&approvals, &audits); err != nil {
					t.Fatal(err)
				}
				if approvals != 0 || audits != 0 {
					t.Fatalf("stale owner banked approval/audit: %d/%d", approvals, audits)
				}
			}
		})
	}
}
