package workersvc

import (
	"encoding/json"
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

func TestCodeCrossCheckDispositionJSONContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refused    bool
	}{
		{"finding_id", `{"finding_id":"F-1","disposition":"addressed","reason":"fixed"}`, false},
		{"legacy id", `{"id":"F-1","disposition":"addressed","reason":"fixed"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got CodeCrossCheckDisposition
			decoder := json.NewDecoder(strings.NewReader(tc.body))
			decoder.DisallowUnknownFields()
			err := decoder.Decode(&got)
			if tc.refused {
				if err == nil {
					t.Fatal("legacy disposition key accepted")
				}
				return
			}
			if err != nil || got.ID != "F-1" {
				t.Fatalf("finding_id request: %+v %v", got, err)
			}
			raw, err := json.Marshal(got)
			if err != nil || string(raw) != tc.body {
				t.Fatalf("persisted disposition: %s %v", raw, err)
			}
		})
	}
}

// Exercise the public service with a transaction double; the LiveDB tests
// separately prove PostgreSQL's write-once marker and custody predicates.
func TestCodeCrossCheckDispositionsBatchAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		batch     []CodeCrossCheckDisposition
		finalized bool
		change    func(*store.Run, *store.CrossCheck)
		refused   bool
	}{
		{name: "empty batch"},
		{name: "exact multibyte reason", batch: []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "declined", Reason: strings.Repeat("é", 512)}}},
		{name: "plus one byte", batch: []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "declined", Reason: strings.Repeat("é", 512) + "x"}}, refused: true},
		{name: "unknown", batch: []CodeCrossCheckDisposition{{ID: "unknown", Disposition: "declined", Reason: "verified"}}, refused: true},
		{name: "duplicate", batch: []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "declined", Reason: "verified"}, {ID: "F-1", Disposition: "addressed", Reason: "fixed"}}, refused: true},
		{name: "empty retry", finalized: true},
		{name: "stale retry", finalized: true, change: func(r *store.Run, _ *store.CrossCheck) { r.ClaimGeneration = 2 }, refused: true},
		{name: "released retry", finalized: true, change: func(r *store.Run, _ *store.CrossCheck) { r.ClaimReleasedAt.Valid = true }, refused: true},
		{name: "foreign worker retry", finalized: true, change: func(r *store.Run, _ *store.CrossCheck) { r.WorkerID = pgconv.UUID(uuid.New()) }, refused: true},
		{name: "foreign user retry", finalized: true, change: func(r *store.Run, _ *store.CrossCheck) { r.UserID = uuid.New() }, refused: true},
		{name: "interrupted retry", finalized: true, change: func(_ *store.Run, c *store.CrossCheck) { c.InterruptedAt.Valid = true }, refused: true},
		{name: "conflicting retry", finalized: true, batch: []CodeCrossCheckDisposition{{ID: "F-1", Disposition: "declined", Reason: "verified"}}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			lead := store.Run{ID: uuid.New(), UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: 1, Status: "running"}
			cc := store.CrossCheck{LeadRunID: lead.ID, LeadClaimGeneration: 1, Outcome: pgconv.Text("completed"), DecidedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, Findings: []byte(`[{"id":"F-1"},{"id":"F_2"}]`)}
			missing := `[{"finding_id":"F-1","disposition":"not_reported","reason":""},{"finding_id":"F_2","disposition":"not_reported","reason":""}]`
			if tc.finalized {
				cc.FinalizedAt = cc.DecidedAt
				cc.Dispositions = []byte(missing)
			}
			if tc.change != nil {
				tc.change(&lead, &cc)
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
			got, err := svc.FinalizeCodeCrossCheckDispositions(t.Context(), worker, lead.ID, 1, tc.batch)
			if tc.refused {
				if !errors.Is(err, ErrCrossCheckRefused) || writes != 0 || tx.committed {
					t.Fatalf("refusal err=%v writes=%d committed=%v", err, writes, tx.committed)
				}
				return
			}
			if err != nil || !got.FinalizedAt.Valid || !tx.committed {
				t.Fatalf("finalization: %+v %v", got, err)
			}
			wantWrites := 1
			if tc.finalized {
				wantWrites = 0
			}
			if writes != wantWrites {
				t.Fatalf("writes=%d want=%d", writes, wantWrites)
			}
			var ds []CodeCrossCheckDisposition
			if err := json.Unmarshal(got.Dispositions, &ds); err != nil || len(ds) != 2 {
				t.Fatalf("dispositions: %s %v", got.Dispositions, err)
			}
			if len(tc.batch) == 0 {
				if string(got.Dispositions) != missing {
					t.Fatalf("missing evidence: %s", got.Dispositions)
				}
			} else if ds[0] != tc.batch[0] || ds[1] != (CodeCrossCheckDisposition{ID: "F_2", Disposition: "not_reported"}) {
				t.Fatalf("batch: %+v", ds)
			}
		})
	}
}

func TestCodeCrossCheckDispositionsRejectUnsafeIDsBeforeDB(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", 65), "white space", "new\nline", "ansi\x1b[31m", "control\x00", "bidi\u202e"} {
		t.Run(id, func(t *testing.T) {
			db := &beginRecorder{begins: make(chan struct{}, 1)}
			svc := New(nil, nil, Params{})
			svc.SetTxBeginner(db)
			_, err := svc.FinalizeCodeCrossCheckDispositions(t.Context(), store.Worker{}, uuid.New(), 1, []CodeCrossCheckDisposition{{ID: id, Disposition: "declined", Reason: "verified"}})
			if !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("unsafe ID accepted: %v", err)
			}
			select {
			case <-db.begins:
				t.Fatal("unsafe ID reached database")
			default:
			}
		})
	}
}
