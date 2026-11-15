package workersvc

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSourceEndDeclinedCancelOverlapLiveDB(t *testing.T) {
	for _, operation := range []string{"park", "resume"} {
		for _, first := range []string{"cancel", "operation"} {
			t.Run(operation+"/"+first, func(t *testing.T) {
				e, svc, w, id := sourceEndFixture(t, "running")
				e.exec(t, "UPDATE workers SET last_heartbeat_at=now()-interval '1 day' WHERE id=$1", w.ID)
				if operation == "resume" {
					if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); err != nil || !applied {
						t.Fatalf("initial hold=%v %v", applied, err)
					}
					// Commit a distinct registration before either contender starts.
					e.exec(t, "UPDATE workers SET snapshot_register_nonce='new-registration', last_heartbeat_at=now() WHERE id=$1", w.ID)
				}
				before := sourceEndRead(t, e, id)
				custody := sourceEndCustody(t, e, id)
				open, total := e.countHolds(t, id)
				ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
				defer cancel()
				seam := sourceEndLockSeam{
					Pool: e.pool, acquired: true, reached: make(chan struct{}),
					release: make(chan struct{}), pid: make(chan uint32, 1),
				}
				if first == "operation" {
					seam.queryTarget = "-- name: GetRunOwnedByWorkerForUpdate :one"
				}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(seam.release) }) }
				svc.SetTxBeginner(seam)
				cancelConn, err := e.pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer cancelConn.Release()
				cancelSvc := New(store.New(cancelConn), newBox(t), testParams())
				cancelSvc.SetTxBeginner(cancelConn)
				cancelSvc.SetBackground(func(func()) {})
				type outcome struct {
					applied bool
					err     error
				}
				opDone := make(chan outcome, 1)
				opJoined := make(chan struct{})
				go func() {
					defer close(opJoined)
					if operation == "park" {
						_, applied, err := svc.SetState(ctx, w, id, sourceEndReport())
						opDone <- outcome{applied, err}
					} else {
						err := svc.ResumeSourceEndHold(ctx, w.UserID, before, 3600)
						opDone <- outcome{err == nil, err}
					}
				}()
				defer func() { release(); cancel(); <-opJoined }()
				select {
				case <-seam.reached:
				case got := <-opDone:
					t.Fatalf("operation finished before seam: %+v", got)
				case <-ctx.Done():
					t.Fatal("operation did not acquire seam lock", ctx.Err())
				}
				opPID := <-seam.pid
				cancelDone := make(chan error, 1)
				cancelJoined := make(chan struct{})
				go func() {
					defer close(cancelJoined)
					res, err := cancelSvc.SubmitInput(ctx, w.UserID, id, "cancel", "owner ends source hold", nil)
					if err == nil && !res.ServerSide {
						err = errors.New("cancel unexpectedly enqueued")
					}
					cancelDone <- err
				}()
				defer func() { release(); cancel(); <-cancelJoined }()
				var savedCancel store.Run
				if first == "operation" {
					// The operation owns worker and run. Positively observe the
					// real cancellation waiting on this operation's transaction.
					if err := sourceEndWaitBlocked(ctx, e.pool, cancelConn.Conn().PgConn().PID(), opPID, cancelDone); err != nil {
						t.Fatal(err)
					}
					release()
				} else {
					// The operation owns only worker, so cancellation must finish
					// while the operation is still paused before its run query.
					if err := <-cancelDone; err != nil {
						t.Fatal(err)
					}
					savedCancel = sourceEndRead(t, e, id)
					release()
				}
				got := <-opDone
				if first == "operation" {
					if !got.applied || got.err != nil {
						t.Fatalf("winning operation applied=%v err=%v", got.applied, got.err)
					}
					if err := <-cancelDone; err != nil {
						t.Fatal(err)
					}
				} else {
					if got.applied || (got.err != nil && !errors.Is(got.err, pgx.ErrNoRows) && !errors.Is(got.err, ErrStaleClaim)) {
						t.Fatalf("losing operation applied=%v err=%v", got.applied, got.err)
					}
					if operation == "resume" && !errors.Is(got.err, pgx.ErrNoRows) {
						t.Fatalf("losing Resume: %v", got.err)
					}
					if !reflect.DeepEqual(savedCancel, sourceEndRead(t, e, id)) {
						t.Fatal("losing operation changed cancellation")
					}
				}
				cancelled := sourceEndRead(t, e, id)
				if cancelled.Status != "cancelled" || cancelled.StopKind.String != "cancelled" || !cancelled.FinishedAt.Valid {
					t.Fatalf("cancel not terminal: %+v", cancelled)
				}
				wantWait, wantEpisode := before.RecoveryWaitCount, before.WorkerRecoveryEpisode
				wantReleasedID, wantReleasedNonce := before.ReleasedWorkerID, before.ReleasedWorkerNonce
				if first == "operation" {
					if operation == "park" {
						wantWait++
						wantReleasedID = before.WorkerID
						wantReleasedNonce.String, wantReleasedNonce.Valid = sourceEndReport().RegisterNonce, true
					} else {
						wantEpisode++
					}
				}
				wantReleased := before.ClaimReleasedAt.Valid || (first == "operation" && operation == "park")
				if cancelled.ClaimReleasedAt.Valid != wantReleased ||
					cancelled.RecoveryWaitCount != wantWait || cancelled.WorkerRecoveryEpisode != wantEpisode ||
					cancelled.ClaimGeneration != before.ClaimGeneration || cancelled.ReleasedWorkerID != wantReleasedID ||
					cancelled.ReleasedWorkerNonce != wantReleasedNonce {
					t.Fatalf("cancel lost accounting or released identity: %+v", cancelled)
				}
				// The one-operation seam must not intercept the replay queries.
				svc.SetTxBeginner(e.pool)
				if _, applied, _ := svc.SetState(ctx, w, id, sourceEndReport()); applied {
					t.Fatal("late report reparks cancellation")
				}
				if err := svc.ResumeSourceEndHold(ctx, w.UserID, before, 3600); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("late Resume: %v", err)
				}
				if !reflect.DeepEqual(cancelled, sourceEndRead(t, e, id)) || sourceEndCustody(t, e, id) != custody {
					t.Fatal("late operations changed terminal row or custody")
				}
				if a, b := e.countHolds(t, id); a != open || b != total {
					t.Fatal("overlap changed custody counts")
				}
			})
		}
	}
}
