package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type queuedAdmissionProbeTx struct {
	pgx.Tx
	settingsLocks, refreshes int
}

func (tx *queuedAdmissionProbeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		tx.settingsLocks++
		return pgconn.CommandTag{}, errors.New("unexpected settings admission lock")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *queuedAdmissionProbeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "name: RefreshFrozenQueuedCustodyTargets ") {
		tx.refreshes++
		return scopedClaimRow{err: errors.New("unexpected queued custody refresh")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

type queuedAdmissionProbeBeginner struct{ tx pgx.Tx }

func (b queuedAdmissionProbeBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }

type queuedAdmissionRowsReader struct{}

func (queuedAdmissionRowsReader) CustodyHoldLimit(context.Context) (int32, error) { return 8, nil }
func (queuedAdmissionRowsReader) CustodyHoldLimitFromRows([]store.AppSetting) (int32, error) {
	return 8, nil
}

func TestQueuedCustodyNoAdmissionAuxiliaryQueries(t *testing.T) {
	for _, mode := range []string{"claim", "replace"} {
		t.Run(mode, func(t *testing.T) {
			svc, original, _ := scopedClaimFixture(t)
			probe := &queuedAdmissionProbeTx{Tx: original}
			svc.SetTxBeginner(queuedAdmissionProbeBeginner{tx: probe})
			snapshot := &ActiveSnapshot{RegisterNonce: "scoped", SnapshotEpoch: 1, Active: []ActiveRunEntry{}}
			if mode == "claim" {
				payload, err := svc.Claim(context.Background(), original.worker, snapshot)
				if err != nil || payload == nil {
					t.Fatalf("ordinary claim: payload=%v err=%v", payload != nil, err)
				}
			} else if applied, err := svc.ReplaceWorkerActiveRuns(context.Background(), store.New(probe), original.worker, snapshot, snapshotModeHeartbeat); err != nil || !applied {
				t.Fatalf("snapshot replacement: applied=%v err=%v", applied, err)
			}
			if probe.settingsLocks != 0 || probe.refreshes != 0 {
				t.Fatalf("no admission requested: settings locks=%d refreshes=%d", probe.settingsLocks, probe.refreshes)
			}
		})
	}
}

func TestQueuedCustodyDisabledAdmissionDoesNotLockSettings(t *testing.T) {
	for _, mode := range []string{"no_reader", "cache_only_reader", "zero_cache", "negative_cache", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			params := testParams()
			params.CustodyHoldLimit = queuedAdmissionRowsReader{}
			cached := int32(8)
			switch mode {
			case "no_reader":
				params.CustodyHoldLimit = nil
			case "cache_only_reader":
				params.CustodyHoldLimit = &fakeCustodyLimit{n: 8}
			case "zero_cache":
				cached = 0
			case "negative_cache":
				cached = -1
			}
			svc := New(&fakeStore{}, newBox(t), params)
			probe := &queuedAdmissionProbeTx{}
			limit, err := svc.queuedCustodyAdmission(context.Background(), probe, cached)
			if mode == "enabled" {
				if err == nil || probe.settingsLocks != 1 || limit != 0 {
					t.Fatalf("enabled admission must take settings lock first: limit=%d locks=%d err=%v", limit, probe.settingsLocks, err)
				}
			} else if err != nil || limit != 0 || probe.settingsLocks != 0 {
				t.Fatalf("disabled admission did DB work: limit=%d locks=%d err=%v", limit, probe.settingsLocks, err)
			}
		})
	}
}
