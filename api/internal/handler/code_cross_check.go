package handler

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func codeCrossCheckResponse(cc store.CrossCheck) apitypes.CodeCrossCheck {
	text := func(v pgtype.Text) *string {
		if !v.Valid {
			return nil
		}
		return &v.String
	}
	timestamp := func(v pgtype.Timestamptz) *time.Time {
		if !v.Valid {
			return nil
		}
		return &v.Time
	}
	var child *string
	if cc.CheckerRunID.Valid {
		id := uuid.UUID(cc.CheckerRunID.Bytes).String()
		child = &id
	}
	return apitypes.CodeCrossCheck{Stage: "code", Round: cc.Round, CandidateGeneration: cc.LeadClaimGeneration,
		BaseCommit: text(cc.BaseCommit), HeadCommit: text(cc.HeadCommit), CandidateDigest: hex.EncodeToString(cc.CandidateDigest),
		CheckerRunID: child, CheckerHarness: text(cc.CheckerHarness), CheckerModel: text(cc.CheckerModel), CheckerEffort: text(cc.CheckerEffort),
		Outcome: cc.Outcome.String, ReasonClass: text(cc.ReasonClass), Findings: cc.Findings, Dispositions: cc.Dispositions, InterruptedAt: timestamp(cc.InterruptedAt),
		FinalizedAt: timestamp(cc.FinalizedAt), DeadlineAt: cc.DeadlineAt.Time}
}

// WorkerCodeCrossCheckDispositions finalizes a single stage-local batch.
// The encoded cap also covers escaped reasons and unknown-length request bodies.
func (h *Handler) WorkerCodeCrossCheckDispositions(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	const bodyLimit = 128 << 10
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit))
	if err != nil {
		httpx.RespondDecodeError(w, err, "invalid dispositions body")
		return
	}
	if !utf8.Valid(raw) {
		httpx.Error(w, http.StatusBadRequest, "invalid UTF-8 body")
		return
	}
	var req struct {
		ClaimGeneration *int64                                `json:"claim_generation"`
		Dispositions    []workersvc.CodeCrossCheckDisposition `json:"dispositions"`
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err := httpx.DecodeJSONStrictBounded(r, &req, bodyLimit); err != nil {
		httpx.RespondDecodeError(w, err, "invalid dispositions body")
		return
	}
	if req.ClaimGeneration == nil || req.Dispositions == nil || len(req.Dispositions) > 20 {
		httpx.Error(w, http.StatusBadRequest, "expected claim generation and at most 20 dispositions")
		return
	}
	cc, err := h.wsvc.FinalizeCodeCrossCheckDispositions(r.Context(), worker, id, *req.ClaimGeneration, req.Dispositions)
	if err != nil {
		crossCheckError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, codeCrossCheckResponse(cc))
}

func (h *Handler) WorkerCodeCrossCheckStatus(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	generation, err := strconv.ParseInt(r.URL.Query().Get("claim_generation"), 10, 64)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid claim generation")
		return
	}
	cc, err := h.wsvc.CodeCrossCheckStatus(r.Context(), worker, id, generation)
	if errors.Is(err, workersvc.ErrCrossCheckNoRow) {
		httpx.JSON(w, http.StatusOK, map[string]any{"stage": "code", "result": "no_row"})
		return
	}
	if err != nil {
		crossCheckError(w, err)
		return
	}
	response := codeCrossCheckResponse(cc)
	if cc.InterruptedAt.Valid {
		response.Findings = []byte("[]")
		response.Dispositions = nil
	}
	httpx.JSON(w, http.StatusOK, response)
}
