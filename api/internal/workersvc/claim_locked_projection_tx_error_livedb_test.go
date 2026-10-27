package workersvc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Overlay this file on ee1e258e69cee9fd50b528757cfbf0110579dbab for the
// unfixed comparison. Its oracle is degraded delivery after a real SQLSTATE
// 22012, not a fake read error with a still-usable transaction.
func TestClaimLockedProjectionTxErrorLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	for _, mode := range []string{"control", "seq", "time", "both"} {
		t.Run(mode, func(t *testing.T) {
			f := newCodexClaimFix(t, e, false)
			w := f.claimant(t, true)
			t.Cleanup(func() { dropCustodyHolds(t, e, f.userID) })
			e.exec(`UPDATE runs SET plan_md='persisted plan',plan_source='agent',auto_approve=false,
 session_id='persisted-session',milestones_frozen='[{"id":"frozen","title":"Frozen work"}]'
 WHERE id=$1`, f.runID)
			at := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
			seq := int64(mustRun(t, e, f.runID).LastSeq) + 1
			e.exec(`INSERT INTO run_messages(run_id,seq,kind,payload,created_at)
 VALUES($1,$2,'plan','{"plan_md":"persisted plan"}',$3)`, f.runID, seq, at)
			fault := &lockedPlanFrameFaultBeginner{TxBeginner: e.pool, mode: mode}
			var before store.Run
			hooked := false
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
				if err != nil || p == nil || run.ID != f.runID {
					t.Fatalf("assembly: %v", err)
				}
				if p.PlanApproved || p.ResumePhase != "awaiting_approval" || p.ResumePlanSeq != seq ||
					p.ResumePlanAt == nil || !p.ResumePlanAt.Equal(at) {
					t.Fatal("pooled assembly did not read the unapproved persisted plan frame")
				}
				before = mustRun(t, e, f.runID)
				hooked = true
				// Only the final transaction gets the fault; pooled assembly is normal.
				f.svc.SetTxBeginner(fault)
			}}
			p, err := f.svc.Claim(e.ctx, w, nil)
			if !hooked {
				t.Fatalf("assembly hook not reached: %v", err)
			}
			// Require the actual database error before the delivery assertion so
			// setup, query matching or compilation failures cannot masquerade as red.
			wantSeqFault := mode == "seq" || mode == "both"
			wantTimeFault := mode == "time" || mode == "both"
			if mode != "control" && (fault.hits == 0 || fault.seqErrors+fault.timeErrors == 0) {
				t.Fatalf("missing real SQLSTATE 22012: seq=%d time=%d hits=%d", fault.seqErrors, fault.timeErrors, fault.hits)
			}
			if mode == "control" && fault.hits != 0 {
				t.Fatal("control unexpectedly injected a fault")
			}
			if err != nil || p == nil {
				t.Fatalf("degraded delivery after real SQLSTATE 22012: payload=%v err=%v commitErr=%v", p != nil, err, fault.commitErr)
			}
			if (wantSeqFault && fault.seqErrors == 0) || (wantTimeFault && fault.timeErrors == 0) ||
				fault.seqErrors+fault.timeErrors != fault.hits {
				t.Fatal("successful delivery did not recover each injected SQLSTATE 22012")
			}
			if fault.commits != 1 || fault.commitErr != nil {
				t.Fatalf("final transaction did not commit: commits=%d err=%v", fault.commits, fault.commitErr)
			}
			if !fault.locked || fault.savepointsBeforeLock != 0 || fault.connectionChanged {
				t.Fatal("final owned run lock did not precede savepoints on the same connection")
			}
			// Deliberately do not require savepoint counts: this file also overlays
			// the unfixed baseline, which must fail at degraded delivery above.
			wantSeq := seq
			if wantSeqFault {
				wantSeq = 0
			}
			if p.ResumePlanSeq != wantSeq || (wantTimeFault && p.ResumePlanAt != nil) ||
				(!wantTimeFault && (p.ResumePlanAt == nil || !p.ResumePlanAt.Equal(at))) ||
				p.PlanMd == nil || *p.PlanMd != "persisted plan" || p.PlanApproved ||
				p.ResumePhase != "awaiting_approval" {
				t.Fatalf("wrong degraded plan projection: seq=%d at=%v approved=%v phase=%s", p.ResumePlanSeq, p.ResumePlanAt, p.PlanApproved, p.ResumePhase)
			}
			after := mustRun(t, e, f.runID)
			if after.Status != "claimed" || after.WorkerID != pgconv.UUID(w.ID) ||
				after.ClaimGeneration != before.ClaimGeneration || after.WorkerID != before.WorkerID ||
				after.PlanMd != before.PlanMd || after.PlanSource != before.PlanSource ||
				!bytes.Equal(after.MilestonesFrozen, before.MilestonesFrozen) ||
				!bytes.Equal(after.MilestonesCandidate, before.MilestonesCandidate) ||
				after.FailOrigin != before.FailOrigin || after.FinishedAt != before.FinishedAt {
				t.Fatal("degraded projection changed claim identity, plan/frozen rows or terminal state")
			}
			if p.ExecutionMilestones == nil || len(*p.ExecutionMilestones) != 1 ||
				(*p.ExecutionMilestones)[0].ID != "frozen" {
				t.Fatal("degraded projection lost frozen execution definitions")
			}
		})
	}
}

