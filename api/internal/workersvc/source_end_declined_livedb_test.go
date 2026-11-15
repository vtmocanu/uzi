package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func sourceEndReport() StateRequest {
	r := retainedReportCause(1, recoveryCauseRetainedRecoveryAdoptionFailed)
	d := RecoveryDetailSourceEndDeclined
	r.RecoveryDetail, r.RegisterNonce = &d, "source-end-registration"
	return r
}

func sourceEndFixture(t *testing.T, status string) (interlockLiveDB, *Service, store.Worker, uuid.UUID) {
	t.Helper()
	e := setupInterlockLiveDB(t)
	id := e.seedWorker(t, nil)
	run := e.fpSeedRunning(t, id, 1)
	mhOpenHold(t, e, run, 1, id)
	e.exec(t, "UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", id, sourceEndReport().RegisterNonce)
	e.exec(t, "UPDATE runs SET status=$2, registration_evidence_generation=1, registration_evidence_nonce=$3, retained_park_count=2 WHERE id=$1", run, status, sourceEndReport().RegisterNonce)
	w, err := e.q.GetWorkerForUpdate(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return e, e.capacityParkService(t, 3), w, run
}

func sourceEndRead(t *testing.T, e interlockLiveDB, id uuid.UUID) store.Run {
	t.Helper()
	r, err := e.q.GetRunByID(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSourceEndDeclinedHoldAndReplayLiveDB(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			e, svc, w, id := sourceEndFixture(t, status)
			before := sourceEndRead(t, e, id)
			open, total := e.countHolds(t, id)
			r, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport())
			if err != nil || !applied {
				t.Fatalf("park applied=%v err=%v", applied, err)
			}
			if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryAdoptionFailed ||
				r.RecoveryWaitDetail.String != RecoveryDetailSourceEndDeclined || r.RecoveryRetryNotBefore.Valid ||
				!r.ClaimReleasedAt.Valid || !r.ReleasedWorkerID.Valid || r.ReleasedWorkerID.Bytes != w.ID ||
				r.ReleasedWorkerNonce.String != sourceEndReport().RegisterNonce || r.RecoveryWaitCount != before.RecoveryWaitCount+1 ||
				r.RetainedParkCount != before.RetainedParkCount || r.RequeueCount != before.RequeueCount ||
				r.ClaimGeneration != before.ClaimGeneration || r.WorkerID != before.WorkerID {
				t.Fatalf("incorrect atomic hold: %+v", r)
			}
			for i := 0; i < 2; i++ {
				got, applied, err := e.capacityParkService(t, 3).SetState(e.ctx, w, id, sourceEndReport())
				if err != nil || applied || !reflect.DeepEqual(got, r) {
					t.Fatalf("fresh-service replay applied=%v err=%v changed=%v", applied, err, !reflect.DeepEqual(got, r))
				}
			}
			if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now().Add(365*24*time.Hour))); err != nil {
				t.Fatal(err)
			}
			if _, err := e.claimFor(t, w.ID, r); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("held run claimed: %v", err)
			}
			if got := sourceEndRead(t, e, id); !reflect.DeepEqual(got, r) {
				t.Fatal("promotion/claim changed hold")
			}
			if a, b := e.countHolds(t, id); a != open || b != total {
				t.Fatal("hold/replay/promotion changed custody")
			}
			if err := svc.ResumeSourceEndHold(e.ctx, w.UserID, r, 3600); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("same registration resumed: %v", err)
			}
			e.exec(t, "UPDATE workers SET snapshot_register_nonce='distinct-registration' WHERE id=$1", w.ID)
			if err := svc.ResumeSourceEndHold(e.ctx, w.UserID, r, 3600); err != nil {
				t.Fatalf("distinct registration resume: %v", err)
			}
			resumed := sourceEndRead(t, e, id)
			if resumed.Status != "queued" || resumed.RecoveryWaitDetail.Valid || resumed.ClaimGeneration != r.ClaimGeneration ||
				resumed.ReleasedWorkerID != r.ReleasedWorkerID || resumed.ReleasedWorkerNonce != r.ReleasedWorkerNonce {
				t.Fatalf("resume lost released identity or changed generation: %+v", resumed)
			}
			if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); applied || !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("old report after resume: applied=%v err=%v", applied, err)
			}
			claimed, err := e.claimFor(t, w.ID, resumed)
			if err != nil || claimed.ID != id || claimed.ClaimGeneration != r.ClaimGeneration+1 {
				t.Fatalf("fresh incarnation claim: id=%s gen=%d err=%v", claimed.ID, claimed.ClaimGeneration, err)
			}
			if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); applied || !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("old report after new claim: applied=%v err=%v", applied, err)
			}
			if got := sourceEndRead(t, e, id); !reflect.DeepEqual(got, claimed) {
				t.Fatal("old report changed fresh claim")
			}
		})
	}
}

