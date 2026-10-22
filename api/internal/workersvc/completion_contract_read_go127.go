//go:build go1.27

package workersvc

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The selected patched toolchain has a single-pass strict decoder. Only frozen
// semantic reads use it; structural reads and all serialized identities stay on
// their existing encoding/json path.
func decodeSemanticCompletionContract(raw []byte, dst *completionContract) error {
	return jsonv2.Unmarshal(raw, dst, jsonv2.RejectUnknownMembers(true),
		jsonv2.MatchCaseInsensitiveNames(false), jsonv2.WithUnmarshalers(completionReadUnmarshalers))
}

var completionReadUnmarshalers = jsonv2.JoinUnmarshalers(
	jsonv2.UnmarshalFunc(func(raw []byte, dst *apitypes.SemanticCitation) error {
		return strictSemanticJSON(raw, dst)
	}),
	jsonv2.UnmarshalFunc(func(raw []byte, dst *apitypes.SemanticEvidence) error {
		return strictSemanticJSON(raw, dst)
	}),
	jsonv2.UnmarshalFunc(func(raw []byte, dst *apitypes.SemanticCoverageAtomicity) error {
		return strictSemanticJSON(raw, dst)
	}),
	jsonv2.UnmarshalFunc(func(raw []byte, dst *json.RawMessage) error {
		// Opaque audit values still obey the original nested-key boundary.
		if len(raw) > 0 && (raw[0] == '{' || raw[0] == '[') {
			var value any
			if err := strictSemanticJSON(raw, &value); err != nil {
				return err
			}
		}
		return json.Unmarshal(raw, dst)
	}),
)
