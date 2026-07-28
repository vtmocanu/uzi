package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// The run-eligibility PRD-label gate (PRD #102 Decision 14).
//
// The fixtures here are deliberately issues that would have been RUNNABLE before
// M6: they carry a prds/*.md link, so HasPrdLink is true and every pre-existing
// gate passes them. That is the whole accident the gate exists to stop — an issue
// filed by someone else, cached only because M6's additive fetch now caches open
// issues, that happens to mention a prds/ path.

func labelsJSON(t *testing.T, labels ...string) []byte {
	t.Helper()
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	return b
}

func TestCreateRunRequiresThePRDLabel(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	cases := []struct {
		name string
		// wire configures the service's settings reader; nil leaves it unwired.
		wire            func(*Service)
		labels          []byte
		allowWithoutPRD bool
		wantErr         error
	}{
		{
			name:    "a non-PRD issue with a PRD link is refused",
			labels:  labelsJSON(t, "bug"),
			wantErr: ErrNotPRDIssue,
		},
		{
			name:    "an issue with no labels at all is refused",
			labels:  labelsJSON(t),
			wantErr: ErrNotPRDIssue,
		},
		{
			// PRDLESS is the escape hatch for a PRD issue with no prds/*.md file yet.
			// It was never a claim about whose issue this is, and letting it bypass here
			// would restore the accident on the exact path autopilot takes.
			name:            "PRDLESS does not bypass the label gate",
			labels:          labelsJSON(t, "bug", "PRDLESS"),
			allowWithoutPRD: true,
			wantErr:         ErrNotPRDIssue,
		},
		{
			name:   "a PRD issue passes",
			labels: labelsJSON(t, "PRD", "bug"),
		},
		{
			// The label is operator-configurable, so the gate must read settings rather
			// than a compiled-in constant. An issue still carrying the OLD label is not
			// a PRD issue once prd_label has been renamed.
			name:    "the configured label is what counts, not the literal PRD",
			wire:    func(s *Service) { s.SetSettings(fakeSettings{prdLabel: "Feature"}) },
			labels:  labelsJSON(t, "PRD"),
			wantErr: ErrNotPRDIssue,
		},
		{
			name:   "an issue carrying the configured label passes",
			wire:   func(s *Service) { s.SetSettings(fakeSettings{prdLabel: "Feature"}) },
			labels: labelsJSON(t, "Feature"),
		},
		{
			// Unwired settings must degrade to enforcing the gate on the compiled-in
			// default, never to skipping it: "settings unavailable" is not consent.
			name:   "unwired settings fall back to the default label and still enforce",
			labels: labelsJSON(t, settings.DefaultPRDLabel),
		},
		{
			name:    "unwired settings still refuse a non-PRD issue",
			labels:  labelsJSON(t, "bug"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// A row whose labels cannot be decoded gives the gate no basis for consent.
			name:    "an undecodable labels value is not a PRD issue",
			labels:  []byte("{not json"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// jsonb null, which SQL NOT NULL does not exclude and which a label-less
			// issue reaches the cache as.
			name:    "a jsonb null labels value is not a PRD issue",
			labels:  []byte("null"),
			wantErr: ErrNotPRDIssue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"manual", "autopilot"} {
				t.Run(path, func(t *testing.T) {
					fs := &fakeStore{
						issueByID:       store.Issue{Title: "T", Labels: tc.labels, HasPrdLink: true},
						createRunResult: store.Run{ID: uuid.New()},
					}
					svc := New(fs, newBox(t), testParams())
					if tc.wire != nil {
						tc.wire(svc)
					}

					var err error
					if path == "manual" {
						_, err = svc.CreateRun(context.Background(), user, repo, 4, "desc", tc.allowWithoutPRD)
					} else {
						_, err = svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc", tc.allowWithoutPRD)
					}

					if tc.wantErr != nil {
						if err != tc.wantErr {
							t.Fatalf("err = %v, want %v", err, tc.wantErr)
						}
						if fs.createRunParams != nil {
							t.Fatalf("a refused run must never reach CreateRun, got %+v", fs.createRunParams)
						}
						return
					}
					if err != nil {
						t.Fatalf("unexpected err = %v", err)
					}
					if fs.createRunParams == nil {
						t.Fatal("an eligible issue must reach CreateRun")
					}
				})
			}
		})
	}
}

// TestPRDLabelGatePrecedesTheLinkGate pins the ORDER, which is a user-facing
// property rather than an implementation detail: a non-PRD issue with no PRD link
// satisfies both failure conditions, and reporting it as a missing link would send
// someone off to add a prds/*.md file to an issue that is not uzi's.
func TestPRDLabelGatePrecedesTheLinkGate(t *testing.T) {
	fs := &fakeStore{issueByID: store.Issue{Title: "T", Labels: labelsJSON(t, "bug"), HasPrdLink: false}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", false); err != ErrNotPRDIssue {
		t.Fatalf("err = %v, want ErrNotPRDIssue (not ErrNoPRDLink)", err)
	}
}
