package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMilestoneReconciliationUsageGenerationGuardLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	worker := f.wkr
	worker.ProtocolCapabilities = nil
	for _, reclaimed := range []bool{false, true} {
		if reclaimed {
			claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
			if err != nil || claim == nil || claim.RunID != id.String() {
				t.Fatalf("same-worker reclaim=%+v err=%v", claim, err)
			}
		}
		for _, tc := range []struct {
			name string
			gen  *int64
			want error
		}{
			{"omitted", nil, ErrMissingClaimGeneration},
			{"zero", i64(0), ErrMissingClaimGeneration},
			{"negative", i64(-1), ErrMissingClaimGeneration},
			{"old", i64(1), ErrStaleClaim},
		} {
			t.Run(tc.name+map[bool]string{false: "/released", true: "/reclaimed"}[reclaimed], func(t *testing.T) {
				leg := uuid.New()
				req := UsageRequest{Legs: []UsageLegMarker{closed(leg, 1)}, Messages: []UsageRecord{um(leg, "old-flight", 1)}}
				if err := f.svc.RecordRunUsage(f.ctx, worker, id, req, tc.gen); !errors.Is(err, tc.want) {
					t.Fatalf("usage err=%v, want %v", err, tc.want)
				}
				for _, table := range []string{"run_usage_legs", "run_usage_messages", "run_usage_tail_state"} {
					var count int
					if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM "+table+" WHERE run_id=$1", id).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("%s has %d rows after rejected usage", table, count)
					}
				}
			})
		}
	}
	current := reconciliationRow(t, f, id).ClaimGeneration
	leg := uuid.New()
	if err := f.svc.RecordRunUsage(f.ctx, worker, id, UsageRequest{Messages: []UsageRecord{um(leg, "current-flight", 1)}}, &current); err != nil {
		t.Fatalf("current usage: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM run_usage_messages WHERE run_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("current usage count=%d err=%v", count, err)
	}
}

// The usage lock must exclude reconciliation's FOR UPDATE run lock before the
// audit read, while the existing afterFence tests permit ordinary release UPDATEs.
func TestMilestoneReconciliationUsageSerializesAuditLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	gen := int64(1)
	f.svc.usageAfterFenceHook = func() {
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		if _, err := tx.Exec(f.ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.New(tx).GetRunByIDForUpdate(f.ctx, id); err == nil {
			t.Fatal("reconciliation run lock succeeded during usage")
		} else {
			var pgErr interface{ SQLState() string }
			if !errors.As(err, &pgErr) || pgErr.SQLState() != "55P03" {
				t.Fatalf("reconciliation run lock err=%v, want lock timeout", err)
			}
		}
	}
	if err := f.svc.RecordRunUsage(f.ctx, f.wkr, id, UsageRequest{}, &gen); err != nil {
		t.Fatal(err)
	}
	f.svc.usageAfterFenceHook = nil
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatalf("reconciliation after usage commit: %v", err)
	}
}
