package workersvc

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only locked reads are implemented: a reconciliation path that attempts any
// mutation fails this fixture. Real SQL and locking are covered by LiveDB tests.
type policyReplayTx struct {
	pgx.Tx
	worker store.Worker
	run    store.Run
	reads  []string
}

type policyReplayBeginner struct{ tx *policyReplayTx }

func (b policyReplayBeginner) Begin(context.Context) (pgx.Tx, error) { return b.tx, nil }
func (*policyReplayTx) Rollback(context.Context) error               { return nil }
func (*policyReplayTx) Commit(context.Context) error {
	return fmt.Errorf("receipt reconciliation must not commit")
}
func (*policyReplayTx) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if query != "SET TRANSACTION ISOLATION LEVEL READ COMMITTED" {
		return pgconn.CommandTag{}, fmt.Errorf("unexpected reconciliation statement: %s", query)
	}
	return pgconn.NewCommandTag("SET"), nil
}
func (tx *policyReplayTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	name := strings.Fields(query)[2]
	tx.reads = append(tx.reads, name)
	switch name {
	case "GetWorkerForUpdate":
		return policyReplayRow{value: tx.worker}
	case "GetRunByIDForUpdate":
		return policyReplayRow{value: tx.run}
	default:
		return policyReplayRow{err: fmt.Errorf("unexpected reconciliation query: %s", name)}
	}
}

type policyReplayRow struct {
	value any
	err   error
}

func (row policyReplayRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	value := reflect.ValueOf(row.value)
	if value.NumField() != len(dest) {
		return fmt.Errorf("policy replay fixture scan shape changed")
	}
	for i, target := range dest {
		reflect.ValueOf(target).Elem().Set(value.Field(i))
	}
	return nil
}

func TestPolicyHoldConvertedJournalReplayReceiptOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*policyReplayTx, *PolicyHoldRequest)
		disposition string
		reason      string
	}{
		{"original journal", func(_ *policyReplayTx, _ *PolicyHoldRequest) {}, "already_applied", ""},
		{"conversion envelope", func(_ *policyReplayTx, r *PolicyHoldRequest) {
			r.ClaimGeneration = 2
			r.Detail = "resume_rejected"
			r.PolicyTag, r.Phase, r.CorrelationID = nil, nil, nil
		}, "already_applied", ""},
		{"unrecorded worker", func(tx *policyReplayTx, _ *PolicyHoldRequest) {
			tx.run.PolicyApplyingWorkerID = pgconv.UUID(uuid.New())
		}, "pending", "ownership_unconfirmed"},
		{"different outcome", func(_ *policyReplayTx, r *PolicyHoldRequest) {
			r.OutcomeID = uuid.NewString()
		}, "pending", "ownership_unconfirmed"},
		{"different source", func(_ *policyReplayTx, r *PolicyHoldRequest) {
			r.SourceGeneration, r.ClaimGeneration = 2, 2
		}, "pending", "ownership_unconfirmed"},
		{"unrecorded generation", func(_ *policyReplayTx, r *PolicyHoldRequest) {
			r.ClaimGeneration = 3
		}, "pending", "ownership_unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := policyHoldFixture()
			worker := store.Worker{ID: uuid.New(), UserID: uuid.New(),
				ProtocolCapabilities: []string{capability.ProviderPolicyHoldV1}}
			run := store.Run{ID: uuid.New(), UserID: worker.UserID, WorkerID: pgconv.UUID(uuid.New()),
				Status: "running", ClaimGeneration: 3, SessionID: pgconv.Text("owner-selected-session"),
				PolicyOutcomeID: pgconv.UUID(uuid.MustParse(req.OutcomeID)), PolicySourceGeneration: pgtype.Int8{Int64: 1, Valid: true},
				PolicyApplyingWorkerID: pgconv.UUID(worker.ID), PolicyApplyingGeneration: pgtype.Int8{Int64: 2, Valid: true}}
			tx := &policyReplayTx{worker: worker, run: run}
			tc.change(tx, &req)
			before := tx.run
			svc := &Service{txBeginner: policyReplayBeginner{tx}}
			got, err := svc.ReportPolicyHold(context.Background(), worker, run.ID, req)
			if err != nil || got.Disposition != tc.disposition || got.Reason != tc.reason {
				t.Fatalf("converted replay: %+v err=%v; want %s/%s", got, err, tc.disposition, tc.reason)
			}
			if got.Disposition == "already_applied" && (got.ObservedStatus != "running" || got.ObservedGeneration != 3) {
				t.Fatalf("receipt did not describe current row: %+v", got)
			}
			if !reflect.DeepEqual(before, tx.run) || !reflect.DeepEqual(tx.reads, []string{"GetWorkerForUpdate", "GetRunByIDForUpdate"}) {
				t.Fatalf("reconciliation mutated or used unexpected queries: %v", tx.reads)
			}
		})
	}
}
