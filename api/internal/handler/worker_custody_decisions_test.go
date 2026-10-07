package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type custodyListStore struct {
	workersvc.Store
	ownerRows []store.ListWorkersByUserRow
	adminRows []store.ListAllWorkersRow
	owner     uuid.UUID
}

func (s *custodyListStore) ListWorkersByUser(_ context.Context, owner uuid.UUID) ([]store.ListWorkersByUserRow, error) {
	s.owner = owner
	return s.ownerRows, nil
}
func (s *custodyListStore) ListAllWorkers(context.Context) ([]store.ListAllWorkersRow, error) {
	return s.adminRows, nil
}

type custodyBatchStore struct {
	recovery.Store
	ids   []uuid.UUID
	calls int
	rows  []store.ListOpenCustodyHoldsForWorkersRow
	err   error
}

func (s *custodyBatchStore) ListOpenCustodyHoldsForWorkers(_ context.Context, ids []uuid.UUID) ([]store.ListOpenCustodyHoldsForWorkersRow, error) {
	s.calls++
	s.ids = ids
	return s.rows, s.err
}
func TestWorkerListsCustodyDecisions(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			for _, empty := range []bool{false, true} {
				t.Run(fmtCustodyCase(admin, fail, empty), func(t *testing.T) {
					owner, a, b := uuid.New(), uuid.New(), uuid.New()
					st := &custodyListStore{}
					ids := []uuid.UUID{}
					if !empty {
						ids = []uuid.UUID{a, b}
						st.ownerRows = []store.ListWorkersByUserRow{{ID: a}, {ID: b}}
						st.adminRows = []store.ListAllWorkersRow{{Worker: store.Worker{ID: a}}, {Worker: store.Worker{ID: b}}}
					}
					batch := &custodyBatchStore{rows: []store.ListOpenCustodyHoldsForWorkersRow{{WorkerID: a, State: "open", RunStatus: "failed"}}}
					if fail {
						batch.err = errors.New("partial batch unavailable")
					}
					h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{}), recoverySvc: recovery.New(batch, nil, nil, recovery.Limits{}, nil)}
					req := httptest.NewRequest(http.MethodGet, "/api/workers", nil).WithContext(mw.ContextWithUser(context.Background(), store.User{ID: owner}))
					rec := httptest.NewRecorder()
					if admin {
						h.AdminListWorkers(rec, req)
					} else {
						h.ListWorkers(rec, req)
						if st.owner != owner {
							t.Fatal("owner boundary moved")
						}
					}
					rows := custodyListJSON(t, rec)
					if len(rows) != len(ids) {
						t.Fatalf("rows=%v", rows)
					}
					if empty {
						if batch.calls != 0 {
							t.Fatal("empty list queried batch")
						}
						return
					}
					if batch.calls != 1 || !reflect.DeepEqual(batch.ids, ids) {
						t.Fatalf("batch calls=%d ids=%v", batch.calls, batch.ids)
					}
					for i, row := range rows {
						count, ok := row["custody_decisions_needed"]
						if fail {
							if ok {
								t.Fatalf("partial count leaked: %v", row)
							}
							continue
						}
						want := float64(0)
						if i == 0 {
							want = 1
						}
						if !ok || count != want {
							t.Fatalf("count=%v present=%v want=%v", count, ok, want)
						}
					}
				})
			}
		}
	}
}
func TestWorkerListsCustodyDecisionsWithoutHolds(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, available := range []bool{false, true} {
			name := fmtCustodyCase(admin, false, false) + "/no-store"
			if available {
				name = fmtCustodyCase(admin, false, false) + "/no-holds"
			}
			t.Run(name, func(t *testing.T) {
				id := uuid.New()
				st := &custodyListStore{
					ownerRows: []store.ListWorkersByUserRow{{ID: id}},
					adminRows: []store.ListAllWorkersRow{{Worker: store.Worker{ID: id}}},
				}
				h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
				batch := &custodyBatchStore{}
				if available {
					h.recoverySvc = recovery.New(batch, nil, nil, recovery.Limits{}, nil)
				}
				req := httptest.NewRequest(http.MethodGet, "/api/workers", nil).WithContext(mw.ContextWithUser(context.Background(), store.User{ID: uuid.New()}))
				rec := httptest.NewRecorder()
				if admin {
					h.AdminListWorkers(rec, req)
				} else {
					h.ListWorkers(rec, req)
				}
				rows := custodyListJSON(t, rec)
				if len(rows) != 1 {
					t.Fatalf("rows=%v", rows)
				}
				count, present := rows[0]["custody_decisions_needed"]
				if available {
					if !present || count != float64(0) || batch.calls != 1 {
						t.Fatalf("empty successful read: count=%v present=%v calls=%d", count, present, batch.calls)
					}
				} else if present || h.recoverySvc != nil {
					t.Fatalf("missing store: count=%v present=%v recovery service=%v", count, present, h.recoverySvc)
				}
			})
		}
	}
}

func fmtCustodyCase(admin, fail, empty bool) string {
	s := "owner"
	if admin {
		s = "admin"
	}
	if fail {
		s += "/failure"
	} else {
		s += "/success"
	}
	if empty {
		s += "/empty"
	}
	return s
}
func custodyListJSON(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	raw, ok := response["workers"]
	if !ok || string(raw) == "null" {
		t.Fatalf("workers list absent: %s", rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
