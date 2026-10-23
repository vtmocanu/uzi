package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// holdStore stubs the SetRunCompletionHold query: held is returned when holdErr is nil, otherwise
// holdErr (pgx.ErrNoRows = the hold's guard refused). The inherited ownedRun/ownedErr feed the
// service's re-read of the run on a refused hold.
type holdStore struct {
	protocolStore
	held    store.Run
	holdErr error
	calls   int
}

func (p *holdStore) SetRunCompletionHold(context.Context, store.SetRunCompletionHoldParams) (store.Run, error) {
	p.calls++
	return p.held, p.holdErr
}

func TestWorkerRunCompletionHoldHTTPMapping(t *testing.T) {
	id := uuid.New()
	capWorker := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	cases := []struct {
		name       string
		st         *holdStore
		worker     *store.Worker
		body       string
		wantCode   int
		wantStatus string // body run.status; "" = not a run body
		wantErr    string
	}{
		{
			name:     "applied returns 200 paused",
			st:       &holdStore{held: store.Run{ID: id, Status: "paused"}},
			body:     `{"head":"H"}`,
			wantCode: http.StatusOK, wantStatus: "paused",
		},
		{
			name:     "refused returns 409 with the real running status",
			st:       &holdStore{holdErr: pgx.ErrNoRows, protocolStore: protocolStore{ownedRun: store.Run{ID: id, Status: "running"}}},
			body:     `{"head":"H"}`,
			wantCode: http.StatusConflict, wantStatus: "running",
		},
		{
			name:     "repeat after an applied hold returns 409 paused",
			st:       &holdStore{holdErr: pgx.ErrNoRows, protocolStore: protocolStore{ownedRun: store.Run{ID: id, Status: "paused"}}},
			body:     `{"head":"H"}`,
			wantCode: http.StatusConflict, wantStatus: "paused",
		},
		{
			name:     "run not owned returns 404",
			st:       &holdStore{holdErr: pgx.ErrNoRows, protocolStore: protocolStore{ownedErr: pgx.ErrNoRows}},
			body:     `{"head":"H"}`,
			wantCode: http.StatusNotFound, wantErr: "run not found for this worker",
		},
		{
			name:     "capability worker without claim_generation returns 409",
			st:       &holdStore{held: store.Run{ID: id, Status: "paused"}},
			worker:   &capWorker,
			body:     `{"head":"H"}`,
			wantCode: http.StatusConflict, wantErr: "must stamp claim_generation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProtocolHandler(t, tc.st)
			req := workerReq(http.MethodPost, tc.body, id)
			if tc.worker != nil {
				req = req.WithContext(mw.ContextWithWorker(req.Context(), *tc.worker))
			}
			rec := httptest.NewRecorder()
			h.WorkerRunCompletionHold(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.wantCode, rec.Body)
			}
			if tc.wantStatus != "" {
				var out struct {
					Run struct {
						Status string `json:"status"`
					} `json:"run"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode: %v body=%s", err, rec.Body)
				}
				if out.Run.Status != tc.wantStatus {
					t.Fatalf("run.status=%q want %q body=%s", out.Run.Status, tc.wantStatus, rec.Body)
				}
			}
			if tc.wantErr != "" && !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Fatalf("body %s lacks %q", rec.Body, tc.wantErr)
			}
			if tc.worker != nil && tc.st.calls != 0 {
				t.Fatalf("fence must refuse before the hold query, calls=%d", tc.st.calls)
			}
		})
	}
}