func TestSourceEndDeclinedRefusalsLiveDB(t *testing.T) {
	for _, name := range []string{"missing evidence", "wrong evidence", "wrong nonce", "missing nonce", "stale generation", "wrong user", "wrong worker", "wrong kind", "wrong status", "stop", "invalid shape"} {
		t.Run(name, func(t *testing.T) {
			e, svc, w, id := sourceEndFixture(t, "running")
			req := sourceEndReport()
			switch name {
			case "missing evidence":
				e.exec(t, "UPDATE runs SET registration_evidence_generation=NULL, registration_evidence_nonce=NULL WHERE id=$1", id)
			case "wrong evidence":
				e.exec(t, "UPDATE runs SET registration_evidence_nonce='other' WHERE id=$1", id)
			case "wrong nonce":
				req.RegisterNonce = "other"
			case "missing nonce":
				req.RegisterNonce = ""
			case "stale generation":
				g := int64(2)
				req.ClaimGeneration = &g
			case "wrong user":
				w.UserID = uuid.New()
			case "wrong worker":
				w.ID = e.seedWorker(t, nil)
			case "wrong kind":
				e.exec(t, "UPDATE runs SET kind='chat', repo_id=NULL, issue_iid=NULL, branch=NULL WHERE id=$1", id)
			case "wrong status":
				e.exec(t, "UPDATE runs SET status='queued' WHERE id=$1", id)
			case "stop":
				e.exec(t, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", id)
			case "invalid shape":
				req.State = "running"
			}
			before := sourceEndRead(t, e, id)
			open, total := e.countHolds(t, id)
			_, applied, err := svc.SetState(e.ctx, w, id, req)
			if applied {
				t.Fatalf("refusal applied (err=%v)", err)
			}
			if name == "missing nonce" || name == "invalid shape" {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("invalid shape: %v", err)
				}
			}
			if after := sourceEndRead(t, e, id); !reflect.DeepEqual(before, after) {
				t.Fatal("refused report mutated row")
			}
			if a, b := e.countHolds(t, id); a != open || b != total {
				t.Fatal("refused report mutated custody")
			}
		})
	}
}

func TestSourceEndDeclinedConcurrentDuplicateLiveDB(t *testing.T) {
	e, svc, w, id := sourceEndFixture(t, "running")
	before := sourceEndRead(t, e, id)
	openBefore, totalBefore := e.countHolds(t, id)
	start := make(chan struct{})
	type outcome struct {
		applied bool
		err     error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport())
			results <- outcome{applied, err}
		}()
	}
	close(start)
	count := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.applied {
			count++
		}
	}
	r := sourceEndRead(t, e, id)
	if count != 1 || r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseRetainedRecoveryAdoptionFailed ||
		r.RecoveryWaitDetail.String != RecoveryDetailSourceEndDeclined || r.RecoveryRetryNotBefore.Valid ||
		!r.ClaimReleasedAt.Valid || !r.ReleasedWorkerID.Valid || r.ReleasedWorkerID.Bytes != w.ID ||
		r.ReleasedWorkerNonce.String != sourceEndReport().RegisterNonce || r.RecoveryWaitCount != before.RecoveryWaitCount+1 ||
		r.ClaimGeneration != before.ClaimGeneration || r.WorkerID != before.WorkerID {
		t.Fatalf("duplicate applications=%d row=%+v", count, r)
	}
	if open, total := e.countHolds(t, id); open != openBefore || total != totalBefore {
		t.Fatalf("concurrent duplicate changed custody: open %d->%d total %d->%d", openBefore, open, totalBefore, total)
	}
}