type lockedPlanFrameFaultBeginner struct {
	TxBeginner
	mode                 string
	hits                 int
	seqErrors            int
	timeErrors           int
	commits              int
	commitErr            error
	locked               bool
	savepointsBeforeLock int
	connectionChanged    bool
}

func (b *lockedPlanFrameFaultBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lockedPlanFrameFaultTx{Tx: tx, owner: b, root: true, pid: tx.Conn().PgConn().PID()}, nil
}

type lockedPlanFrameFaultTx struct {
	pgx.Tx
	owner *lockedPlanFrameFaultBeginner
	root  bool
	pid   uint32
}

// Rewrap every nested Begin so store.New(savepoint) still reaches the fault.
func (tx *lockedPlanFrameFaultTx) Begin(ctx context.Context) (pgx.Tx, error) {
	if !tx.owner.locked {
		tx.owner.savepointsBeforeLock++
	}
	sp, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if sp.Conn().PgConn().PID() != tx.pid {
		tx.owner.connectionChanged = true
	}
	return &lockedPlanFrameFaultTx{Tx: sp, owner: tx.owner, pid: tx.pid}, nil
}

func (tx *lockedPlanFrameFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: GetRunOwnedByWorkerForUpdate :one") {
		return lockedPlanFrameObservedRow{Row: tx.Tx.QueryRow(ctx, sql, args...), observe: func(err error) {
			if err == nil {
				tx.owner.locked = true
			}
		}}
	}
	seq := strings.HasPrefix(sql, "-- name: LatestPlanSeqForRun :one")
	at := strings.HasPrefix(sql, "-- name: LatestPersistedPlanFrameAtForRun :one")
	inject := (seq && (tx.owner.mode == "seq" || tx.owner.mode == "both")) ||
		(at && (tx.owner.mode == "time" || tx.owner.mode == "both"))
	if !inject {
		return tx.Tx.QueryRow(ctx, sql, args...)
	}
	tx.owner.hits++
	// Execute literal SQL on the underlying transaction. Scan receives the real
	// PostgreSQL error and the transaction is aborted until savepoint rollback.
	return lockedPlanFrameObservedRow{Row: tx.Tx.QueryRow(ctx, "SELECT 1/0"), observe: func(err error) {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22012" {
			if seq {
				tx.owner.seqErrors++
			} else {
				tx.owner.timeErrors++
			}
		}
	}}
}

func (tx *lockedPlanFrameFaultTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	if tx.root {
		tx.owner.commits++
		tx.owner.commitErr = err
	}
	return err
}

type lockedPlanFrameObservedRow struct {
	pgx.Row
	observe func(error)
}

func (r lockedPlanFrameObservedRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.observe(err)
	return err
}
