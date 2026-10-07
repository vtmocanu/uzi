package workersvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each injected trigger is scoped to one exact fixture ID and removed before fixture cleanup.
func boundedLiveTrigger(t *testing.T, f *supersedeFix, table, operation string, id uuid.UUID, action string) {
	t.Helper()
	name := "m2_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	f.e.exec(t, fmt.Sprintf("CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s.id = '%s'::uuid THEN %s END IF; RETURN %s; END $$", name, map[string]string{"UPDATE": "NEW", "DELETE": "OLD"}[operation], id, action, map[string]string{"UPDATE": "NEW", "DELETE": "OLD"}[operation]))
	t.Cleanup(func() { f.e.exec(t, fmt.Sprintf("DROP FUNCTION %s() CASCADE", name)) })
	f.e.exec(t, fmt.Sprintf("CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()", name, operation, table, name))
}

func boundedLiveNoRetention(t *testing.T, f *supersedeFix) {
	t.Helper()
	if _, err := f.e.q.GetCheckpointRetention(f.e.ctx, f.oldRun); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unexpected live retention: %v", err)
	}
}

func TestBoundedLiveAtomicRollbackLiveDB(t *testing.T) {
	for _, base := range []string{"", retentionTestTip} {
		for _, failure := range []string{"update error", "delete error", "delete zero rows"} {
			t.Run(failure+"/base="+base, func(t *testing.T) {
				f := newLiveAttemptFix(t)
				if base != "" {
					f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now()-interval '1 day' WHERE id=$1", f.oldRun, base)
				}
				id := readyLiveAttempt(t, f)
				before, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
				if err != nil {
					t.Fatal(err)
				}
				table, op, target, action := "checkpoint_publish_attempts", "DELETE", id, "RAISE EXCEPTION 'm2 injected delete';"
				if failure == "update error" {
					table, op, target, action = "runs", "UPDATE", f.oldRun, "RAISE EXCEPTION 'm2 injected update';"
				}
				if failure == "delete zero rows" {
					action = "RETURN NULL;"
				}
				boundedLiveTrigger(t, f, table, op, target, action)
				done, settle, err := f.svc1.reconcilePublishAttemptLocked(f.e.ctx, id, func(context.Context) error { return nil })
				if done || settle || (failure != "delete zero rows" && err == nil) || (failure == "delete zero rows" && err != nil) {
					t.Fatalf("rollback outcome: done=%v settle=%v err=%v", done, settle, err)
				}
				assertLiveEvidence(t, f, id, base, false)
				after, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
				if err != nil || before.CheckpointTipAt != after.CheckpointTipAt {
					t.Fatalf("rollback changed timestamp: before=%v after=%v err=%v", before.CheckpointTipAt, after.CheckpointTipAt, err)
				}
				if !f.attemptRow(t, id).ReconcileReadyAt.Valid {
					t.Fatal("eligible evidence lost")
				}
				boundedLiveNoRetention(t, f)
			})
		}
	}
}

type boundedLiveStore struct {
	Store
	inserted       func(uuid.UUID)
	beforeDelete   func(uuid.UUID)
	markCalls      int
	markContextErr error
	markDeadline   time.Time
	failMark       bool
}

