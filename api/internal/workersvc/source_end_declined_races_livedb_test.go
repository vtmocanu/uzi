package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sourceEndLockSeam signals immediately before the worker-lock query, or after
// Scan has acquired its row lock. Each instance wraps exactly one operation.
type sourceEndLockSeam struct {
	*pgxpool.Pool
	acquired bool
	reached  chan struct{}
	release  chan struct{}
}

func (b sourceEndLockSeam) Begin(ctx context.Context) (pgx.Tx, error) {
	return b.BeginTx(ctx, pgx.TxOptions{})
}

func (b sourceEndLockSeam) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.Pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return sourceEndSeamTx{Tx: tx, seam: b}, nil
}

type sourceEndSeamTx struct {
	pgx.Tx
	seam sourceEndLockSeam
}

func (tx sourceEndSeamTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "-- name: GetWorkerForUpdate :one") {
		return tx.Tx.QueryRow(ctx, sql, args...)
	}
	if !tx.seam.acquired {
		close(tx.seam.reached)
		return tx.Tx.QueryRow(ctx, sql, args...)
	}
	return sourceEndSeamRow{Row: tx.Tx.QueryRow(ctx, sql, args...), ctx: ctx, seam: tx.seam}
}

type sourceEndSeamRow struct {
	pgx.Row
	ctx  context.Context
	seam sourceEndLockSeam
}

func (r sourceEndSeamRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	close(r.seam.reached)
	select {
	case <-r.seam.release:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func TestSourceEndDeclinedCancelOrdersLiveDB(t *testing.T) {
	for _, operation := range []string{"park", "resume"} {
		for _, first := range []string{"cancel", "operation"} {
			t.Run(operation+"/"+first, func(t *testing.T) {
				e, svc, w, id := sourceEndFixture(t, "running")
				// A running flight with no live poller uses SubmitInput's server-side
				// cancellation, as does every released source-end hold.
				e.exec(t, "UPDATE workers SET last_heartbeat_at=now()-interval '1 day' WHERE id=$1", w.ID)
				if operation == "resume" {
					if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); err != nil || !applied {
						t.Fatalf("initial hold=%v %v", applied, err)
					}
					e.exec(t, "UPDATE workers SET snapshot_register_nonce='new-registration', last_heartbeat_at=now() WHERE id=$1", w.ID)
				}
				observed := sourceEndRead(t, e, id)
				custody := func() string {
					t.Helper()
					var s string
					if err := e.pool.QueryRow(e.ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY id)::text,'[]') FROM recovery_custody_holds h WHERE run_id=$1", id).Scan(&s); err != nil {
						t.Fatal(err)
					}
					return s
				}
				before := custody()
				ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
				defer cancel()
				// Each operation completes before the next starts. The channel makes
				// the serialized order explicit; the context bounds DB lock waits.
				run := func(fn func() error) error {
					done := make(chan error, 1)
					go func() { done <- fn() }()
					return <-done
				}
				cancelRun := func() error {
					res, err := svc.SubmitInput(ctx, w.UserID, id, "cancel", "owner ends source hold", nil)
					if err == nil && !res.ServerSide {
						return errors.New("cancel unexpectedly enqueued")
					}
					return err
				}
				other := func() error {
					if operation == "resume" {
						return svc.ResumeSourceEndHold(ctx, w.UserID, observed, 3600)
					}
					_, _, err := svc.SetState(ctx, w, id, sourceEndReport())
					return err
				}
				if first == "operation" {
					if err := run(other); err != nil {
						t.Fatal(err)
					}
					if err := run(cancelRun); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := run(cancelRun); err != nil {
						t.Fatal(err)
					}
					cancelled := sourceEndRead(t, e, id)
					err := run(other)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, ErrStaleClaim) {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(cancelled, sourceEndRead(t, e, id)) {
						t.Fatal("late operation reopened cancelled run")
					}
				}
				cancelled := sourceEndRead(t, e, id)
				if cancelled.Status != "cancelled" || cancelled.StopKind.String != "cancelled" || !cancelled.FinishedAt.Valid {
					t.Fatalf("cancel did not become terminal: %+v", cancelled)
				}
				if _, applied, _ := svc.SetState(ctx, w, id, sourceEndReport()); applied {
					t.Fatal("report reparks cancellation")
				}
				if err := svc.ResumeSourceEndHold(ctx, w.UserID, observed, 3600); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("Resume after cancellation: %v", err)
				}
				if !reflect.DeepEqual(cancelled, sourceEndRead(t, e, id)) || custody() != before {
					t.Fatal("replay/Resume changed cancellation or custody")
				}
			})
		}
	}
}

