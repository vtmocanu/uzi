package workersvc

import (
	"bytes"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// frozenIssueCaptures preserves the worker's source ID but never its issue
// content or provenance. A mismatched capture is refused before compilation,
// so worker citations cannot silently change meaning after rebinding.
func frozenIssueCaptures(description string, captures []apitypes.SemanticSourceCapture) ([]apitypes.SemanticSourceCapture, error) {
	out := append([]apitypes.SemanticSourceCapture(nil), captures...)
	issues := 0
	for i, source := range out {
		if source.Kind != "issue" {
			continue
		}
		issues++
		if !bytes.Equal(source.Bytes, []byte(description)) {
			return nil, fmt.Errorf("issue capture does not match frozen run")
		}
		out[i].Bytes = []byte(description)
		out[i].Provenance = "server:run-issue"
		out[i].Completeness = "complete"
	}
	if issues != 1 {
		return nil, fmt.Errorf("one frozen issue capture required")
	}
	return out, nil
}
