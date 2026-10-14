package workersvc

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These corrupt persisted shapes are prohibited by PostgreSQL CHECK constraints.
// Exercise the helper directly too, so its fail-closed checks remain independently proved.
func TestReportOnlyMalformedArrays(t *testing.T) {
	frozen := frozenJSON(t, "m1")
	contract, err := buildCompletionContract(frozen)
	if err != nil {
		t.Fatal(err)
	}
	base := store.Run{
		Kind: "issue", CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
		ContractRevision: pgtype.Int4{Int32: 1, Valid: true}, CompletionContract: contract,
		MilestonesFrozen: frozen, MilestonesCompleted: idsJSON(t, "m1"),
	}
	if _, valid := headlessCompletedUnion(base, reportOnlyRequest()); !valid {
		t.Fatal("valid control was refused")
	}
	for _, tc := range []struct{ name, frozen, completed string }{
		{name: "JSON-null frozen", frozen: "null"},
		{name: "object frozen", frozen: "{}"},
		{name: "JSON-null completed", completed: "null"},
		{name: "object completed", completed: "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := base
			if tc.frozen != "" {
				run.MilestonesFrozen = []byte(tc.frozen)
			}
			if tc.completed != "" {
				run.MilestonesCompleted = []byte(tc.completed)
			}
			if _, valid := headlessCompletedUnion(run, reportOnlyRequest()); valid {
				t.Fatal("malformed persisted array authorized delivery")
			}
		})
	}
}

func assertReportOnlyFixtureConstraint(t *testing.T, e interlockLiveDB, id uuid.UUID, query string) {
	t.Helper()
	before := reportOnlySnapshot(t, e, id)
	_, err := e.pool.Exec(e.ctx, query, id)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatalf("expected array CHECK refusal, got %v", err)
	}
	if !bytes.Equal(before, reportOnlySnapshot(t, e, id)) {
		t.Fatal("rejected malformed array changed run")
	}
}
