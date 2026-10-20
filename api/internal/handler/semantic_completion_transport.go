package handler

import (
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

func (h *Handler) decodeSemanticCompletionDecision(r *http.Request, owner, id uuid.UUID, dst *completionDecisionRequest) error {
	if r.Header.Get(apitypes.SemanticIdentityHeader) != apitypes.SemanticIdentityVersion {
		return httpx.DecodeJSON(r, dst)
	}
	// Read-only preflight grants a transport budget, never write authority.
	// CompletionPending rechecks the frozen contract's individual field bounds.
	// The service rechecks ownership, exact values and revision under the run lock.
	run, err := h.wsvc.GetRunForViewer(r.Context(), owner, false, id)
	if err != nil || run.UserID != owner {
		return httpx.DecodeJSON(r, dst)
	}
	pending, err := workersvc.CompletionPending(run)
	if err != nil || pending == nil {
		return httpx.DecodeJSON(r, dst)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, apitypes.SemanticRunInputMaxBytes+1))
	if err != nil || len(raw) > apitypes.SemanticRunInputMaxBytes {
		return errors.New("semantic decision too large")
	}
	if err := workersvc.DecodeSemanticRequest(raw, dst); err != nil {
		return err
	}
	if len(raw) <= apitypes.RunInputLegacyMaxBytes {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	echo := fields["maintainers"]
	if dst.Decision != "authorize_maintainer" || dst.ContractRevision <= 0 || len(dst.Keep) != 0 || len(dst.Criteria) != 0 ||
		len(dst.Maintainers) == 0 || len(dst.Maintainers) > 256 || len(echo) > apitypes.SemanticApprovalMaxBytes ||
		len(raw)-len(echo)+len("null") > apitypes.RunInputLegacyMaxBytes {
		return errors.New("invalid semantic decision envelope")
	}
	frozen := make(map[string]apitypes.SemanticMaintainerApproval, len(pending.Criteria))
	for _, c := range pending.Criteria {
		frozen[c.CriterionID] = apitypes.SemanticMaintainerApproval{ID: c.CriterionID, SemanticMaintainer: c.SemanticMaintainer}
	}
	seen := map[string]bool{}
	for _, m := range dst.Maintainers {
		want, ok := frozen[m.ID]
		if !ok || seen[m.ID] || !reflect.DeepEqual(m, want) {
			return errors.New("invalid semantic maintainer echo")
		}
		seen[m.ID] = true
	}
	return nil
}
