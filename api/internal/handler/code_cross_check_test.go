package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type codeStatusTx struct {
	pgx.Tx
	lead  store.Run
	check store.CrossCheck
}

func (tx *codeStatusTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *codeStatusTx) Commit(context.Context) error          { return nil }
func (tx *codeStatusTx) Rollback(context.Context) error        { return nil }
func (tx *codeStatusTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.HasPrefix(sql, "-- name: GetRunOwnedByWorkerForUpdate"):
		return codeStatusRow{value: tx.lead}
	case strings.HasPrefix(sql, "-- name: GetCodeCrossCheck"):
		return codeStatusRow{value: tx.check}
	default:
		return codeStatusRow{err: pgx.ErrNoRows}
	}
}

type codeStatusRow struct {
	value any
	err   error
}

func (r codeStatusRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	value := reflect.ValueOf(r.value)
	for i, target := range dest {
		reflect.ValueOf(target).Elem().Set(value.Field(i))
	}
	return nil
}

func TestCodeCrossCheckWorkerInterruptedProjection(t *testing.T) {
	for _, reason := range []string{"snapshot_failed", "worker_unsupported", "superseded"} {
		t.Run(reason, func(t *testing.T) {
			worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
			tx := &codeStatusTx{lead: store.Run{ID: uuid.New(), UserID: worker.UserID, Status: "running", ClaimGeneration: 2},
				check: store.CrossCheck{Stage: "code", Round: 1, LeadClaimGeneration: 1, Outcome: pgtype.Text{String: "failed", Valid: true},
					ReasonClass: pgtype.Text{String: reason, Valid: true}, InterruptedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
					Findings: []byte(`[{"id":"F-1"}]`), Dispositions: []byte(`[{"finding_id":"F-1","disposition":"declined","reason":"verified"}]`)}}
			if reason == "superseded" {
				tx.check.HeadCommit = pgtype.Text{String: strings.Repeat("a", 40), Valid: true}
				tx.check.BaseCommit = tx.check.HeadCommit
			}
			svc := workersvc.New(nil, nil, workersvc.Params{})
			svc.SetTxBeginner(tx)
			h := &Handler{wsvc: svc}
			router := chi.NewRouter()
			router.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(mw.ContextWithWorker(r.Context(), worker)))
				})
			})
			router.Get("/runs/{id}/code", h.WorkerCodeCrossCheckStatus)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest("GET", "/runs/"+tx.lead.ID.String()+"/code?claim_generation=2", nil))
			var got apitypes.CodeCrossCheck
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ReasonClass == nil || *got.ReasonClass != reason || got.InterruptedAt == nil || string(got.Findings) != "[]" || string(got.Dispositions) != "null" {
				t.Fatalf("worker projection: %d %s", rec.Code, rec.Body.String())
			}
			if reason != "superseded" && (got.HeadCommit != nil || got.BaseCommit != nil) {
				t.Fatal("invented snapshot")
			}
			if string(tx.check.Findings) != `[{"id":"F-1"}]` || string(tx.check.Dispositions) != `[{"finding_id":"F-1","disposition":"declined","reason":"verified"}]` {
				t.Fatal("worker projection changed durable evidence")
			}
		})
	}
}
