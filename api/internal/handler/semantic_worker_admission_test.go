package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type semanticAdmissionStore struct {
	workersvc.Store
	run    store.Run
	worker uuid.UUID
}

func (s *semanticAdmissionStore) GetRunOwnedByWorker(_ context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error) {
	if s.worker != uuid.Nil && arg.WorkerID.Bytes != s.worker {
		return store.Run{}, pgx.ErrNoRows
	}
	run := s.run
	run.ID = arg.ID
	return run, nil
}

type semanticAdmissionTx struct {
	pgx.Tx
	enrolled bool
}

func (tx *semanticAdmissionTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *semanticAdmissionTx) Rollback(context.Context) error        { return nil }
func (tx *semanticAdmissionTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if sql != "SET TRANSACTION ISOLATION LEVEL READ COMMITTED" {
		return pgconn.CommandTag{}, errors.New("unexpected statement")
	}
	return pgconn.CommandTag{}, nil
}
func (tx *semanticAdmissionTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if !strings.HasPrefix(sql, "-- name: GetSemanticObservations") {
		panic("unexpected admission query")
	}
	return semanticAdmissionRow{enrolled: tx.enrolled}
}

type semanticAdmissionRow struct{ enrolled bool }

func (row semanticAdmissionRow) Scan(dst ...any) error {
	if !row.enrolled {
		return pgx.ErrNoRows
	}
	*dst[0].(*[]byte) = []byte(`{}`)
	return nil
}

func semanticBodyHandler(enrolled bool) (*Handler, *semanticAdmissionStore) {
	st := &semanticAdmissionStore{run: store.Run{Status: "running", ClaimGeneration: 1}}
	svc := workersvc.New(st, nil, workersvc.Params{})
	svc.SetTxBeginner(&semanticAdmissionTx{enrolled: enrolled})
	return &Handler{wsvc: svc}, st
}

type semanticReadProbe struct{ reads int }

func (p *semanticReadProbe) Read([]byte) (int, error) {
	p.reads++
	return 0, errors.New("body must not be read")
}
func (*semanticReadProbe) Close() error { return nil }

func TestSemanticWorkerWritesFenceBeforeBody(t *testing.T) {
	for _, operation := range []string{"candidate", "reserve", "settle", "publish"} {
		for _, mode := range []string{"foreign", "unenrolled", "released", "terminal", "paused", "zero generation", "admitted"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				h, st := semanticBodyHandler(mode != "unenrolled")
				worker := store.Worker{ID: uuid.New()}
				st.worker = worker.ID
				switch mode {
				case "foreign":
					worker.ID = uuid.New()
				case "released":
					st.run.ClaimReleasedAt = pgtype.Timestamptz{Valid: true}
				case "terminal":
					st.run.Status = "completed"
				case "paused":
					st.run.Status = "paused"
				case "zero generation":
					st.run.ClaimGeneration = 0
				}
				body := &semanticReadProbe{}
				req := httptest.NewRequest(http.MethodPost, "/", body)
				route := chi.NewRouteContext()
				route.URLParams.Add("id", uuid.NewString())
				req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), worker))
				rec := httptest.NewRecorder()
				calls := map[string]http.HandlerFunc{"candidate": h.WorkerSemanticCandidate, "reserve": h.WorkerSemanticCoverageReserve, "settle": h.WorkerSemanticCoverageSettle, "publish": h.WorkerSemanticCoveragePublish}
				calls[operation](rec, req)
				if mode == "admitted" {
					if rec.Code != http.StatusBadRequest || body.reads == 0 {
						t.Fatalf("admitted request must reach bounded decoder: status=%d reads=%d", rec.Code, body.reads)
					}
				} else if rec.Code != http.StatusConflict || body.reads != 0 {
					t.Fatalf("request must be fenced before reading: status=%d reads=%d", rec.Code, body.reads)
				}
			})
		}
	}
}

func TestSemanticWorkerWritesUseWorkerLimiter(t *testing.T) {
	ls := newProbeLimiters()
	h := &Handler{}
	routes := h.Routes(ls[0], ls[1], ls[2], ls[3], ls[4], ls[5], ls[6], ls[7], ls[8], ls[9], ls[10]).(chi.Routes)
	want := map[string]bool{}
	for _, suffix := range []string{"candidate", "coverage/reserve", "coverage/settle", "coverage/publish"} {
		want["/api/worker/runs/{id}/semantic/"+suffix] = false
	}
	budget := 0
	for i, name := range limiterNames {
		if name == limProposal {
			budget = i + 1
		}
	}
	if budget == 0 {
		t.Fatal("proposal limiter not found")
	}
	p := &prober{}
	err := chi.Walk(routes, func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		if _, exists := want[pattern]; !exists || method != http.MethodPost {
			return nil
		}
		for _, m := range mws {
			got, perUser := p.probe(m)
			if got == 0 {
				continue
			}
			if got != budget || perUser {
				t.Fatalf("%s has wrong limiter: budget=%d perUser=%v", pattern, got, perUser)
			}
			chain := m(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(probeSentinel) }))
			key := "/semantic-worker-probe/" + uuid.NewString()
			call := func(worker uuid.UUID) int {
				r := httptest.NewRequest(http.MethodPost, key, nil)
				route := chi.NewRouteContext()
				route.RoutePatterns = []string{key}
				r = r.WithContext(mw.ContextWithWorker(context.WithValue(r.Context(), chi.RouteCtxKey, route), store.Worker{ID: worker}))
				w := httptest.NewRecorder()
				chain.ServeHTTP(w, r)
				return w.Code
			}
			worker := uuid.New()
			for range budget {
				if code := call(worker); code != probeSentinel {
					t.Fatalf("%s rejected before budget: %d", pattern, code)
				}
			}
			if call(worker) != http.StatusTooManyRequests || call(uuid.New()) != probeSentinel {
				t.Fatalf("%s must use independent per-worker buckets", pattern)
			}
			want[pattern] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for pattern, seen := range want {
		if !seen {
			t.Errorf("%s has no worker proposal limiter", pattern)
		}
	}
}
