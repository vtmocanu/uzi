package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func memoryHTTP(h *Handler, worker *store.Worker, id, lane, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Post("/runs/{id}/memory/reserve", h.WorkerMemoryReserve)
	router.Post("/runs/{id}/memory/outcome", h.WorkerMemoryOutcome)
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/memory/"+lane, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if worker != nil {
		req = req.WithContext(mw.ContextWithWorker(req.Context(), *worker))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestWorkerMemoryMalformedRequests(t *testing.T) {
	h := &Handler{}
	worker := store.Worker{ID: uuid.New()}
	id := uuid.New().String()
	for _, lane := range []string{"reserve", "outcome"} {
		if r := memoryHTTP(h, nil, id, lane, "{}"); r.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s=%d", lane, r.Code)
		}
		for _, body := range []string{"{", "{}", `{"unexpected":true}`, `{"run_id":"invalid"}`} {
			if r := memoryHTTP(h, &worker, id, lane, body); r.Code != http.StatusBadRequest {
				t.Fatalf("%s body=%s status=%d", lane, body, r.Code)
			}
		}
		if r := memoryHTTP(h, &worker, "bad", lane, "{}"); r.Code != http.StatusBadRequest {
			t.Fatalf("invalid path=%d", r.Code)
		}
	}
}

func TestMemoryClaimPolicyAbsentWire(t *testing.T) {
	for _, payload := range []any{workersvc.ClaimPayload{}, workersvc.ChatClaimPayload{}} {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]json.RawMessage
		if err = json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		if string(obj["memory_policy"]) != "null" {
			t.Fatalf("absent opt-in invented policy: %s", obj["memory_policy"])
		}
	}
}