func TestSourceEndDeclinedRegistrationLockOrdersLiveDB(t *testing.T) {
	for _, operation := range []string{"park", "resume"} {
		for _, first := range []string{"registration", "operation"} {
			for _, commit := range []bool{false, true} {
				name := operation + "/" + first + "/rollback"
				if commit {
					name = operation + "/" + first + "/commit"
				}
				t.Run(name, func(t *testing.T) {
					e, svc, w, id := sourceEndFixture(t, "claimed")
					if operation == "resume" {
						if _, applied, err := svc.SetState(e.ctx, w, id, sourceEndReport()); err != nil || !applied {
							t.Fatalf("initial park=%v %v", applied, err)
						}
					}
					before := sourceEndRead(t, e, id)
					open, total := e.countHolds(t, id)
					ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
					defer cancel()
					seam := sourceEndLockSeam{Pool: e.pool, acquired: first == "operation", reached: make(chan struct{}), release: make(chan struct{})}
					svc.SetTxBeginner(seam)
					type result struct {
						applied bool
						err     error
					}
					done := make(chan result, 1)
					start := func() {
						go func() {
							if operation == "park" {
								_, applied, err := svc.SetState(ctx, w, id, sourceEndReport())
								done <- result{applied, err}
							} else {
								err := svc.ResumeSourceEndHold(ctx, w.UserID, before, 3600)
								done <- result{err == nil, err}
							}
						}()
					}
					reg, err := e.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = reg.Rollback(e.ctx) }()
					update := func() error {
						_, err := reg.Exec(ctx, "UPDATE workers SET snapshot_register_nonce='new-registration' WHERE id=$1", w.ID)
						return err
					}
					if first == "registration" {
						if err := update(); err != nil {
							t.Fatal(err)
						}
						start()
						select {
						case <-seam.reached:
						case <-ctx.Done():
							t.Fatal("operation did not reach worker lock", ctx.Err())
						}
					} else {
						start()
						select {
						case <-seam.reached:
						case <-ctx.Done():
							t.Fatal("operation did not acquire worker lock", ctx.Err())
						}
					}
					registered := make(chan error, 1)
					launched := make(chan struct{})
					go func() {
						close(launched)
						if first == "operation" {
							if err := update(); err != nil {
								registered <- err
								return
							}
						}
						if commit {
							registered <- reg.Commit(ctx)
						} else {
							registered <- reg.Rollback(ctx)
						}
					}()
					<-launched
					close(seam.release)
					got := <-done
					if err := <-registered; err != nil {
						t.Fatal("registration failed", err)
					}
					wantApplied := operation == "park"
					if first == "registration" && commit {
						wantApplied = operation == "resume"
					}
					if got.applied != wantApplied {
						t.Fatalf("applied=%v err=%v want=%v", got.applied, got.err, wantApplied)
					}
					if wantApplied && got.err != nil {
						t.Fatal(got.err)
					}
					if !wantApplied && !errors.Is(got.err, pgx.ErrNoRows) && !errors.Is(got.err, ErrStaleClaim) {
						t.Fatalf("unexpected refusal: %v", got.err)
					}
					current, err := e.q.GetWorkerForUpdate(e.ctx, w.ID)
					if err != nil {
						t.Fatal(err)
					}
					wantNonce := sourceEndReport().RegisterNonce
					if commit {
						wantNonce = "new-registration"
					}
					if current.SnapshotRegisterNonce.String != wantNonce {
						t.Fatalf("current nonce=%q want=%q", current.SnapshotRegisterNonce.String, wantNonce)
					}
					after := sourceEndRead(t, e, id)
					if got.applied {
						wantStatus := "recovery_wait"
						if operation == "resume" {
							wantStatus = "queued"
						}
						if after.Status != wantStatus || after.ClaimGeneration != before.ClaimGeneration {
							t.Fatalf("operation status=%s generation=%d", after.Status, after.ClaimGeneration)
						}
					}
					if operation == "park" && got.applied && after.ReleasedWorkerNonce.String != sourceEndReport().RegisterNonce {
						t.Fatal("park recorded nonce from a later registration")
					}
					if !got.applied && !reflect.DeepEqual(before, after) {
						t.Fatal("refusal changed run")
					}
					if commit {
						_, applied, err := e.capacityParkService(t, 3).SetState(ctx, w, id, sourceEndReport())
						if applied || !errors.Is(err, ErrStaleClaim) || !reflect.DeepEqual(after, sourceEndRead(t, e, id)) {
							t.Fatalf("old registration report applied=%v err=%v", applied, err)
						}
					}
					if a, b := e.countHolds(t, id); a != open || b != total {
						t.Fatal("registration race changed custody")
					}
				})
			}
		}
	}
}
