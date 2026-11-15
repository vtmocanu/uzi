package handler

import (
	"context"
	"errors"
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

type inventoryFailureStore struct {
	protocolStore
	worker            store.Worker
	livenessCommitted bool
}

func (s *inventoryFailureStore) HeartbeatWorker(_ context.Context, arg store.HeartbeatWorkerParams) (store.Worker, error) {
	s.livenessCommitted = true
	s.heartbeatArg = arg
	return s.worker, nil
}

type inventoryFailureBegin struct{ calls int }

func (b *inventoryFailureBegin) Begin(context.Context) (pgx.Tx, error) {
	b.calls++
	return nil, errors.New("injected inventory transaction failure")
}

func TestWorkerHeartbeatInventoryFailureKeepsExclusionUnknown(t *testing.T) {
	op := uuid.NewString()
	for name, body := range map[string]string{
		"plain": `{"version":"v"}`,
		"held":  `{"hibernation_exclusion":{"op_id":"` + op + `","held":true}}`,
		"lost":  `{"hibernation_exclusion":{"op_id":"` + op + `","held":false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			st := &inventoryFailureStore{worker: store.Worker{ID: uuid.New(), UserID: uuid.New(), Kind: "hosted", HibernationState: "preparing", ProtocolCapabilities: []string{capability.HibernationInventoryV1}}}
			h := newProtocolHandler(t, st)
			begin := &inventoryFailureBegin{}
			h.wsvc.SetTxBeginner(begin)
			req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(body))
			req = req.WithContext(mw.ContextWithWorker(req.Context(), st.worker))
			rec := httptest.NewRecorder()
			h.WorkerHeartbeat(rec, req)
			if !st.livenessCommitted || begin.calls != 1 {
				t.Fatalf("did not reach inventory after liveness: committed=%v begins=%d", st.livenessCommitted, begin.calls)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("heartbeat = %d, want 500 to preserve the held exclusion: %s", rec.Code, rec.Body)
			}
		})
	}
}
