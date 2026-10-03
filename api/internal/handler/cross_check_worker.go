package handler

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
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
		httpx.Error(w, http.StatusConflict, "cross-check refused")
		return
	}
	httpx.Error(w, http.StatusInternalServerError, "cross-check failed")
}

// Decode before scrubbing so escaped Unicode, bidi controls and credential text
// cannot bypass the ingest boundary by hiding in JSON escapes.
func scrubPlanCrossCheckMilestones(raw json.RawMessage) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var values []any
	if err := decoder.Decode(&values); err != nil || len(values) > 64 {
		return nil, false
	}
	var scrub func(any, int) (any, bool)
	scrub = func(value any, depth int) (any, bool) {
		if depth > 32 {
			return nil, false
		}
		switch item := value.(type) {
		case string:
			return scrubThenBoundMarkdown(item, 256*1024), true
		case []any:
			for i := range item {
				var ok bool
				item[i], ok = scrub(item[i], depth+1)
				if !ok {
					return nil, false
				}
			}
		case map[string]any:
			for key, child := range item {
				if key != scrubThenBoundSelfReported(key, 128) {
					return nil, false
				}
				next, ok := scrub(child, depth+1)
				if !ok {
					return nil, false
				}
				item[key] = next
			}
		}
		return value, true
	}
	clean, ok := scrub(values, 0)
	if !ok {
		return nil, false
	}
	encoded, err := json.Marshal(clean)
	return encoded, err == nil && len(encoded) <= 256*1024
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
	if httpx.DecodeJSONStrict(r, &req) != nil || req.Stage != "plan" || req.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid cross-check request")
		return
	}
	c := req.PlanCrossCheckCandidate
	if len(c.PlanMd) > 256*1024 || len(c.PlanningDiff) > 512*1024 || len(c.Milestones) > 256*1024 {
		httpx.Error(w, http.StatusBadRequest, "candidate exceeds limit")
		return
	}
	c.PlanMd = scrubThenBoundMarkdown(c.PlanMd, 256*1024)
	c.PlanningDiff = scrubThenBoundMarkdown(c.PlanningDiff, 512*1024)
	scrubbedMilestones, ok := scrubPlanCrossCheckMilestones(c.Milestones)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "invalid milestones")
		return
	}
	c.Milestones = scrubbedMilestones
	for i := range c.RequiredCapabilities {
		c.RequiredCapabilities[i] = scrubThenBoundSelfReported(c.RequiredCapabilities[i], 256)
	}
	for i := range c.RequiredTools {
		c.RequiredTools[i] = scrubThenBoundSelfReported(c.RequiredTools[i], 256)
	}
	c.SizeClass = scrubThenBoundSelfReported(c.SizeClass, 64)
	cc, err := h.wsvc.SubmitPlanCrossCheck(r.Context(), worker, id, *req.ClaimGeneration, c)
	if err != nil {
		crossCheckError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"round": cc.Round, "checker_run_id": uuid.UUID(cc.CheckerRunID.Bytes).String(),
		"candidate_digest": hex.EncodeToString(cc.CandidateDigest), "deadline_at": cc.DeadlineAt.Time})
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
	cc, leadLastSeq, err := h.wsvc.PlanCrossCheckStatus(r.Context(), worker, id, gen, int32(round))
	if err != nil {
		crossCheckError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"round": cc.Round, "verdict": cc.Verdict,
		"reason_class": cc.ReasonClass.String, "findings": json.RawMessage(cc.Findings), "deadline_at": cc.DeadlineAt.Time,
		"lead_last_seq": leadLastSeq})
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
	items := make([]map[string]string, 0, len(req.Items))
	for _, item := range req.Items {
		if len(item.File)+len(item.Severity)+len(item.Summary)+len(item.Rationale) > 2*1024 ||
			(item.Severity != "info" && item.Severity != "warning" && item.Severity != "error") {
			httpx.Error(w, http.StatusBadRequest, "invalid cross-check item")
			return
		}
		items = append(items, map[string]string{"file": scrubThenBoundSelfReported(item.File, 512),
			"severity": item.Severity, "summary": scrubThenBoundMarkdown(item.Summary, 1024),
			"rationale": scrubThenBoundMarkdown(item.Rationale, 2048)})
	}
	findings, err := json.Marshal(map[string]any{"summary": scrubThenBoundMarkdown(req.Summary, 4*1024), "items": items})
	if err != nil || len(findings) > 32*1024 {
		httpx.Error(w, http.StatusBadRequest, "findings exceed limit")
		return
	}
	_, err = h.wsvc.DecidePlanCrossCheck(r.Context(), worker, id, *req.ClaimGeneration, req.Verdict, req.Verdict, findings)
	if err != nil {
		crossCheckError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
