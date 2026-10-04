package handler

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type planCrossCheckRequest struct {
	Stage           string `json:"stage"`
	ClaimGeneration *int64 `json:"claim_generation"`
	workersvc.PlanCrossCheckCandidate
}

type crossCheckVerdictRequest struct {
	ClaimGeneration *int64 `json:"claim_generation"`
	Verdict         string `json:"verdict"`
	ReasonClass     string `json:"reason_class"`
	Summary         string `json:"summary"`
	Items           []struct {
		File      string `json:"file"`
		Severity  string `json:"severity"`
		Summary   string `json:"summary"`
		Rationale string `json:"rationale"`
	} `json:"items"`
}

func crossCheckError(w http.ResponseWriter, err error) {
	if errors.Is(err, workersvc.ErrCrossCheckRefused) {
		reason := "cross_check_refused"
		switch {
		case errors.Is(err, workersvc.ErrCrossCheckInterrupted):
			reason = "interrupted"
		case errors.Is(err, workersvc.ErrCrossCheckUnavailable):
			reason = "checker_unavailable"
		}
		httpx.ErrorReason(w, http.StatusConflict, "cross-check refused", reason)
		return
	}
	httpx.Error(w, http.StatusInternalServerError, "cross-check failed")
}

func (h *Handler) WorkerSubmitPlanCrossCheck(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req planCrossCheckRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 8<<20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "cross-check envelope exceeds limit", "envelope_too_large")
		} else {
			httpx.ErrorReason(w, http.StatusBadRequest, "invalid cross-check request", "candidate_invalid")
		}
		return
	}
	if req.Stage != "plan" || req.ClaimGeneration == nil {
		httpx.ErrorReason(w, http.StatusBadRequest, "invalid cross-check request", "candidate_invalid")
		return
	}
	c := req.PlanCrossCheckCandidate
	if len(c.PlanMd) > 256*1024 || len(c.PlanningDiff) > 512*1024 || len(c.Milestones) > 256*1024 ||
		len(c.RequiredCapabilities) > 64 || len(c.RequiredTools) > 64 {
		httpx.ErrorReason(w, http.StatusBadRequest, "candidate exceeds limit", "candidate_too_large")
		return
	}
	c, normalizeErr := workersvc.NormalizePlanCrossCheckCandidate(c)
	if normalizeErr != nil || c.PlanMd == "" || len(c.BaseCommit) != 40 ||
		strings.Trim(c.BaseCommit, "0123456789abcdefABCDEF") != "" ||
		(c.SizeClass != "s" && c.SizeClass != "m" && c.SizeClass != "l") {
		httpx.ErrorReason(w, http.StatusBadRequest, "invalid cross-check candidate", "candidate_invalid")
		return
	}
	cc, err := h.wsvc.SubmitPlanCrossCheck(r.Context(), worker, id, *req.ClaimGeneration, c)
	if err != nil {
		crossCheckError(w, err)
		return
	}
	status, err := h.wsvc.PlanCrossCheckStatusWithReconciliation(r.Context(), worker, id, *req.ClaimGeneration, cc.Round)
	writeCrossCheckStatus(w, status, err)

}

func (h *Handler) WorkerPlanCrossCheckStatus(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	round, e1 := strconv.ParseInt(chi.URLParam(r, "round"), 10, 32)
	gen, e2 := strconv.ParseInt(r.URL.Query().Get("claim_generation"), 10, 64)
	if e1 != nil || e2 != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid cross-check status request")
		return
	}
	status, err := h.wsvc.PlanCrossCheckStatusWithReconciliation(r.Context(), worker, id, gen, int32(round))
	writeCrossCheckStatus(w, status, err)
}

func writeCrossCheckStatus(w http.ResponseWriter, status workersvc.PlanCrossCheckStatusResult, err error) {
	if status.Parked && status.Reconciliation != nil && (err == nil || errors.Is(err, workersvc.ErrCrossCheckNoRow)) {
		httpx.JSON(w, http.StatusOK, struct {
			Result      string `json:"result"`
			Verdict     string `json:"verdict"`
			ReasonClass string `json:"reason_class"`
			*workersvc.LeadReconciliation
		}{"parked", status.Verdict, status.ReasonClass, status.Reconciliation})
		return
	}
	if errors.Is(err, workersvc.ErrCrossCheckNoRow) {
		httpx.JSON(w, http.StatusOK, planCrossCheckNoRowResponse{Result: "no_row", ReasonClass: "no_candidate", leadReconciliationResponse: reconciliationResponse(status.LeadLastSeq, status.Reconciliation)})
		return
	}
	if err != nil {
		crossCheckError(w, err)
		return
	}
	candidate := crossCheckResponse(status.CrossCheck, status.LeadLastSeq)
	candidate.leadReconciliationResponse = reconciliationResponse(status.LeadLastSeq, status.Reconciliation)
	httpx.JSON(w, http.StatusOK, candidate)

}