// Inject a server transaction failure, not a worker/slot process crash.
type sourceEndCommitFault struct {
	*pgxpool.Pool
}

func (b sourceEndCommitFault) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.BeginTx(ctx, pgx.TxOptions{})
}
func (b sourceEndCommitFault) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.Pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return sourceEndRollbackTx{tx}, nil
}

type sourceEndRollbackTx struct {
	pgx.Tx
}

func (tx sourceEndRollbackTx) Commit(ctx context.Context) error {
	_ = tx.Rollback(ctx)
	return errors.New("injected server failure before commit")
}

func TestSourceEndDeclinedCrashBeforeCommitLiveDB(t *testing.T) {
	e, svc, w, id := sourceEndFixture(t, "running")
	before := sourceEndRead(t, e, id)
	svc.SetTxBeginner(sourceEndCommitFault{e.pool})
	if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); applied || err == nil {
		t.Fatalf("rollback applied=%v err=%v", applied, err)
	}
	if !reflect.DeepEqual(before, sourceEndRead(t, e, id)) {
		t.Fatal("precommit server failure persisted partial hold")
	}
	if _, applied, err := e.capacityParkService(t, 3).SetState(e.ctx, w, id, sourceEndReport()); !applied || err != nil {
		t.Fatalf("retry after rollback applied=%v err=%v", applied, err)
	}
}

func sourceEndCommittedRestart(t *testing.T, acknowledged bool) {
	t.Helper()
	e, svc, w, id := sourceEndFixture(t, "running")
	r, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport())
	if err != nil || !applied {
		t.Fatalf("commit applied=%v err=%v", applied, err)
	}
	// acknowledged models whether the caller retained the successful response; both
	// server-restart windows must preserve the committed row with no extra effects.
	if acknowledged {
		// After ACK, a fresh server observes the durable hold without requiring a retry.
		if got := sourceEndRead(t, e, id); !reflect.DeepEqual(got, r) {
			t.Fatal("acknowledged hold was not durable")
		}
	}
	restarted := e.capacityParkService(t, 3)
	got, applied, err := restarted.SetState(e.ctx, w, id, sourceEndReport())
	if err != nil || applied || !reflect.DeepEqual(got, r) {
		t.Fatalf("restart replay applied=%v err=%v changed=%v", applied, err, !reflect.DeepEqual(got, r))
	}
}
func TestSourceEndDeclinedCrashAfterCommitBeforeAckLiveDB(t *testing.T) {
	sourceEndCommittedRestart(t, false)
}
func TestSourceEndDeclinedCrashAfterAckLiveDB(t *testing.T) {
	sourceEndCommittedRestart(t, true)
}

func TestSourceEndDeclinedOwnerCancelLiveDB(t *testing.T) {
	e, svc, w, id := sourceEndFixture(t, "running")
	r, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport())
	if err != nil || !applied {
		t.Fatalf("park applied=%v err=%v", applied, err)
	}
	open, total := e.countHolds(t, id)
	res, err := svc.SubmitInput(e.ctx, w.UserID, id, "cancel", "owner ends source hold", nil)
	if err != nil || !res.ServerSide {
		t.Fatalf("owner cancel server-side=%v err=%v", res.ServerSide, err)
	}
	cancelled := sourceEndRead(t, e, id)
	if cancelled.Status != "cancelled" || cancelled.StopKind.String != "cancelled" ||
		cancelled.ReleasedWorkerID != r.ReleasedWorkerID || cancelled.ReleasedWorkerNonce != r.ReleasedWorkerNonce ||
		cancelled.ClaimGeneration != r.ClaimGeneration || cancelled.RecoveryWaitCount != r.RecoveryWaitCount {
		t.Fatalf("cancel changed recovery bounds: %+v", cancelled)
	}
	if _, applied, _ := svc.SetState(e.ctx, w, id, sourceEndReport()); applied {
		t.Fatal("old report applied after cancel")
	}
	if !reflect.DeepEqual(sourceEndRead(t, e, id), cancelled) {
		t.Fatal("old report mutated cancelled run")
	}
	if a, b := e.countHolds(t, id); a != open || b != total {
		t.Fatal("cancel/report changed retained custody")
	}
}
