package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests exercise the public service custody check; the transaction double
// does not evaluate SQL. TestCodeCrossCheckInteractiveFinalizationLiveDB covers
// the database predicates after query generation.
func TestCodeCrossCheckInteractiveDispositionCustody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*store.Run)
		refused bool
	}{
		{name: "waiting interactive task"},
		{name: "claimed", change: func(r *store.Run) { r.Status = "claimed" }},
		{name: "running", change: func(r *store.Run) { r.Status = "running" }},
		{name: "noninteractive", change: func(r *store.Run) { r.Interactive = false }, refused: true},
		{name: "other kind", change: func(r *store.Run) { r.Kind = "issue" }, refused: true},
		{name: "other wait", change: func(r *store.Run) { r.Status = "awaiting_input" }, refused: true},
		{name: "stale", change: func(r *store.Run) { r.ClaimGeneration = 2 }, refused: true},
		{name: "released", change: func(r *store.Run) { r.ClaimReleasedAt.Valid = true }, refused: true},
		{name: "foreign worker", change: func(r *store.Run) { r.WorkerID = pgconv.UUID(uuid.New()) }, refused: true},
		{name: "foreign user", change: func(r *store.Run) { r.UserID = uuid.New() }, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			lead := store.Run{ID: uuid.New(), UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID),
				ClaimGeneration: 1, Status: "awaiting_followup", Kind: "task", Interactive: true}
			cc := store.CrossCheck{LeadRunID: lead.ID, LeadClaimGeneration: 1, Outcome: pgconv.Text("completed"),
				DecidedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, Findings: []byte(`[{"id":"F-1"}]`)}
			if tc.change != nil {
				tc.change(&lead)
			}
			writes := 0
			tx := &crossCheckBoundaryTx{t: t}
			tx.query = func(sql string, args []any) pgx.Row {
				switch {
				case strings.HasPrefix(sql, "-- name: GetRunOwnedByWorkerForUpdate"):
					if lead.WorkerID != pgconv.UUID(worker.ID) {
						return crossCheckBoundaryRow{err: pgx.ErrNoRows}
					}
					return crossCheckBoundaryRow{value: lead}
				case strings.HasPrefix(sql, "-- name: GetCodeCrossCheck"):
					return crossCheckBoundaryRow{value: cc}
				case strings.HasPrefix(sql, "-- name: FinalizeCodeCrossCheckDispositions"):
					writes++
					cc.Dispositions = args[0].([]byte)
					cc.FinalizedAt = cc.DecidedAt
					return crossCheckBoundaryRow{value: cc}
				default:
					t.Fatalf("unexpected query: %s", sql)
					return crossCheckBoundaryRow{err: pgx.ErrNoRows}
				}
			}
			svc := New(nil, nil, Params{})
			svc.SetTxBeginner(tx)
			got, err := svc.FinalizeCodeCrossCheckDispositions(t.Context(), worker, lead.ID, 1,
				[]CodeCrossCheckDisposition{{ID: "F-1", Disposition: "addressed", Reason: "verified"}})
			if tc.refused {
				if !errors.Is(err, ErrCrossCheckRefused) || writes != 0 || tx.committed {
					t.Fatalf("refusal: err=%v writes=%d committed=%v", err, writes, tx.committed)
				}
			} else if err != nil || writes != 1 || !tx.committed || !got.FinalizedAt.Valid ||
				string(got.Dispositions) != `[{"finding_id":"F-1","disposition":"addressed","reason":"verified"}]` {
				t.Fatalf("custody finalization: %+v err=%v writes=%d", got, err, writes)
			}
		})
	}
}
