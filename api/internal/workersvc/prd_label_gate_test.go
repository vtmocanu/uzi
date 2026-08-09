package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// The run-eligibility gate (PRD #102 Decision 14, generalised by PRD #196).
//
// PRD #196 widens the gate from "carries the primary label" to "carries ANY
// run-eligible label" (an admin-configured set that always includes the primary,
// default {PRD, bug}), and adds a PRD-link waiver scoped to a MANUAL run eligible via
// a NON-PRIMARY label. Autopilot behaviour does not change.
//
// The fixtures here are deliberately issues that would have been RUNNABLE before M6:
// they carry a prds/*.md link, so HasPrdLink is true and every pre-existing gate
// passes them. That is the whole accident the gate exists to stop — an issue filed by
// someone else, cached only because M6's additive fetch now caches open issues, that
// happens to mention a prds/ path.

func labelsJSON(t *testing.T, labels ...string) []byte {
	t.Helper()
	b, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	return b
}

// TestCreateRunEligibilityGate exercises ONLY the eligibility (label-set) gate: every
// fixture carries a PRD link, so the link gate always passes and the outcome turns on
// whether the issue carries a run-eligible label. The PRD-link waiver is tested
// separately (TestCreateRunPRDLinkWaiver), where HasPrdLink is false.
func TestCreateRunEligibilityGate(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	// eligiblePRDbug wires primary "PRD" with a configured non-primary eligible label
	// "bug"; runEligibleLabels unions the primary in, so the effective set is
	// {PRD, bug}. It is the shipped default expressed explicitly, so the table does
	// not depend on the compiled-in fallback except where it says so.
	eligiblePRDbug := func(s *Service) {
		s.SetSettings(fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}})
	}

	cases := []struct {
		name string
		// wire configures the service's settings reader; nil leaves it unwired, which
		// falls back to the compiled-in eligible set {PRD, bug}.
		wire            func(*Service)
		labels          []byte
		allowWithoutPRD bool
		wantErr         error
	}{
		{
			// bug is run-eligible by default now, so an issue carrying it (and a PRD
			// link) is runnable — the reversal PRD #196 is about.
			name:   "an eligible non-primary issue is runnable",
			wire:   eligiblePRDbug,
			labels: labelsJSON(t, "bug"),
		},
		{
			// The anti-accident property survives the widening: an issue carrying
			// neither the primary nor any eligible label is refused even WITH a PRD
			// link, exactly as a non-PRD issue was before.
			name:    "a neither-primary-nor-eligible issue is refused even with a PRD link",
			wire:    eligiblePRDbug,
			labels:  labelsJSON(t, "documentation"),
			wantErr: ErrNotPRDIssue,
		},
		{
			name:    "an issue with no labels at all is refused",
			wire:    eligiblePRDbug,
			labels:  labelsJSON(t),
			wantErr: ErrNotPRDIssue,
		},
		{
			// PRDLESS is the escape hatch for an eligible issue with no prds/*.md file
			// yet. It was never a claim about whose issue this is, and letting it bypass
			// the eligibility gate would restore the accident on the exact path
			// autopilot takes. "documentation" is deliberately NOT eligible.
			name:            "PRDLESS does not bypass the eligibility gate",
			wire:            eligiblePRDbug,
			labels:          labelsJSON(t, "documentation", "PRDLESS"),
			allowWithoutPRD: true,
			wantErr:         ErrNotPRDIssue,
		},
		{
			name:   "a primary issue passes",
			wire:   eligiblePRDbug,
			labels: labelsJSON(t, "PRD", "bug"),
		},
		{
			// The primary is operator-configurable, so the gate must read settings
			// rather than a compiled-in constant. With prd_label renamed to "Feature"
			// and no configured extras, only "Feature" is eligible: an issue still
			// carrying the OLD "PRD" label is not eligible.
			name:    "the configured primary is what counts, not the literal PRD",
			wire:    func(s *Service) { s.SetSettings(fakeSettings{prdLabel: "Feature"}) },
			labels:  labelsJSON(t, "PRD"),
			wantErr: ErrNotPRDIssue,
		},
		{
			name:   "an issue carrying the configured primary passes",
			wire:   func(s *Service) { s.SetSettings(fakeSettings{prdLabel: "Feature"}) },
			labels: labelsJSON(t, "Feature"),
		},
		{
			// runEligibleLabels ALWAYS unions the primary in, even when the reader
			// returns a set that omits it — a hand-edited settings row that dropped the
			// primary can never make it non-runnable (fail-safe).
			name:   "the primary is eligible even if the reader omits it",
			wire:   func(s *Service) { s.SetSettings(fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}}) },
			labels: labelsJSON(t, "PRD"),
		},
		{
			// Unwired settings must degrade to enforcing the gate on the compiled-in
			// default eligible set {PRD, bug}, never to skipping it.
			name:   "unwired settings fall back to the default set: primary passes",
			labels: labelsJSON(t, settings.DefaultPRDLabel),
		},
		{
			name:   "unwired settings fall back to the default set: bug passes",
			labels: labelsJSON(t, "bug"),
		},
		{
			name:    "unwired settings still refuse a non-eligible issue",
			labels:  labelsJSON(t, "documentation"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// A row whose labels cannot be decoded gives the gate no basis for consent.
			name:    "an undecodable labels value is not eligible",
			labels:  []byte("{not json"),
			wantErr: ErrNotPRDIssue,
		},
		{
			// jsonb null, which SQL NOT NULL does not exclude and which a label-less
			// issue reaches the cache as.
			name:    "a jsonb null labels value is not eligible",
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
						// nil waitOnLimit (PRD #35): inherit the owner's default, which is
						// the pre-#35 behaviour this gate was written against.
						_, err = svc.CreateRun(context.Background(), user, repo, 4, "desc", tc.allowWithoutPRD, nil, nil)
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

// TestCreateRunPRDLinkWaiver pins the PRD #196 Decision 7 waiver and its two scope
// qualifiers. Every fixture has HasPrdLink=false and no PRDLESS, so the ONLY thing
// that can let the run through is the waiver. The waiver applies iff the issue is
// eligible via a NON-PRIMARY label AND the run is manual (!autoApprove).
func TestCreateRunPRDLinkWaiver(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	cases := []struct {
		name    string
		fs      fakeSettings
		labels  []byte
		manual  bool // true → CreateRun (human); false → CreateAutopilotRun
		wantErr error
	}{
		{
			// The headline journey: a bug-only issue with no link, waiver on, started
			// by a human. Eligible via the non-primary label "bug", so the waiver
			// applies and the run succeeds with no Promote and no PRDLESS.
			name:   "manual bug-only, waiver on, succeeds",
			fs:     fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}, waivesPRDLink: true},
			labels: labelsJSON(t, "bug"),
			manual: true,
		},
		{
			// Same issue with the waiver turned off: the link gate bites again.
			name:    "manual bug-only, waiver off, refused",
			fs:      fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}, waivesPRDLink: false},
			labels:  labelsJSON(t, "bug"),
			manual:  true,
			wantErr: ErrNoPRDLink,
		},
		{
			// The non-primary qualifier: a primary-only issue with no link is still
			// refused even with the waiver on. The waiver is for issues eligible by a
			// label OTHER than the primary; the primary's own link requirement is
			// unchanged (add a link or PRDLESS).
			name:    "manual primary-only, waiver on, still refused",
			fs:      fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}, waivesPRDLink: true},
			labels:  labelsJSON(t, "PRD"),
			manual:  true,
			wantErr: ErrNoPRDLink,
		},
		{
			// A PRD+bug issue started by a human: eligible via the non-primary "bug",
			// so the waiver applies and it succeeds. This is the manual counterpart of
			// the autopilot guard case below — same labels, opposite outcome, the whole
			// point of the !autoApprove scope.
			name:   "manual PRD+bug, waiver on, succeeds",
			fs:     fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}, waivesPRDLink: true},
			labels: labelsJSON(t, "PRD", "bug"),
			manual: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{
				issueByID:       store.Issue{Title: "T", Labels: tc.labels, HasPrdLink: false},
				createRunResult: store.Run{ID: uuid.New()},
			}
			svc := New(fs, newBox(t), testParams())
			svc.SetSettings(tc.fs)

			var err error
			if tc.manual {
				_, err = svc.CreateRun(context.Background(), user, repo, 4, "desc", false, nil, nil)
			} else {
				_, err = svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc", false)
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
				t.Fatal("a waived run must reach CreateRun")
			}
		})
	}
}

