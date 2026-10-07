package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep the fixture's old run live, with no terminal trigger or retention call.
func newLiveAttemptFix(t *testing.T) *supersedeFix {
	t.Helper()
	f := newSupersedeFixWith(t, func(f *supersedeFix) { f.iid++ })
	f.e.exec(t, "UPDATE runs SET checkpoint_tip = NULL, checkpoint_tip_at = NULL WHERE id = $1", f.oldRun)
	f.forge.set(f.branchRef, lateTip)
	return f
}
func readyLiveAttempt(t *testing.T, f *supersedeFix) uuid.UUID {
	t.Helper()
	id := f.recordAttempt(t, lateTip)
	if n, err := f.e.q.MarkCheckpointPublishAttemptReady(f.e.ctx, id); err != nil || n != 1 {
		t.Fatalf("mark ready: rows=%d err=%v", n, err)
	}
	return id
}
func assertLiveEvidence(t *testing.T, f *supersedeFix, id uuid.UUID, tip string, consumed bool) {
	t.Helper()
	got := f.runTip(t, f.oldRun)
	if (tip != "") != got.Valid || got.String != tip {
		t.Fatalf("confirmed run tip=%v, want %q", got, tip)
	}
	_, err := f.e.q.GetCheckpointPublishAttempt(f.e.ctx, id)
	if consumed {
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("confirmed exact attempt must be consumed: %v", err)
		}
	} else if err != nil {
		t.Fatalf("attempt evidence lost: %v", err)
	}
	if gotTip, ok := f.forge.ref(f.branchRef); !ok || gotTip != lateTip {
		t.Fatalf("live reconciliation changed forge ref: %q %v", gotTip, ok)
	}
	f.forge.mu.Lock()
	defer f.forge.mu.Unlock()
	if len(f.forge.events) != 0 {
		t.Fatalf("live reconciliation wrote forge: %v", f.forge.events)
	}
}

func TestLiveReadyAttemptConfirmsWithoutRetentionLiveDB(t *testing.T) {
	for _, withRecord := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing untouched"}[withRecord], func(t *testing.T) {
			f := newLiveAttemptFix(t)
			if withRecord {
				f.e.exec(t, "INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state) VALUES($1,$2,$3,$4,$5,$6,'retained')", f.oldRun, f.e.userID, f.e.repoID, f.branch, f.branchRef, retentionTestTip)
			}
			before, berr := f.e.q.GetCheckpointRetention(f.e.ctx, f.oldRun)
			id := readyLiveAttempt(t, f)
			f.reconcileAttempts(t, f.svc1)
			assertLiveEvidence(t, f, id, lateTip, true)
			after, aerr := f.e.q.GetCheckpointRetention(f.e.ctx, f.oldRun)
			if !reflect.DeepEqual(before, after) || !errors.Is(aerr, berr) {
				t.Fatalf("live confirmation mutated retention: before=%+v err=%v after=%+v err=%v", before, berr, after, aerr)
			}
		})
	}
}

func TestLivePendingAttemptNeverConsumedLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	id := f.recordAttempt(t, lateTip)
	f.e.exec(t, "UPDATE checkpoint_publish_attempts SET attempted_at=now()-interval '30 days' WHERE id=$1", id)
	calls := 0
	f.svc1.SetListRefTipsFn(func(context.Context, pushbroker.ListRefsOptions, ...string) (map[string]string, error) {
		calls++
		return map[string]string{f.branchRef: lateTip}, nil
	})
	f.reconcileAttempts(t, f.svc1)
	// Also drive the locked reread, as if readiness changed after a stale candidate page.
	done, settle, err := f.svc1.reconcilePublishAttemptLocked(f.e.ctx, id, func(context.Context) error { return nil })
	if err != nil || done || settle || calls != 0 {
		t.Fatalf("pending live row touched: done=%v settle=%v err=%v lists=%d", done, settle, err, calls)
	}
	assertLiveEvidence(t, f, id, "", false)
	if a := f.attemptRow(t, id); a.Checks != 0 {
		t.Fatalf("pending live row deferred/aged: %+v", a)
	}
}

