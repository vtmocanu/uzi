package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The run-eligibility gate (PRD #764 M1): a single configurable `uzi` label is the ONE
// gate. An issue is uzi's to run iff its cached labels carry the configured uzi_label.
// The old PRD-link gate (Gate B), PRDLESS bypass, and non-primary waiver are GONE — a
// run no longer requires a prds/*.md link.
//
// These cases are calibrated to FAIL against the pre-change binary: a `uzi`-only issue
// with NO PRD link (HasPrdLink=false, no PRDLESS) was refused pre-change (Gate A's
// eligible-set did not include "uzi", or Gate B's link requirement bit) and now RUNS on
// every create path.

func labelsJSON(t *testing.T, labels ...string) []byte {
	t.Helper()
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	return b
}

// runAllPaths invokes each of the four create paths against a freshly-built service so a
// prior path's created-run state cannot leak. It returns the error from each path keyed
// by name, plus whether the run reached the store's CreateRun (i.e. passed the gate).
type pathResult struct {
	err     error
	reached bool
}

func runAllPaths(t *testing.T, wire func(*Service), labels []byte, hasPRDLink bool) map[string]pathResult {
	t.Helper()
	user, repo := uuid.New(), uuid.New()
	out := map[string]pathResult{}
	invoke := func(name string, call func(svc *Service) error) {
		fs := &fakeStore{
			issueByID:       store.Issue{Title: "T", Labels: labels, HasPrdLink: hasPRDLink},
			createRunResult: store.Run{ID: uuid.New()},
		}
		svc := New(fs, newBox(t), testParams())
		if wire != nil {
			wire(svc)
		}
		err := call(svc)
		out[name] = pathResult{err: err, reached: fs.createRunParams != nil}
	}
	invoke("CreateRun", func(svc *Service) error {
		_, err := svc.CreateRun(context.Background(), user, repo, 4, "desc", nil, nil)
		return err
	})
	invoke("CreateScheduledRun", func(svc *Service) error {
		_, err := svc.CreateScheduledRun(context.Background(), user, repo, 4, "desc", nil, nil, false, nil)
		return err
	})
	invoke("CreateAutopilotRun", func(svc *Service) error {
		_, err := svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc")
		return err
	})
	invoke("CreateScheduledAutopilotRun", func(svc *Service) error {
		_, err := svc.CreateScheduledAutopilotRun(context.Background(), user, repo, 4, "desc", nil, nil, false)
		return err
	})
	return out
}

// TestUziLabelGate is the M1 headline: a `uzi`-only issue (no PRD link, no PRDLESS)
// RUNS on every path, and an issue without `uzi` is refused with ErrNotPRDIssue on
// every path. The first case is exactly the one that fails on the pre-change binary.
func TestUziLabelGate(t *testing.T) {
	uziWired := func(s *Service) { s.SetSettings(fakeSettings{uziLabel: "uzi"}) }

	cases := []struct {
		name       string
		wire       func(*Service)
		labels     []byte
		hasPRDLink bool
		wantErr    error // nil ⇒ the run must fire on every path
	}{
		{
			// The headline reversal (fails pre-change): only `uzi`, no PRD link, no
			// PRDLESS — refused pre-change, runs now on every path.
			name:   "a uzi-only issue with no PRD link runs on every path",
			wire:   uziWired,
			labels: labelsJSON(t, "uzi"),
		},
		{
			// A run no longer requires the label to co-exist with anything: bug+uzi
			// (a swept issue) with no link fires. Pre-change this hit Gate B.
			name:   "a bug+uzi issue with no PRD link runs on every path",
			wire:   uziWired,
			labels: labelsJSON(t, "bug", "uzi"),
		},
		{
			// A present PRD link is neither required nor a substitute: an issue with a
			// link but WITHOUT `uzi` is still refused.
			name:       "a PRD-linked issue without uzi is refused on every path",
			wire:       uziWired,
			labels:     labelsJSON(t, "bug"),
			hasPRDLink: true,
			wantErr:    ErrNotPRDIssue,
		},
		{
			name:    "an issue with no labels at all is refused",
			wire:    uziWired,
			labels:  labelsJSON(t),
			wantErr: ErrNotPRDIssue,
		},
		{
			// PRDLESS is no longer special — an issue carrying it but NOT `uzi` is
			// refused, proving the old escape hatch is gone.
			name:    "PRDLESS without uzi is refused",
			wire:    uziWired,
			labels:  labelsJSON(t, "PRDLESS"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// The label is operator-configurable: with uzi_label renamed to "run-me",
			// an issue carrying the literal "uzi" is not eligible.
			name:    "the configured label is what counts, not the literal uzi",
			wire:    func(s *Service) { s.SetSettings(fakeSettings{uziLabel: "run-me"}) },
			labels:  labelsJSON(t, "uzi"),
			wantErr: ErrNotPRDIssue,
		},
		{
			name:   "an issue carrying the configured label passes",
			wire:   func(s *Service) { s.SetSettings(fakeSettings{uziLabel: "run-me"}) },
			labels: labelsJSON(t, "run-me"),
		},
		{
			// Unwired settings must degrade to enforcing the gate on the compiled-in
			// default label "uzi", never to skipping it.
			name:   "unwired settings fall back to the default uzi label: passes",
			labels: labelsJSON(t, settings.DefaultUziLabel),
		},
		{
			name:    "unwired settings still refuse a non-uzi issue",
			labels:  labelsJSON(t, "documentation"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// A row whose labels cannot be decoded gives the gate no basis for consent.
			name:    "an undecodable labels value is not eligible",
			wire:    uziWired,
			labels:  []byte("{not json"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// jsonb null, which a label-less issue reaches the cache as.
			name:    "a jsonb null labels value is not eligible",
			wire:    uziWired,
			labels:  []byte("null"),
			wantErr: ErrNotPRDIssue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := runAllPaths(t, tc.wire, tc.labels, tc.hasPRDLink)
			for path, r := range results {
				if tc.wantErr != nil {
					if r.err != tc.wantErr {
						t.Errorf("[%s] err = %v, want %v", path, r.err, tc.wantErr)
					}
					if r.reached {
						t.Errorf("[%s] a refused run must never reach CreateRun", path)
					}
					continue
				}
				if r.err != nil {
					t.Errorf("[%s] unexpected err = %v", path, r.err)
				}
				if !r.reached {
					t.Errorf("[%s] an eligible issue must reach CreateRun", path)
				}
			}
		})
	}
}
