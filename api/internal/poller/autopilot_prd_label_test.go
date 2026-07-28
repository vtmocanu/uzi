package poller

import (
	"context"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// Autopilot's PRD-label predicate (PRD #102 M6, Decision 11b).
//
// Before M6 the candidate query filtered on the autopilot label alone, and its own
// comment recorded why that was enough: the sync filter meant every cached issue
// carried the PRD label. The additive fetch deletes that invariant, and the
// consequence is not cosmetic — a stranger's open issue carrying the autopilot
// label would become a candidate, and the detector would then read its label
// events over the forge every tick and either start an unattended run on it or
// post a comment on it.
//
// What a fake store CAN show is that the resolved label reaches the query. What it
// cannot show is the filtering, because the filtering is SQL: that lives in
// store/autopilot_candidates_integration_test.go, against a real server.

func TestDetectFiltersCandidatesOnBothLabels(t *testing.T) {
	cases := []struct {
		name string
		lab  apLabeler
		want string
	}{
		{
			name: "the configured PRD label is passed to the query",
			lab:  apLabeler{label: apLabel, prdLabel: "Feature"},
			want: "Feature",
		},
		{
			// Unconfigured settings must narrow to the default, never widen to no
			// filter. Every other outcome of a settings blip is autopilot not firing;
			// this one would be autopilot firing on issues that are not uzi's.
			name: "an unconfigured PRD label falls back to the compiled-in default",
			lab:  apLabeler{label: apLabel},
			want: settings.DefaultPRDLabel,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &apStore{}
			NewAutopilot(st, &apRuns{}, tc.lab).detect(context.Background(), repoRow(), &apForge{})

			if len(st.candParams) != 1 {
				t.Fatalf("expected one candidate query, got %d", len(st.candParams))
			}
			if got := st.candParams[0].PrdLabel; got != tc.want {
				t.Fatalf("PrdLabel = %q, want %q", got, tc.want)
			}
			if got := st.candParams[0].Label; got != apLabel {
				t.Fatalf("Label = %q, want the autopilot label %q — the PRD predicate is ADDITIONAL, not a replacement", got, apLabel)
			}
		})
	}
}

// TestDetectFallsBackWithoutASettingsReader covers the nil-reader path, which
// resolves both labels from compiled-in defaults.
func TestDetectFallsBackWithoutASettingsReader(t *testing.T) {
	st := &apStore{}
	NewAutopilot(st, &apRuns{}, nil).detect(context.Background(), repoRow(), &apForge{})

	if len(st.candParams) != 1 {
		t.Fatalf("expected one candidate query, got %d", len(st.candParams))
	}
	if got := st.candParams[0].PrdLabel; got != settings.DefaultPRDLabel {
		t.Fatalf("PrdLabel = %q, want %q", got, settings.DefaultPRDLabel)
	}
}

// TestNotPRDIssueIsRecordedAndSilent covers the run-create rejection Decision 14
// introduced. The candidate query already excludes non-PRD issues, so reaching
// this case means the label was removed between the query and the create — but the
// case must exist, because without it the error falls to default:, which records
// nothing and re-evaluates the issue on every tick. That is one
// ListIssueLabelEvents forge call per issue per minute, indefinitely.
//
// The two assertions are the two halves of Decision 11b:
//   - the event id IS recorded, which is what ends the re-evaluation loop;
//   - NO comment is posted, because an outward-facing write on an issue that is
//     not uzi's is precisely the outcome the decision exists to prevent.
func TestNotPRDIssueIsRecordedAndSilent(t *testing.T) {
	st := &apStore{
		candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")},
		cc:         ccOwner("alice", true, true),
	}
	f := &apForge{events: map[int64][]forge.LabelEvent{7: {addEvt(11, "alice")}}}
	runs := &apRuns{err: workersvc.ErrNotPRDIssue}

	detectWith(st, runs, f)

	if len(runs.calls) != 1 {
		t.Fatalf("expected the create to be attempted once, got %d", len(runs.calls))
	}
	up := lastUpsert(t, st)
	if up.IssueIid != 7 || up.LastEventID != 11 {
		t.Fatalf("recorded trigger = %+v, want issue 7 at event 11 — an unrecorded event re-evaluates every tick", up)
	}
	if len(f.notes) != 0 {
		t.Fatalf("no comment may be posted on an issue that is not uzi's, got %+v", f.notes)
	}
}