func TestLiveConfirmationRaceGuardsLiveDB(t *testing.T) {
	for _, name := range []string{
		"tip before list", "tip during list null base", "tip during list nonnull base", "timestamp null", "same tip",
		"other persisted", "other pending attempt", "other ready attempt", "other retention", "claim committed during list",
		"retention appears", "retention ref changes", "retention recovery changes", "retention state changes", "retention tip changes",
		"superseding", "handed on", "terminal during list", "no transaction", "fence lost", "list failure", "ref absent", "ref mismatch",
	} {
		t.Run(name, func(t *testing.T) {
			f := newLiveAttemptFix(t)
			hasRecord := name == "retention ref changes" || name == "retention recovery changes" || name == "retention state changes" || name == "retention tip changes" || name == "superseding" || name == "handed on"
			if hasRecord {
				f.e.exec(t, "INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state) VALUES($1,$2,$3,$4,$5,$6,'retained')", f.oldRun, f.e.userID, f.e.repoID, f.branch, f.branchRef, retentionTestTip)
			}
			if name == "tip during list nonnull base" {
				f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now()-interval '1 day' WHERE id=$1", f.oldRun, retentionTestTip)
			}
			id := readyLiveAttempt(t, f)
			wantTip := ""
			if name == "tip before list" {
				f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now()+interval '1 second' WHERE id=$1", f.oldRun, newerTip)
				wantTip = newerTip
			}
			if name == "timestamp null" {
				f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=NULL WHERE id=$1", f.oldRun, newerTip)
				wantTip = newerTip
			}
			if name == "same tip" {
				f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=NULL WHERE id=$1", f.oldRun, lateTip)
				wantTip = lateTip
			}
			if name == "superseding" {
				f.e.exec(t, "UPDATE checkpoint_retentions SET state='superseding',recovery_ref=$2 WHERE run_id=$1", f.oldRun, f.recoveryRef)
			}
			if name == "handed on" {
				f.handOnSlot(t, lateTip)
			}
			claim := func(kind string) {
				switch kind {
				case "persisted":
					f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", f.newRun, lateTip)
				case "pending attempt", "ready attempt":
					other, err := f.svc1.recordPublishAttempt(f.e.ctx, f.newRun, f.branch, f.branchRef, lateTip)
					if err != nil {
						t.Fatal(err)
					}
					if kind == "ready attempt" {
						if _, err := f.e.q.MarkCheckpointPublishAttemptReady(f.e.ctx, other); err != nil {
							t.Fatal(err)
						}
					}
				case "retention":
					f.e.exec(t, "INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state) VALUES($1,$2,$3,$4,$5,$6,'settling')", f.newRun, f.e.userID, f.e.repoID, f.branch, f.branchRef, lateTip)
				}
			}
			if name == "other persisted" {
				claim("persisted")
			}
			if name == "other pending attempt" {
				claim("pending attempt")
			}
			if name == "other ready attempt" {
				claim("ready attempt")
			}
			if name == "other retention" {
				claim("retention")
			}
			if name == "no transaction" {
				f.svc1.SetTxBeginner(nil)
			}
			fired := false
			f.svc1.SetListRefTipsFn(func(ctx context.Context, o pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
				tips, err := f.forge.listRefTips(ctx, o, refs...)
				if fired {
					t.Fatal("unexpected second list")
				}
				fired = true
				switch name {
				case "tip during list null base", "tip during list nonnull base":
					f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", f.oldRun, newerTip)
					wantTip = newerTip
				case "claim committed during list":
					claim("pending attempt")
				case "retention appears":
					f.e.exec(t, "INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state) VALUES($1,$2,$3,$4,$5,$6,'retained')", f.oldRun, f.e.userID, f.e.repoID, f.branch, f.branchRef, retentionTestTip)
				case "retention ref changes":
					f.e.exec(t, "UPDATE checkpoint_retentions SET ref=$2 WHERE run_id=$1", f.oldRun, "refs/changed")
				case "retention recovery changes":
					f.e.exec(t, "UPDATE checkpoint_retentions SET recovery_ref=$2 WHERE run_id=$1", f.oldRun, f.recoveryRef)
				case "retention state changes":
					f.e.exec(t, "UPDATE checkpoint_retentions SET state='settling' WHERE run_id=$1", f.oldRun)
				case "retention tip changes":
					f.e.exec(t, "UPDATE checkpoint_retentions SET tip=$2 WHERE run_id=$1", f.oldRun, newerTip)
				case "terminal during list":
					f.e.exec(t, "UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", f.oldRun)
				case "list failure":
					return nil, errInjected
				case "ref absent":
					return map[string]string{}, nil
				case "ref mismatch":
					return map[string]string{f.branchRef: newerTip}, nil
				}
				return tips, err
			})
			fence := func(context.Context) error {
				if name == "fence lost" {
					return ErrRetentionLockLost
				}
				return nil
			}
			done, settle, err := f.svc1.reconcilePublishAttemptLocked(f.e.ctx, id, fence)
			if name == "same tip" {
				if err != nil || !done || settle {
					t.Fatalf("same-tip confirmation: done=%v settle=%v err=%v", done, settle, err)
				}
				assertLiveEvidence(t, f, id, lateTip, true)
			} else {
				if done || settle {
					t.Fatalf("guard failed open: done=%v settle=%v err=%v", done, settle, err)
				}
				assertLiveEvidence(t, f, id, wantTip, false)
				if name == "fence lost" {
					if !errors.Is(err, ErrRetentionLockLost) || f.attemptRow(t, id).Checks != 0 {
						t.Fatalf("lost fence mutated backoff: err=%v attempt=%+v", err, f.attemptRow(t, id))
					}
				}
			}
			if !fired {
				t.Fatal("race barrier never reached")
			}
		})
	}
}

