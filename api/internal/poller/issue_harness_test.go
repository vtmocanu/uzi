package poller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestAutopilotFreshHarnessLabels(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		labels                            []string
		token, consent                    bool
		owner                             string
		resolveErr                        error
		wantHarness                       workersvc.Harness
		wantFetch, wantComment, wantRetry bool
		reason                            string
	}{
		{"codex_without_anthropic", []string{"harness::codex"}, false, true, "alice", nil, workersvc.HarnessCodex, true, false, false, ""},
		{"claude_missing_no_fallback", []string{"harness::claude"}, false, true, "alice", workersvc.ErrNoCredentialForHarness, "", true, false, true, "no_credential_for_harness"},
		{"codex_disabled", []string{"harness::codex"}, true, true, "alice", workersvc.ErrHarnessCredentialDisabled, "", true, false, true, ""},
		{"no_label_no_anthropic", nil, false, true, "alice", nil, "", true, true, false, ""},
		{"no_label_legacy", nil, true, true, "alice", nil, "", true, false, false, ""},
		{"opt_out", []string{"harness::codex"}, false, false, "alice", nil, "", false, true, false, ""},
		{"identity_mismatch", []string{"harness::codex"}, false, true, "other", nil, "", false, true, false, ""},
		{"both", []string{"harness::codex", "harness::claude"}, true, true, "alice", nil, "", true, false, true, "harness_label_invalid"},
		{"unknown", []string{"harness::unknown"}, true, true, "alice", nil, "", true, false, true, "harness_label_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(prev)
			st := &apStore{candidates: []store.ListAutopilotCandidateIssuesRow{candIssue(7, "alice")}, cc: ccOwner(tc.owner, tc.consent, tc.token)}
			runs := &apRuns{resolveErr: tc.resolveErr}
			f := &apForge{issue: forge.Issue{Description: "fresh", Labels: tc.labels}, events: map[int64][]forge.LabelEvent{7: {addEvt(100, "alice")}}}
			detectWith(st, runs, f)
			wantRuns := 0
			if tc.wantHarness != "" || tc.name == "no_label_legacy" {
				wantRuns = 1
			}
			if len(runs.calls) != wantRuns {
				t.Fatalf("calls=%+v logs=%s", runs.calls, &logs)
			}
			if wantRuns == 1 {
				explicit := runs.calls[0].explicit
				if tc.wantHarness == "" && explicit != nil || tc.wantHarness != "" && (explicit == nil || *explicit != tc.wantHarness) {
					t.Fatalf("explicit=%v", explicit)
				}
			}
			if (f.fetches > 0) != tc.wantFetch || (len(f.notes) > 0) != tc.wantComment || (len(st.upserts) == 0) != tc.wantRetry {
				t.Fatalf("fetches=%d notes=%v upserts=%v", f.fetches, f.notes, st.upserts)
			}
			if tc.wantRetry && !strings.Contains(logs.String(), "poller: autopilot create run") {
				t.Fatalf("missing refusal log: %s", &logs)
			}
			if tc.reason != "" && !strings.Contains(logs.String(), tc.reason) {
				t.Fatalf("missing reason %s: %s", tc.reason, &logs)
			}
			if tc.wantHarness != "" && (len(runs.resolutions) != 1 || *runs.resolutions[0] != tc.wantHarness) {
				t.Fatalf("resolution=%v", runs.resolutions)
			}
		})
	}
}
