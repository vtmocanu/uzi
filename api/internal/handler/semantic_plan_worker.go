package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type semanticCandidateRequest struct {
	ClaimGeneration *int64                            `json:"claim_generation"`
	Structural      workersvc.PlanCrossCheckCandidate `json:"structural"`
	Proposal        json.RawMessage                   `json:"proposal"`
	Sources         []apitypes.SemanticSourceCapture  `json:"sources"`
}

// readSemanticWorkerBody bounds raw bytes before recursive strict decoding.
func readSemanticWorkerBody(w http.ResponseWriter, r *http.Request, limit int64, dst any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		err = workersvc.DecodeSemanticRequest(raw, dst)
	}
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid semantic request")
		return false
	}
	return true
}

func semanticWorkerRun(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return uuid.Nil, uuid.Nil, false
	}
	run, ok := httpx.PathUUID(w, r, "id", "run")
	return worker.ID, run, ok
}

func (h *Handler) WorkerSemanticCandidate(w http.ResponseWriter, r *http.Request) {
	worker, run, ok := semanticWorkerRun(w, r)
	if !ok {
		return
	}
	var req semanticCandidateRequest
	// The 48 MiB proposal plus <7 MiB of escaped bounded structural fields,
	// <1 MiB of base64 source captures/metadata and wrapper fit within 64 MiB.
	// The compiler independently enforces all decoded field and source limits.
	if !readSemanticWorkerBody(w, r, 64<<20, &req) {
		return
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration < 1 || req.Structural.CandidateIdentityVersion != "" || req.Structural.SemanticCandidate != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid semantic candidate")
		return
	}
	candidate, err := h.wsvc.SubmitSemanticCandidate(r.Context(), worker, run, *req.ClaimGeneration, req.Structural, req.Proposal, req.Sources)
	if err != nil {
		httpx.Error(w, http.StatusConflict, "semantic candidate refused")
		return
	}
	payload, err := h.wsvc.SemanticCandidatePayload(r.Context(), worker, run, *req.ClaimGeneration, candidate.Digest)
	if err != nil {
		httpx.Error(w, http.StatusConflict, "semantic candidate payload refused")
		return
	}
	httpx.JSON(w, http.StatusOK, payload)
}

func (h *Handler) WorkerSemanticCoverageStatus(w http.ResponseWriter, r *http.Request) {
	worker, run, ok := semanticWorkerRun(w, r)
	if !ok {
		return
	}
	values := r.URL.Query()
	generation, err := strconv.ParseInt(values.Get("claim_generation"), 10, 64)
	if err != nil || generation < 1 || len(values) != 1 || len(values["claim_generation"]) != 1 {
		httpx.Error(w, http.StatusBadRequest, "invalid semantic claim generation")
		return
	}
	status, err := h.wsvc.SemanticCoverageStatus(r.Context(), worker, run, generation)
	if err != nil {
		httpx.Error(w, http.StatusConflict, "semantic coverage refused")
		return
	}
	httpx.JSON(w, http.StatusOK, status)
}

// The generation is a flat transport fence. The remaining exact keys are
// decoded by the service DTO boundary; no nested request object is accepted.
func (h *Handler) workerSemanticCoverageWrite(w http.ResponseWriter, r *http.Request, operation string) {
	worker, run, ok := semanticWorkerRun(w, r)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	limit := int64(4096)
	if operation == "publish" {
		limit = (64 << 10) + 128
	}
	if !readSemanticWorkerBody(w, r, limit, &fields) {
		return
	}
	var generation int64
	if json.Unmarshal(fields["claim_generation"], &generation) != nil || generation < 1 {
		httpx.Error(w, http.StatusBadRequest, "invalid semantic claim generation")
		return
	}
	delete(fields, "claim_generation")
	raw, err := json.Marshal(fields)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid semantic request")
		return
	}
	if operation == "reserve" {
		result, err := h.wsvc.ReserveSemanticCoverage(r.Context(), worker, run, generation, raw)
		if err != nil {
			httpx.Error(w, http.StatusConflict, "semantic reservation refused")
			return
		}
		httpx.JSON(w, http.StatusOK, result)
		return
	}
	if operation == "settle" {
		err = h.wsvc.SettleSemanticCoverage(r.Context(), worker, run, generation, raw)
	} else {
		err = h.wsvc.PublishSemanticCoverage(r.Context(), worker, run, generation, raw)
	}
	if err != nil {
		httpx.Error(w, http.StatusConflict, "semantic coverage write refused")
		return
	}
	status, err := h.wsvc.SemanticCoverageStatus(r.Context(), worker, run, generation)
	if err != nil {
		httpx.Error(w, http.StatusConflict, "semantic coverage refused")
		return
	}
	httpx.JSON(w, http.StatusOK, status)
}
func (h *Handler) WorkerSemanticCoverageReserve(w http.ResponseWriter, r *http.Request) {
	h.workerSemanticCoverageWrite(w, r, "reserve")
}
func (h *Handler) WorkerSemanticCoverageSettle(w http.ResponseWriter, r *http.Request) {
	h.workerSemanticCoverageWrite(w, r, "settle")
}
func (h *Handler) WorkerSemanticCoveragePublish(w http.ResponseWriter, r *http.Request) {
	h.workerSemanticCoverageWrite(w, r, "publish")
}