func TestLiveSameTipConfirmationPreservesTimestampLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	id := readyLiveAttempt(t, f)
	f.e.exec(t, "UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now()+interval '1 hour' WHERE id=$1", f.oldRun, lateTip)
	before, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatal(err)
	}
	f.reconcileAttempts(t, f.svc1)
	assertLiveEvidence(t, f, id, lateTip, true)
	after, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatal(err)
	}
	if after.CheckpointTipAt.Time.Before(before.CheckpointTipAt.Time) {
		t.Fatalf("confirmation regressed timestamp: %v -> %v", before.CheckpointTipAt, after.CheckpointTipAt)
	}
}

func TestLiveAttemptListingReadinessLiveDB(t *testing.T) {
	f := newLiveAttemptFix(t)
	pending := f.recordAttempt(t, lateTip)
	ready := readyLiveAttempt(t, f)
	first := f.attemptRow(t, ready).ReconcileReadyAt
	if n, err := f.e.q.MarkCheckpointPublishAttemptReady(f.e.ctx, ready); err != nil || n != 1 {
		t.Fatalf("idempotent ready: %d %v", n, err)
	}
	if got := f.attemptRow(t, ready).ReconcileReadyAt; got != first {
		t.Fatalf("readiness stamp moved: %v -> %v", first, got)
	}
	if n, err := f.e.q.MarkCheckpointPublishAttemptReady(f.e.ctx, uuid.New()); err != nil || n != 0 {
		t.Fatalf("unknown readiness ID creates evidence: %d %v", n, err)
	}
	list := func(run uuid.UUID) []store.CheckpointPublishAttempt {
		rows, err := f.e.q.ListDueCheckpointPublishAttempts(f.e.ctx, store.ListDueCheckpointPublishAttemptsParams{OnlyRunID: pgconv.UUID(run), Cooling: pgtype.Interval{Microseconds: time.Hour.Microseconds(), Valid: true}, MaxRows: 1})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := list(f.oldRun); len(rows) != 1 || rows[0].ID != ready {
		t.Fatalf("bounded live page=%v, want only ready ID %s (pending %s)", rows, ready, pending)
	}
	if rows := list(f.newRun); len(rows) != 0 {
		t.Fatalf("only_run_id leaked: %v", rows)
	}
	f.e.exec(t, "UPDATE checkpoint_publish_attempts SET next_check_at=now()+interval '1 hour' WHERE id=$1", ready)
	if rows := list(f.oldRun); len(rows) != 0 {
		t.Fatalf("backoff ignored: %v", rows)
	}
	f.e.exec(t, "UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", f.oldRun)
	if rows := list(f.oldRun); len(rows) != 0 {
		t.Fatalf("terminal cooling ignored: %v", rows)
	}
	f.e.exec(t, "UPDATE runs SET status_since=now()-interval '2 hours' WHERE id=$1", f.oldRun)
	if rows := list(f.oldRun); len(rows) != 1 || rows[0].ID != pending {
		t.Fatalf("legacy terminal pending evidence excluded: %v", rows)
	}
}
