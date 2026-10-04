package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func TestPromoteVaultLockedRecoveryWaitRunsGuards(t *testing.T) {
	for _, mode := range []string{"missing", "locked", "foreign-unlocked", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			uid := uuid.New()
			box := newBox(t)
			calls := 0
			fs := &fakeStore{promoteVaultLocked: func(context.Context, uuid.UUID) ([]store.PromoteVaultLockedRecoveryWaitRunsRow, error) {
				calls++
				return nil, nil
			}}
			svc := New(fs, box, testParams())
			ctx := context.Background()
			var want error
			switch mode {
			case "locked":
				svc.SetVault(vault.New(box, newMemVaultStore()))
			case "foreign-unlocked":
				svc.SetVault(unlockedVault(t, uuid.New(), box))
			case "cancelled", "expired":
				svc.SetVault(unlockedVault(t, uid, box))
				var cancel context.CancelFunc
				if mode == "expired" {
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					want = context.Canceled
				}
				defer cancel()
			}
			bc, lc := &parkBroadcaster{}, &fakeLifecycle{}
			svc.SetBroadcaster(bc)
			svc.SetLifecycle(lc)
			err := svc.PromoteVaultLockedRecoveryWaitRuns(ctx, uid)
			if !errors.Is(err, want) || calls != 0 || len(bc.states) != 0 || len(lc.notes) != 0 {
				t.Fatalf("err=%v want=%v calls=%d states=%v notes=%v", err, want, calls, bc.states, lc.notes)
			}
		})
	}
}

func TestPromoteVaultLockedRecoveryWaitRunsQueryPublication(t *testing.T) {
	for _, mode := range []string{"success", "empty", "error", "cancelled-query"} {
		t.Run(mode, func(t *testing.T) {
			uid := uuid.New()
			box := newBox(t)
			bc, lc := &parkBroadcaster{}, &fakeLifecycle{}
			rows := []store.PromoteVaultLockedRecoveryWaitRunsRow{
				{ID: uuid.New(), UserID: uid, Status: "queued"},
				{ID: uuid.New(), UserID: uid, Status: "queued"},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var queryCtx context.Context
			var want error
			calls := 0
			fs := &fakeStore{promoteVaultLocked: func(qctx context.Context, owner uuid.UUID) ([]store.PromoteVaultLockedRecoveryWaitRunsRow, error) {
				calls++
				queryCtx = qctx
				if owner != uid {
					t.Fatalf("query owner=%s want=%s", owner, uid)
				}
				deadline, ok := qctx.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 5*time.Second {
					t.Fatalf("query deadline=%v ok=%v", deadline, ok)
				}
				if len(bc.states) != 0 || len(lc.notes) != 0 {
					t.Fatal("publication occurred before the query completed")
				}
				switch mode {
				case "empty":
					return nil, nil
				case "error":
					want = errors.New("database unavailable")
					return rows, want // even partial rows must not publish on error
				case "cancelled-query":
					cancel()
					<-qctx.Done()
					want = context.Canceled
					return rows, qctx.Err()
				}
				return rows, nil
			}}
			svc := New(fs, box, testParams())
			svc.SetVault(unlockedVault(t, uid, box))
			svc.SetBroadcaster(bc)
			svc.SetLifecycle(lc)
			err := svc.PromoteVaultLockedRecoveryWaitRuns(ctx, uid)
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("err=%v want=%v calls=%d", err, want, calls)
			}
			if !errors.Is(queryCtx.Err(), context.Canceled) {
				t.Fatalf("child context was not cancelled on return: %v", queryCtx.Err())
			}
			if mode != "success" {
				if len(bc.states) != 0 || len(lc.notes) != 0 {
					t.Fatalf("unsuccessful/empty query published states=%v notes=%v", bc.states, lc.notes)
				}
				return
			}
			if len(bc.states) != len(rows) || len(lc.notes) != len(rows) {
				t.Fatalf("states=%v notes=%v, want one of each for each returned row", bc.states, lc.notes)
			}
			for i, row := range rows {
				if !bc.sawState(row.ID, row.Status) || lc.notes[i].runID != row.ID || lc.notes[i].status != row.Status {
					t.Fatalf("missing publication for %+v: states=%v notes=%v", row, bc.states, lc.notes)
				}
			}
		})
	}
}