func (h *Handler) WorkerCrossCheckVerdict(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req crossCheckVerdictRequest
	if httpx.DecodeJSONStrict(r, &req) != nil || req.ClaimGeneration == nil || len(req.Items) > 20 || len(req.Summary) > 4*1024 ||
		(req.Verdict != "approve" && req.Verdict != "revise" && req.Verdict != "block" && req.Verdict != "failed") {
		httpx.Error(w, http.StatusBadRequest, "invalid cross-check verdict")
		return
	}
	raw, err := json.Marshal(map[string]any{"summary": req.Summary, "items": req.Items})
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid cross-check findings")
		return
	}
	findings, err := workersvc.NormalizeCrossCheckFindings(req.Verdict, req.ReasonClass, raw)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid cross-check reason or findings")
		return
	}
	_, err = h.wsvc.DecidePlanCrossCheck(r.Context(), worker, id, *req.ClaimGeneration, req.Verdict, req.ReasonClass, findings)
	if err != nil {
		crossCheckError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// leadReconciliationResponse owns the cursor; the optional proof details exclude
// that field so encoding/json cannot shadow it through anonymous embedding.
type leadReconciliationResponse struct {
	LeadLastSeq int32 `json:"lead_last_seq"`
	*LeadReconciliationDetails
}

// LeadReconciliationDetails contains optional wire proof fields other than the cursor.
type LeadReconciliationDetails struct {
	ClaimGeneration       int64      `json:"claim_generation"`
	PlanCrossCheckSettled bool       `json:"plan_cross_check_settled"`
	GatePresentationID    *uuid.UUID `json:"gate_presentation_id,omitempty"`
	GateRevision          int64      `json:"gate_revision"`
	GatePayloadDigest     string     `json:"gate_payload_digest,omitempty"`
	CurrentPlanSHA256     string     `json:"current_plan_sha256"`
}

func reconciliationResponse(seq int32, proof *workersvc.LeadReconciliation) leadReconciliationResponse {
	response := leadReconciliationResponse{LeadLastSeq: seq}
	if proof != nil {
		response.LeadLastSeq = proof.LeadLastSeq
		response.LeadReconciliationDetails = &LeadReconciliationDetails{
			ClaimGeneration: proof.ClaimGeneration, PlanCrossCheckSettled: proof.PlanCrossCheckSettled,
			GatePresentationID: proof.GatePresentationID, GateRevision: proof.GateRevision,
			GatePayloadDigest: proof.GatePayloadDigest, CurrentPlanSHA256: proof.CurrentPlanSHA256,
		}
	}
	return response
}

type workerReconciledStateAck struct {
	Run              any `json:"run"`
	CredentialSwitch any `json:"credential_switch,omitempty"`
	Disposition      any `json:"disposition,omitempty"`
	leadReconciliationResponse
}

type planCrossCheckNoRowResponse struct {
	Result      string `json:"result"`
	ReasonClass string `json:"reason_class"`
	leadReconciliationResponse
}

type planCrossCheckCandidateResponse struct {
	Result              string                            `json:"result"`
	Round               int32                             `json:"round"`
	CheckerRunID        *string                           `json:"checker_run_id"`
	CandidateDigest     string                            `json:"candidate_digest"`
	CandidateGeneration int64                             `json:"candidate_generation"`
	Candidate           workersvc.PlanCrossCheckCandidate `json:"candidate"`
	Verdict             string                            `json:"verdict"`
	ReasonClass         string                            `json:"reason_class"`
	Findings            json.RawMessage                   `json:"findings"`
	DeadlineAt          time.Time                         `json:"deadline_at"`
	leadReconciliationResponse
}

// crossCheckResponse publishes the canonical stored candidate and server digest.
func crossCheckResponse(cc store.CrossCheck, lastSeq int32) planCrossCheckCandidateResponse {
	var childID *string
	if cc.CheckerRunID.Valid {
		value := uuid.UUID(cc.CheckerRunID.Bytes).String()
		childID = &value
	}
	return planCrossCheckCandidateResponse{
		Result: "candidate", Round: cc.Round, CheckerRunID: childID,
		CandidateDigest: hex.EncodeToString(cc.CandidateDigest), CandidateGeneration: cc.LeadClaimGeneration,
		Candidate: workersvc.PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: json.RawMessage(cc.Milestones),
			RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String,
			BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String},
		Verdict: cc.Verdict, ReasonClass: cc.ReasonClass.String, Findings: json.RawMessage(cc.Findings),
		DeadlineAt: cc.DeadlineAt.Time, leadReconciliationResponse: reconciliationResponse(lastSeq, nil),
	}
}
