package uzicli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func readOwnerResponse(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return raw, nil
}

func validateOwnerRunTransport(resp *http.Response, raw []byte, run apitypes.RunDTO) error {
	marker := resp.Header.Get(apitypes.SemanticIdentityHeader)
	semantic := run.SemanticGate
	var env struct {
		Run map[string]json.RawMessage `json:"run"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Exitf(ExitGeneric, "invalid owner run envelope")
	}
	part, present := env.Run["semantic_gate"]
	if present && semantic == nil {
		return Exitf(ExitGeneric, "invalid owner run semantic field")
	}
	if (marker != "" && marker != apitypes.SemanticIdentityVersion) ||
		(marker == apitypes.SemanticIdentityVersion) != (semantic != nil) {
		return Exitf(ExitGeneric, "owner run semantic marker/body mismatch")
	}
	if semantic == nil {
		return nil
	}
	if len(part) > apitypes.SemanticGateMaxBytes ||
		len(raw)-len(part) > apitypes.OwnerRunLegacyMaxBytes ||
		semantic.Presentation.Candidate.CandidateIdentityVersion != apitypes.SemanticIdentityVersion ||
		semantic.Approval.CandidateIdentityVersion != apitypes.SemanticIdentityVersion {
		return Exitf(ExitGeneric, "invalid owner run semantic transport")
	}
	return nil
}
