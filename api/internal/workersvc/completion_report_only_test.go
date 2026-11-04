package workersvc

import (
	"bytes"
	"encoding/json"
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

func TestReportOnlyOwnerDeferralUnion(t *testing.T) {
	frozen := frozenJSON(t, "m1", "m2")
	raw, err := buildCompletionContract(frozen)
	if err != nil {
		t.Fatal(err)
	}
	withScope := func(in []string, out ...string) []byte {
		var c completionContract
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		c.Scope = &scopeBlock{In: in}
		for _, id := range out {
			c.Scope.Out = append(c.Scope.Out, deferredEntry{MilestoneID: id, Reason: "later", Revision: 1})
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name      string
		contract  []byte
		completed []byte
		want      bool
	}{
		{"deferred m2, m1 done", withScope([]string{"m1"}, "m2"), idsJSON(t, "m1"), true},
		{"deferred m2, both done", withScope([]string{"m1"}, "m2"), idsJSON(t, "m1", "m2"), true},
		{"deferred m2, m1 missing", withScope([]string{"m1"}, "m2"), idsJSON(t), false},
		{"deferred m2, only deferred done", withScope([]string{"m1"}, "m2"), idsJSON(t, "m2"), false},
		{"everything deferred", withScope(nil, "m1", "m2"), idsJSON(t, "m1", "m2"), false},
		{"no scope, m2 missing", raw, idsJSON(t, "m1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{
				Kind: "issue", CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
				ContractRevision: pgtype.Int4{Int32: 1, Valid: true}, CompletionContract: tc.contract,
				MilestonesFrozen: frozen, MilestonesCompleted: tc.completed,
			}
			if _, valid := headlessCompletedUnion(run, reportOnlyRequest()); valid != tc.want {
				t.Fatalf("valid=%v want %v", valid, tc.want)
			}
		})
	}
}
