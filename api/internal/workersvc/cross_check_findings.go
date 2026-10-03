package workersvc

import (
	"bytes"
	"encoding/json"
	"io"
)

// NormalizeCrossCheckFindings accepts only bounded model-authored findings.
// Outcome labels and reason classes are validated independently by the server.
func NormalizeCrossCheckFindings(verdict, reason string, raw []byte) ([]byte, error) {
	switch verdict {
	case "approve", "revise", "block":
		if reason != verdict {
			return nil, ErrCrossCheckRefused
		}
	case "failed":
		switch reason {
		case "malformed", "model_error", "model_timeout", "checker_unavailable", "confinement_failed":
		default:
			return nil, ErrCrossCheckRefused
		}
	default:
		return nil, ErrCrossCheckRefused
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || len(raw) > 32*1024 {
		return nil, ErrCrossCheckRefused
	}
	var findings struct {
		Summary string `json:"summary"`
		Items   []struct {
			File      string `json:"file"`
			Severity  string `json:"severity"`
			Summary   string `json:"summary"`
			Rationale string `json:"rationale"`
		} `json:"items"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&findings) != nil || len(findings.Summary) > 4*1024 || len(findings.Items) > 20 {
		return nil, ErrCrossCheckRefused
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, ErrCrossCheckRefused
	}
	findings.Summary = scrubThenBound(findings.Summary, 4*1024)
	for i := range findings.Items {
		item := &findings.Items[i]
		if len(item.File)+len(item.Severity)+len(item.Summary)+len(item.Rationale) > 2*1024 {
			return nil, ErrCrossCheckRefused
		}
		switch item.Severity {
		case "info", "warning", "error":
		default:
			return nil, ErrCrossCheckRefused
		}
		file, err := normalizeCrossCheckIdentifier(item.File, 512)
		if err != nil {
			return nil, err
		}
		item.File = file
		item.Summary = scrubThenBound(item.Summary, 1024)
		item.Rationale = scrubThenBound(item.Rationale, 2048)
		if len(item.File)+len(item.Severity)+len(item.Summary)+len(item.Rationale) > 2*1024 {
			return nil, ErrCrossCheckRefused
		}
	}
	clean, err := json.Marshal(findings)
	if err != nil || len(clean) > 32*1024 {
		return nil, ErrCrossCheckRefused
	}
	return clean, nil
}