func (q *boundedLiveStore) RecordCheckpointPublishAttempt(ctx context.Context, p store.RecordCheckpointPublishAttemptParams) (uuid.UUID, error) {
	id, err := q.Store.RecordCheckpointPublishAttempt(ctx, p)
	if err == nil && q.inserted != nil {
		q.inserted(id)
	}
	return id, err
}
func (q *boundedLiveStore) DeleteCheckpointPublishAttempt(ctx context.Context, id uuid.UUID) (int64, error) {
	if q.beforeDelete != nil {
		q.beforeDelete(id)
	}
	return q.Store.DeleteCheckpointPublishAttempt(ctx, id)
}
func (q *boundedLiveStore) MarkCheckpointPublishAttemptReady(ctx context.Context, id uuid.UUID) (int64, error) {
	q.markCalls++
	q.markContextErr = ctx.Err()
	q.markDeadline, _ = ctx.Deadline()
	if q.failMark {
		return 0, errors.New("m2 readiness unavailable")
	}
	return q.Store.MarkCheckpointPublishAttemptReady(ctx, id)
}
func boundedLiveWorker(t *testing.T, f *supersedeFix) store.Worker {
	t.Helper()
	var id uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT worker_id FROM runs WHERE id=$1", f.oldRun).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return f.rf.wkr(id)
}
func boundedLivePending(t *testing.T, f *supersedeFix, id uuid.UUID) {
	t.Helper()
	before := f.attemptRow(t, id)
	if before.ReconcileReadyAt.Valid {
		t.Fatal("pending attempt became ready")
	}
	f.reconcileAttempts(t, f.svc2)
	done, settle, err := f.svc2.reconcilePublishAttemptLocked(f.e.ctx, id, func(context.Context) error { return nil })
	if err != nil || done || settle {
		t.Fatalf("pending reconcile: %v %v %v", done, settle, err)
	}
	if after := f.attemptRow(t, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("pending evidence mutated: before=%+v after=%+v", before, after)
	}
	assertLiveEvidence(t, f, id, "", false)
	boundedLiveNoRetention(t, f)
	claimed, err := f.e.q.CheckpointTipClaimedByOtherRun(f.e.ctx, store.CheckpointTipClaimedByOtherRunParams{RepoID: pgconv.UUID(f.e.repoID), RunID: f.newRun, Tip: lateTip})
	if err != nil || !claimed {
		t.Fatalf("pending competing claim lost: %v %v", claimed, err)
	}
}

func TestBoundedLiveReturnedPendingOutcomesLiveDB(t *testing.T) {
	for _, outcome := range []string{"generic", "pre-send generic", "sentinel", "already current", "cancelled unknown readiness failure", "advanced readiness failure"} {
		t.Run(outcome, func(t *testing.T) {
			f := newLiveAttemptFix(t)
			original := f.recordAttempt(t, lateTip)
			q := &boundedLiveStore{Store: f.e.q, failMark: strings.Contains(outcome, "readiness failure")}
			var own uuid.UUID
			q.inserted = func(id uuid.UUID) { own = id }
			f.svc1.q = q
			ctx, cancel := context.WithCancel(f.e.ctx)
			defer cancel()
			f.svc1.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				switch outcome {
				case "sentinel":
					return pushbroker.Result{}, pushbroker.ErrTipMissing
				case "already current":
					return pushbroker.Result{AlreadyCurrent: true}, nil
				case "cancelled unknown readiness failure":
					cancel()
					return pushbroker.Result{Disposition: pushbroker.PublishOutcomeUnknown}, context.Canceled
				case "advanced readiness failure":
					f.forge.set(f.branchRef, retentionTestTip)
					if !f.forge.land(f.branchRef, retentionTestTip, lateTip) {
						t.Fatal("advance did not land")
					}
					return pushbroker.Result{Disposition: pushbroker.PublishAdvanced}, nil
				default:
					return pushbroker.Result{}, errors.New("m2 broker returned before typed outcome")
				}
			})
			res, err := f.svc1.Publish(ctx, boundedLiveWorker(t, f), f.oldRun, lateTip, []byte("pack"))
			if own == uuid.Nil {
				t.Fatal("publish did not insert attempt")
			}
			if outcome == "advanced readiness failure" {
				if err != nil || !res.Published {
					t.Fatalf("successful publish failed on readiness: %+v %v", res, err)
				}
				if tip := f.runTip(t, f.oldRun); !tip.Valid || tip.String != lateTip {
					t.Fatalf("successful tip not accounted: %v", tip)
				}
				if a, aerr := f.e.q.GetCheckpointPublishAttempt(f.e.ctx, own); aerr == nil && a.ReconcileReadyAt.Valid {
					t.Fatal("failed readiness stamped")
				} else if aerr != nil && !errors.Is(aerr, pgx.ErrNoRows) {
					t.Fatal(aerr)
				}
			} else {
				if res.Published {
					t.Fatalf("pending outcome acquired ownership: %+v", res)
				}
				if outcome == "already current" && (err != nil || res.Skipped != "not_descendant") {
					t.Fatalf("unowned noop: %+v %v", res, err)
				}
				if outcome == "sentinel" && (err != nil || res.Skipped != "unsupported") {
					t.Fatalf("sentinel: %+v %v", res, err)
				}
				if outcome != "already current" && outcome != "sentinel" && err == nil {
					t.Fatal("broker error lost")
				}
				boundedLivePending(t, f, original)
				if outcome == "sentinel" || outcome == "already current" {
					if _, err := f.e.q.GetCheckpointPublishAttempt(f.e.ctx, own); !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("own cleanup: %v", err)
					}
				} else {
					boundedLivePending(t, f, own)
				}
			}
			if q.failMark {
				if q.markCalls != 1 || q.markContextErr != nil || q.markDeadline.IsZero() || time.Until(q.markDeadline) > retentionRecordTimeout {
					t.Fatalf("readiness context/calls: %+v", q)
				}
			} else if q.markCalls != 0 {
				t.Fatal("untyped outcome stamped ready")
			}
			if a := f.attemptRow(t, original); a.ReconcileReadyAt.Valid {
				t.Fatal("original pending stamped")
			}
			boundedLiveNoRetention(t, f)
		})
	}
}

