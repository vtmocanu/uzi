package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestResolveIssueHarnessPrecedence(t *testing.T) {
	claude, codex := HarnessClaude, HarnessCodex
	for _, labels := range []struct {
		name    string
		values  []string
		label   Harness
		invalid bool
	}{
		{"none", nil, "", false},
		{"unrelated", []string{"uzi", "Harness::codex", "harness:codex"}, "", false},
		{"claude", []string{"harness::claude"}, HarnessClaude, false},
		{"codex", []string{"harness::codex"}, HarnessCodex, false},
		{"duplicate", []string{"harness::codex", "harness::codex"}, HarnessCodex, false},
		{"both", []string{"harness::claude", "harness::codex"}, "", true},
		{"unknown", []string{"harness::other"}, "", true},
		{"empty", []string{"harness::"}, "", true},
		{"unknown_after_valid", []string{"harness::codex", "harness::other"}, "", true},
	} {
		for _, pin := range []*Harness{nil, &claude, &codex} {
			name := "unpinned"
			if pin != nil {
				name = string(*pin)
			}
			t.Run(labels.name+"/"+name, func(t *testing.T) {
				capture := &issueinput.Capture{ProjectID: 42, Issue: forge.Issue{IID: 7, Labels: labels.values}}
				got, err := ResolveIssueHarness(capture, 42, 7, pin)
				if labels.invalid {
					if !errors.Is(err, ErrHarnessLabelInvalid) {
						t.Fatalf("error=%v", err)
					}
					return
				}
				if pin != nil && labels.label != "" && *pin != labels.label {
					if !errors.Is(err, ErrHarnessLabelConflict) {
						t.Fatalf("error=%v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := pin
				if pin == nil && labels.label != "" {
					want = &labels.label
				}
				if (got.Explicit == nil) != (want == nil) || want != nil && *got.Explicit != *want {
					t.Fatalf("selection=%+v want=%v", got, want)
				}
				if got.LabelSelected != (pin == nil && labels.label != "") {
					t.Fatalf("provenance=%+v", got)
				}
			})
		}
	}
}

func TestResolveIssueHarnessRequiresExpectedCapture(t *testing.T) {
	for _, capture := range []*issueinput.Capture{nil, {ProjectID: 43, Issue: forge.Issue{IID: 7}}, {ProjectID: 42, Issue: forge.Issue{IID: 8}}} {
		if _, err := ResolveIssueHarness(capture, 42, 7, nil); !errors.Is(err, ErrIssueHarnessCapture) {
			t.Fatalf("error=%v", err)
		}
	}
}

func TestManualIssueCreationIgnoresHarnessLabels(t *testing.T) {
	fs := &fakeStore{
		issueByID:       store.Issue{Title: "manual", Labels: labelsJSON(t, "uzi", "harness::codex")},
		createRunResult: store.Run{ID: uuid.New()},
	}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "body", nil, nil, false, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if fs.createRunParams == nil || fs.createRunParams.Harness != string(HarnessClaude) {
		t.Fatalf("create=%+v", fs.createRunParams)
	}
}
