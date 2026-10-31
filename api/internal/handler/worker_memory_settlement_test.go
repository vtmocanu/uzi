package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func settlementHTTP(h *Handler, worker *store.Worker, id string, body []byte) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Post("/runs/{id}/memory/settlement", h.WorkerMemoryParkSettlement)
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/memory/settlement", bytes.NewReader(body))
	if worker != nil {
		req = req.WithContext(mw.ContextWithWorker(req.Context(), *worker))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// The receipt route reads at most MaxMemoryParkSettlementBytes. Over the cap is a 413 decided before
// any service call (the handler has no service here, so reaching it would panic); anything the strict
// decoder rejects is a 400 without a write.
func TestWorkerMemoryParkSettlementBodyLimits(t *testing.T) {
	h := &Handler{}
	worker := store.Worker{ID: uuid.New(), ProtocolCapabilities: []string{capability.MemoryParkSettlementV1}}
	id := uuid.New().String()
	if r := settlementHTTP(h, nil, id, []byte("{}")); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", r.Code)
	}
	if r := settlementHTTP(h, &worker, "bad", []byte("{}")); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid run id = %d", r.Code)
	}
	over := bytes.Repeat([]byte{' '}, workersvc.MaxMemoryParkSettlementBytes+1)
	if r := settlementHTTP(h, &worker, id, over); r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the cap = %d, want 413", r.Code)
	}
	// Exactly at the cap is read in full and refused by the decoder, not by the limit.
	at := bytes.Repeat([]byte{' '}, workersvc.MaxMemoryParkSettlementBytes)
	svc := workersvc.New(nil, nil, workersvc.Params{})
	if r := settlementHTTP(&Handler{wsvc: svc}, &worker, id, at); r.Code != http.StatusBadRequest {
		t.Fatalf("at the cap = %d, want 400", r.Code)
	}
	for _, body := range []string{"", "{", "{}", `{"version":1}`, "[]", `{"version":1,"version":1}`} {
		if r := settlementHTTP(&Handler{wsvc: svc}, &worker, id, []byte(body)); r.Code != http.StatusBadRequest {
			t.Fatalf("body %q = %d, want 400", body, r.Code)
		}
	}
}

// A worker that never negotiated the receipt protocol is refused before its body is decoded.
func TestWorkerMemoryParkSettlementRequiresCapability(t *testing.T) {
	svc := workersvc.New(nil, nil, workersvc.Params{})
	worker := store.Worker{ID: uuid.New()}
	r := settlementHTTP(&Handler{wsvc: svc}, &worker, uuid.New().String(), []byte("not json"))
	if r.Code != http.StatusConflict {
		t.Fatalf("no capability = %d, want 409", r.Code)
	}
}
