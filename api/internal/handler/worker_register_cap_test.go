package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// capCaptureStore is a register-path workersvc.Store that records the
// max_concurrent_runs RegisterWorker was asked to persist.
type capCaptureStore struct {
	registerStore
	got   pgtype.Int4
	calls int
}

func (c *capCaptureStore) RegisterWorker(ctx context.Context, arg store.RegisterWorkerParams) (store.RegisterWorkerRow, error) {
	c.got = arg.MaxConcurrentRuns
	c.calls++
	return c.registerStore.RegisterWorker(ctx, arg)
}

// Issue #1624: an ephemeral (run-bound) worker can claim only its bound run (PRD #529
// Decision 4), so the server records its cap as 1 whatever it advertises; a persistent
// worker keeps its advertised cap (or NULL when it sends none).
func TestRegisterClampsEphemeralWorkerCapToOne(t *testing.T) {
	cases := []struct {
		name      string
		ephemeral bool
		body      string
		want      pgtype.Int4
	}{
		{"ephemeral advertising 2", true, `{"version":"1.0.0","max_concurrent_runs":2}`, pgtype.Int4{Int32: 1, Valid: true}},
		{"ephemeral advertising nothing", true, `{"version":"1.0.0"}`, pgtype.Int4{Int32: 1, Valid: true}},
		{"ephemeral advertising out-of-range 0", true, `{"version":"1.0.0","max_concurrent_runs":0}`, pgtype.Int4{Int32: 1, Valid: true}},
		{"persistent advertising 2", false, `{"version":"1.0.0","max_concurrent_runs":2}`, pgtype.Int4{Int32: 2, Valid: true}},
		{"persistent advertising nothing", false, `{"version":"1.0.0"}`, pgtype.Int4{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &capCaptureStore{registerStore: registerStore{kind: "hosted"}}
			h := &Handler{wsvc: workersvc.New(st, newHandlerTestBox(t), workersvc.Params{})}

			req := httptest.NewRequest(http.MethodPost, "/api/worker/register", strings.NewReader(tc.body))
			wkr := store.Worker{ID: uuid.New(), UserID: uuid.New(), TokenHash: []byte("h"), Kind: "hosted", Ephemeral: tc.ephemeral}
			req = req.WithContext(mw.ContextWithWorker(req.Context(), wkr))

			rec := httptest.NewRecorder()
			h.WorkerRegister(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			if st.calls != 1 {
				t.Fatalf("RegisterWorker calls = %d, want 1", st.calls)
			}
			if st.got != tc.want {
				t.Fatalf("stored max_concurrent_runs = %+v, want %+v", st.got, tc.want)
			}
		})
	}
}
