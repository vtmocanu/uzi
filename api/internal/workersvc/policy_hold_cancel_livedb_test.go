package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests require UZI_TEST_DATABASE_URL and use the existing retention
// fixture's real SQL, advisory locks, sealed forge credential and inline settle.
type policyCancelLiveStore struct {
	Store
	f                      *retentionFix
	t                      *testing.T
	checkpoints, teardowns int
}

func (s *policyCancelLiveStore) assertCommitted(id uuid.UUID) {
	s.t.Helper()
	if got := s.f.runStatus(s.t, id); got != "cancelled" {
		s.t.Fatalf("effect observed uncommitted status %q", got)
	}
}
func (s *policyCancelLiveStore) InsertCheckpointRetentionIfHeld(ctx context.Context, p store.InsertCheckpointRetentionIfHeldParams) (int64, error) {
	s.assertCommitted(p.RunID)
	s.checkpoints++
	return s.Store.InsertCheckpointRetentionIfHeld(ctx, p)
}
func (s *policyCancelLiveStore) DeleteEphemeralWorkerForRun(ctx context.Context, p store.DeleteEphemeralWorkerForRunParams) (int64, error) {
	s.assertCommitted(p.RunID)
	s.teardowns++
	return s.Store.DeleteEphemeralWorkerForRun(ctx, p)
}

type policyCancelLiveBroadcaster struct {
	fakeBroadcaster
	st *policyCancelLiveStore
	id uuid.UUID
}

func (b *policyCancelLiveBroadcaster) PublishState(id uuid.UUID, status string) {
	b.st.assertCommitted(id)
	if id != b.id || status != "cancelled" {
		b.st.t.Fatal("incorrect published cancellation")
	}
	b.fakeBroadcaster.PublishState(id, status)
}

type policyCancelLiveLifecycle struct {
	fakeLifecycle
	st *policyCancelLiveStore
}

func (l *policyCancelLiveLifecycle) Notify(id uuid.UUID, status string) {
	l.st.assertCommitted(id)
	l.fakeLifecycle.Notify(id, status)
}

