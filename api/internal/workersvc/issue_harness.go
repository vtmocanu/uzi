package workersvc

import (
	"errors"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

var (
	ErrHarnessLabelInvalid  = errors.New("harness_label_invalid")
	ErrHarnessLabelConflict = errors.New("harness_label_conflict")
	ErrIssueHarnessCapture  = errors.New("issue harness: missing or mismatched fresh capture")
)

// IssueHarnessSelection is ephemeral selection provenance, never a stored pin.
type IssueHarnessSelection struct {
	Explicit      *Harness
	LabelSelected bool
}

// ResolveIssueHarness uses only the fresh capture for the expected forge identity.
// Labels cannot override an actual pin, and invalid labels take precedence over conflicts.
func ResolveIssueHarness(capture *issueinput.Capture, projectID, iid int64, pin *Harness) (IssueHarnessSelection, error) {
	if capture == nil || capture.ProjectID != projectID || capture.Issue.IID != iid {
		return IssueHarnessSelection{}, ErrIssueHarnessCapture
	}
	var label Harness
	for _, value := range capture.Issue.Labels {
		if !strings.HasPrefix(value, "harness::") {
			continue
		}
		var h Harness
		switch value {
		case "harness::claude":
			h = HarnessClaude
		case "harness::codex":
			h = HarnessCodex
		default:
			return IssueHarnessSelection{}, ErrHarnessLabelInvalid
		}
		if label != "" && label != h {
			return IssueHarnessSelection{}, ErrHarnessLabelInvalid
		}
		label = h
	}
	if label == "" {
		return IssueHarnessSelection{Explicit: pin}, nil
	}
	if pin != nil {
		if *pin != label {
			return IssueHarnessSelection{}, ErrHarnessLabelConflict
		}
		return IssueHarnessSelection{Explicit: pin}, nil
	}
	return IssueHarnessSelection{Explicit: &label, LabelSelected: true}, nil
}
