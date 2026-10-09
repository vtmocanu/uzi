package workersvc

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"
)

// CodeCrossCheckFinding preserves checker identity exactly. Prose is untrusted.
type CodeCrossCheckFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int32  `json:"line"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

var codeFindingID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NormalizeCodeCrossCheckFindings rejects ambiguous identities before scrubbing.
func NormalizeCodeCrossCheckFindings(outcome, reason string, findings []CodeCrossCheckFinding) ([]byte, error) {
	if outcome == "completed" {
		if reason != "" {
			return nil, ErrCrossCheckRefused
		}
	} else if outcome == "failed" {
		switch reason {
		case "malformed", "model_error", "model_timeout", "checker_unavailable", "confinement_failed", "snapshot_failed":
		default:
			return nil, ErrCrossCheckRefused
		}
		if len(findings) != 0 {
			return nil, ErrCrossCheckRefused
		}
	} else {
		return nil, ErrCrossCheckRefused
	}
	original, originalErr := json.Marshal(findings)
	if originalErr != nil || len(original) > 32768 {
		return nil, ErrCrossCheckRefused
	}
	if len(findings) > 20 {
		return nil, ErrCrossCheckRefused
	}
	clean := make([]CodeCrossCheckFinding, len(findings))
	seen := make(map[string]bool, len(findings))
	for i, f := range findings {
		if !codeFindingID.MatchString(f.ID) || seen[f.ID] || f.Line < 0 ||
			!utf8.ValidString(f.Path) || !utf8.ValidString(f.Title) || !utf8.ValidString(f.Detail) {
			return nil, ErrCrossCheckRefused
		}
		seen[f.ID] = true
		switch f.Severity {
		case "critical", "major", "minor":
		default:
			return nil, ErrCrossCheckRefused
		}
		raw, err := json.Marshal(f)
		if err != nil || len(raw) > 2048 {
			return nil, ErrCrossCheckRefused
		}
		f.Path = scrubThenBound(f.Path, 2048)
		f.Title = scrubThenBound(f.Title, 2048)
		f.Detail = scrubThenBound(f.Detail, 2048)
		raw, err = json.Marshal(f)
		if err != nil || len(raw) > 2048 {
			return nil, ErrCrossCheckRefused
		}
		clean[i] = f
	}
	raw, err := json.Marshal(clean)
	if err != nil || len(raw) > 32768 {
		return nil, ErrCrossCheckRefused
	}
	return raw, nil
}