// TestWaiverNeverStartsAnAutopilotRunLinkLess is PRD #196 M4 guard test 3, and the
// risk it guards (from the PRD's risk table) is that a waiver implemented WITHOUT the
// !autoApprove qualifier starts a link-less run UNATTENDED. Both cases go through
// CreateAutopilotRun (autoApprove=true), with the waiver ON, HasPrdLink=false and no
// PRDLESS, and BOTH must still return ErrNoPRDLink.
//
// The two cases fail through DIFFERENT qualifiers, which is why both exist:
//   - PRD+autopilot: eligible only via the PRIMARY, so the NON-PRIMARY qualifier
//     already refuses it. This is the case M4 test 2 (autopilot candidacy) also
//     covers indirectly, and it matches today's behaviour exactly.
//   - PRD+autopilot+bug: eligible via the NON-PRIMARY "bug", so the non-primary
//     qualifier ALONE would let it through — only the !autoApprove qualifier stops
//     it. This is the case the guard is really for; the manual counterpart above
//     (PRD+bug via CreateRun) succeeds, proving the two differ solely by the human.
func TestWaiverNeverStartsAnAutopilotRunLinkLess(t *testing.T) {
	user, repo := uuid.New(), uuid.New()

	cases := []struct {
		name   string
		labels []byte
	}{
		{
			name:   "PRD+autopilot, eligible only by the primary",
			labels: labelsJSON(t, "PRD", "autopilot"),
		},
		{
			name:   "PRD+autopilot+bug, eligible by a non-primary label",
			labels: labelsJSON(t, "PRD", "autopilot", "bug"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{
				issueByID:       store.Issue{Title: "T", Labels: tc.labels, HasPrdLink: false},
				createRunResult: store.Run{ID: uuid.New()},
			}
			svc := New(fs, newBox(t), testParams())
			// Waiver ON, "bug" eligible: the most permissive configuration, so only the
			// gate's own scoping can be refusing these.
			svc.SetSettings(fakeSettings{prdLabel: "PRD", eligibleLabels: []string{"bug"}, waivesPRDLink: true})

			// allowWithoutPRD=false: NO PRDLESS. Autopilot link-less runs remain
			// possible only via PRDLESS, and that path is unchanged; the waiver must
			// never be a second door into an unattended run.
			if _, err := svc.CreateAutopilotRun(context.Background(), user, repo, 4, "desc", false); err != ErrNoPRDLink {
				t.Fatalf("err = %v, want ErrNoPRDLink — the waiver must never start an autopilot run link-less", err)
			}
			if fs.createRunParams != nil {
				t.Fatalf("a refused autopilot run must never reach CreateRun, got %+v", fs.createRunParams)
			}
		})
	}
}

// TestEligibilityGatePrecedesTheLinkGate pins the ORDER, which is a user-facing
// property rather than an implementation detail: a non-eligible issue with no PRD
// link satisfies both failure conditions, and reporting it as a missing link would
// send someone off to add a prds/*.md file to an issue that is not uzi's.
//
// "documentation" is used deliberately: it is neither the primary nor in the default
// eligible set, so it cannot slip through the widened gate or the waiver.
func TestEligibilityGatePrecedesTheLinkGate(t *testing.T) {
	fs := &fakeStore{issueByID: store.Issue{Title: "T", Labels: labelsJSON(t, "documentation"), HasPrdLink: false}}
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "desc", false, nil, nil); err != ErrNotPRDIssue {
		t.Fatalf("err = %v, want ErrNotPRDIssue (not ErrNoPRDLink)", err)
	}
}
