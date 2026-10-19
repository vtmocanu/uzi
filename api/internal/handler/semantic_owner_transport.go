package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Encode both budgets before publishing the semantic transport marker.
func writeOwnerRun(w http.ResponseWriter, dto apitypes.RunDTO) {
	if dto.SemanticGate == nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"run": dto})
		return
	}
	semantic, err := json.Marshal(dto.SemanticGate)
	if err != nil || len(semantic) > apitypes.SemanticGateMaxBytes ||
		dto.SemanticGate.Presentation.Candidate.CandidateIdentityVersion != apitypes.SemanticIdentityVersion ||
		dto.SemanticGate.Approval.CandidateIdentityVersion != apitypes.SemanticIdentityVersion {
		httpx.Error(w, http.StatusInternalServerError, "semantic presentation unavailable")
		return
	}
	dto.SemanticGate = nil
	remainder, err := json.Marshal(map[string]any{"run": dto})
	if err != nil || len(remainder)+len(`,"semantic_gate":`) > apitypes.OwnerRunLegacyMaxBytes {
		httpx.Error(w, http.StatusInternalServerError, "semantic presentation unavailable")
		return
	}
	// The envelope ends in two object closers; insert the already-encoded field.
	raw := append(remainder[:len(remainder)-2], []byte(`,"semantic_gate":`)...)
	raw = append(raw, semantic...)
	raw = append(raw, '}', '}')
	w.Header().Set(apitypes.SemanticIdentityHeader, apitypes.SemanticIdentityVersion)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (h *Handler) decodeOwnerRunInput(r *http.Request, owner, id uuid.UUID, dst *apitypes.RunInputRequest) error {
	if r.Header.Get(apitypes.SemanticIdentityHeader) != apitypes.SemanticIdentityVersion {
		return httpx.DecodeJSON(r, dst)
	}
	// A marker only requests a transport budget. Actual ownership and a stored
	// semantic presentation are prerequisites; the locked service freeze remains authority.
	run, err := h.wsvc.GetRunForViewer(r.Context(), owner, false, id)
	if err != nil || run.UserID != owner {
		return errors.New("semantic owner context unavailable")
	}
	presentation, approval := workersvc.SemanticGateBindingFromRun(run)
	if presentation == nil || approval == nil || presentation.Candidate.CandidateIdentityVersion != apitypes.SemanticIdentityVersion {
		return errors.New("semantic owner context unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, apitypes.SemanticRunInputMaxBytes+1))
	if err != nil || len(raw) > apitypes.SemanticRunInputMaxBytes {
		return errors.New("semantic input too large")
	}
	if err := workersvc.DecodeSemanticRequest(raw, dst); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	echo := fields["semantic_approval"]
	if dst.Kind != "approve_plan" || dst.SemanticApproval == nil ||
		!reflect.DeepEqual(dst.SemanticApproval, approval) ||
		len(echo) > apitypes.SemanticApprovalMaxBytes ||
		len(raw)-len(echo)+len("null") > apitypes.RunInputLegacyMaxBytes ||
		bytes.Equal(bytes.TrimSpace(echo), []byte("null")) {
		return errors.New("invalid semantic approval envelope")
	}
	return nil
}