func TestBoundedLivePendingInterleavingsLiveDB(t *testing.T) {
	for _, barrier := range []string{"after insert before base", "before unowned noop return", "readiness withdrawn after page"} {
		t.Run(barrier, func(t *testing.T) {
			f := newLiveAttemptFix(t)
			original := f.recordAttempt(t, lateTip)
			if barrier == "readiness withdrawn after page" {
				id := readyLiveAttempt(t, f)
				page, err := f.e.q.ListDueCheckpointPublishAttempts(f.e.ctx, store.ListDueCheckpointPublishAttemptsParams{OnlyRunID: pgconv.UUID(f.oldRun), MaxRows: 10})
				if err != nil || len(page) != 1 || page[0].ID != id {
					t.Fatalf("stale page: %+v %v", page, err)
				}
				f.e.exec(t, "UPDATE checkpoint_publish_attempts SET reconcile_ready_at=NULL WHERE id=$1", id)
				boundedLivePending(t, f, id)
				boundedLivePending(t, f, original)
				return
			}
			q := &boundedLiveStore{Store: f.e.q}
			var own uuid.UUID
			fired := false
			q.inserted = func(id uuid.UUID) {
				own = id
				if barrier == "after insert before base" {
					fired = true
					boundedLivePending(t, f, id)
					boundedLivePending(t, f, original)
				}
			}
			q.beforeDelete = func(id uuid.UUID) {
				if id != own {
					t.Fatal("cleanup targeted original evidence")
				}
				if barrier == "before unowned noop return" {
					fired = true
					boundedLivePending(t, f, id)
					boundedLivePending(t, f, original)
				}
			}
			f.svc1.q = q
			res, err := f.svc1.Publish(f.e.ctx, boundedLiveWorker(t, f), f.oldRun, lateTip, []byte("pack"))
			if !fired || err != nil || res.Published || res.Skipped != "not_descendant" {
				t.Fatalf("interleaving/noop: fired=%v result=%+v err=%v", fired, res, err)
			}
			if _, err := f.e.q.GetCheckpointPublishAttempt(f.e.ctx, own); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("new noop row not cleaned: %v", err)
			}
			boundedLivePending(t, f, original)
		})
	}
}

func TestBoundedLiveTerminalOrderingLiveDB(t *testing.T) {
	for _, order := range []string{"confirmation first", "terminal during list"} {
		t.Run(order, func(t *testing.T) {
			f := newLiveAttemptFix(t)
			id := readyLiveAttempt(t, f)
			terminal := func() {
				tx, err := f.e.pool.Begin(f.e.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(f.e.ctx) }()
				if _, err = tx.Exec(f.e.ctx, "UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", f.oldRun); err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(f.e.ctx); err != nil {
					t.Fatal(err)
				}
			}
			if order == "terminal during list" {
				f.svc1.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
					tips, err := f.forge.listRefTips(ctx, o, refs...)
					terminal()
					return tips, err
				})
			}
			done, settle, err := f.svc1.reconcilePublishAttemptLocked(f.e.ctx, id, func(context.Context) error { return nil })
			if err != nil || settle || done != (order == "confirmation first") {
				t.Fatalf("ordering: done=%v settle=%v err=%v", done, settle, err)
			}
			if order == "confirmation first" {
				assertLiveEvidence(t, f, id, lateTip, true)
				boundedLiveNoRetention(t, f)
				terminal()
				rec := f.row(t)
				if rec.Tip != lateTip || rec.Ref != f.branchRef || rec.State != retentionRetained {
					t.Fatalf("terminal did not bind confirmed tip: %+v", rec)
				}
				assertLiveEvidence(t, f, id, lateTip, true)
			} else {
				assertLiveEvidence(t, f, id, "", false)
				boundedLiveNoRetention(t, f)
			}
		})
	}
}