func TestPolicyHoldCancelOrdinaryEffectsLiveDB(t *testing.T) {
	for _, custody := range []bool{false, true} {
		name := "without_custody"
		if custody {
			name = "open_custody"
		}
		t.Run(name, func(t *testing.T) {
			f := newRetentionFix(t)
			wid := f.e.seedWorker(t, []string{capability.ProviderPolicyHoldV1})
			id, iid := f.seedPublishedRun(t, wid)
			f.e.exec(t, "UPDATE runs SET harness='codex', session_id='saved-session', stop_kind='cancelled' WHERE id=$1", id)
			f.e.exec(t, "UPDATE workers SET ephemeral=true, ephemeral_run_id=$2 WHERE id=$1", wid, id)
			f.e.fpSeedScopeDirected(t, id)
			var holdID uuid.UUID
			if custody {
				holdID = f.openHold(t, id, wid, 1)
			}
			worker, err := f.e.q.GetWorkerByID(f.e.ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.e.q.GetRunByID(f.e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			st := &policyCancelLiveStore{Store: f.e.q, f: f, t: t}
			f.svc.q = st
			b := &policyCancelLiveBroadcaster{st: st, id: id}
			lc := &policyCancelLiveLifecycle{st: st}
			f.svc.SetBroadcaster(b)
			f.svc.SetLifecycle(lc)
			// The locked database row must override this stale authentication snapshot.
			worker.Ephemeral = false
			req := policyHoldFixture()
			got, err := f.svc.ReportPolicyHold(f.e.ctx, worker, id, req)
			if err != nil || got.Disposition != "terminal" || got.ObservedStatus != "cancelled" {
				t.Fatalf("cancel reply=%+v err=%v", got, err)
			}
			after, err := f.e.q.GetRunByID(f.e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "cancelled" || after.StopKind.String != "cancelled" || after.FailOrigin.Valid ||
				after.SessionID != before.SessionID || after.CheckpointTip != before.CheckpointTip ||
				after.PolicyOutcomeID != before.PolicyOutcomeID || after.PolicySourceGeneration != before.PolicySourceGeneration ||
				after.PolicyPreserveRequired != before.PolicyPreserveRequired || after.PolicyRefusalCount != before.PolicyRefusalCount {
				t.Fatal("cancellation changed saved source/session/policy evidence")
			}
			if !reflect.DeepEqual(b.statuses, []string{"cancelled"}) ||
				!reflect.DeepEqual(lc.notes, []lifecycleNote{{runID: id, status: "cancelled"}}) {
				t.Fatalf("publish=%v notify=%v", b.statuses, lc.notes)
			}
			if disposition, settled := f.e.scopeDisposition(t, id); !settled || disposition != "declined" {
				t.Fatalf("scope disposition=%q settled=%v", disposition, settled)
			}
			if st.checkpoints != 1 || st.teardowns != 1 {
				t.Fatalf("checkpoint/teardown seam calls=%d/%d", st.checkpoints, st.teardowns)
			}
			_, workerErr := f.e.q.GetWorkerByID(f.e.ctx, wid)
			if custody {
				f.assertRetained(t, id, iid)
				if workerErr != nil {
					t.Fatalf("custody worker deleted: %v", workerErr)
				}
				var state string
				var liveWorker, liveRun uuid.UUID
				if err := f.e.pool.QueryRow(f.e.ctx, "SELECT state, live_worker_id, live_run_id FROM recovery_custody_holds WHERE id=$1", holdID).Scan(&state, &liveWorker, &liveRun); err != nil {
					t.Fatal(err)
				}
				if state != "open" || liveWorker != wid || liveRun != id {
					t.Fatal("cancellation released live custody")
				}
				// A terminal replay must not redrive any of the five effects.
				replay, err := f.svc.ReportPolicyHold(f.e.ctx, worker, id, req)
				if err != nil || replay.Disposition != "terminal" {
					t.Fatalf("replay=%+v err=%v", replay, err)
				}
				if len(b.statuses) != 1 || len(lc.notes) != 1 || st.checkpoints != 1 || st.teardowns != 1 {
					t.Fatal("terminal replay repeated effects")
				}
			} else {
				f.assertDeleted(t, id, agentIssueBranch(iid))
				if !errors.Is(workerErr, pgx.ErrNoRows) {
					t.Fatalf("ephemeral worker remains: %v", workerErr)
				}
			}
		})
	}
}

// Failure injection wraps a real transaction: the real cancellation UPDATE is
// rolled back when the authoritative read or commit fails.
type policyCancelFailBeginner struct {
	inner TxBeginner
	stage string
}

func (b policyCancelFailBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &policyCancelFailTx{Tx: tx, stage: b.stage}, nil
}

type policyCancelFailTx struct {
	pgx.Tx
	stage string
}

func (tx *policyCancelFailTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.stage == "readback" && strings.HasPrefix(sql, "-- name: GetRunOwnedByWorker ") {
		return policyReplayRow{err: errors.New("injected authoritative read failure")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *policyCancelFailTx) Commit(ctx context.Context) error {
	if tx.stage == "commit" {
		return errors.New("injected commit failure")
	}
	return tx.Tx.Commit(ctx)
}
func (tx *policyCancelFailTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "-- name: CancelRunByWorker ") {
		if tx.stage == "mutation" {
			return pgconn.CommandTag{}, errors.New("injected mutation failure")
		}
		if tx.stage == "zero_rows" {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestPolicyHoldCancelRollbackLiveDB(t *testing.T) {
	for _, stage := range []string{"mutation", "zero_rows", "readback", "commit", "owner_stop"} {
		t.Run(stage, func(t *testing.T) {
			f := newRetentionFix(t)
			wid := f.e.seedWorker(t, []string{capability.ProviderPolicyHoldV1})
			id, _ := f.seedPublishedRun(t, wid)
			stop := "cancelled"
			if stage == "owner_stop" {
				stop = "stopped"
			}
			f.e.exec(t, "UPDATE runs SET harness='codex', session_id='saved-session', stop_kind=$2 WHERE id=$1", id, stop)
			f.e.fpSeedScopeDirected(t, id)
			worker, err := f.e.q.GetWorkerByID(f.e.ctx, wid)
			if err != nil {
				t.Fatal(err)
			}
			before, err := f.e.q.GetRunByID(f.e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			b, lc := &fakeBroadcaster{}, &fakeLifecycle{}
			f.svc.SetBroadcaster(b)
			f.svc.SetLifecycle(lc)
			f.svc.SetTxBeginner(policyCancelFailBeginner{inner: f.e.pool, stage: stage})
			got, err := f.svc.ReportPolicyHold(f.e.ctx, worker, id, policyHoldFixture())
			wantErr := stage != "zero_rows" && stage != "owner_stop"
			if (err != nil) != wantErr {
				t.Fatalf("reply=%+v err=%v", got, err)
			}
			if !wantErr && got.Disposition != "pending" {
				t.Fatalf("reply=%+v", got)
			}
			after, err := f.e.q.GetRunByID(f.e.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed/no-op cancellation changed run")
			}
			if len(b.statuses) != 0 || len(lc.notes) != 0 || len(f.deleteCalls()) != 0 {
				t.Fatal("failed/no-op cancellation ran effects")
			}
			if _, settled := f.e.scopeDisposition(t, id); settled {
				t.Fatal("failed/no-op cancellation settled scope")
			}
			if _, exists := f.row(t, id); exists {
				t.Fatal("rolled back cancellation left retention record")
			}
		})
	}
}
