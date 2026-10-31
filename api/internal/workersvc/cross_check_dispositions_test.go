package workersvc

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestExtractCrossCheckDispositions(t *testing.T) {
	long := strings.Repeat("a", crossCheckResponseMaxBytes+100)
	cases := []struct {
		name      string
		plan      string
		wantText  string
		wantState string
	}{
		{"no heading", "# Plan\n\nDo things.\n", "", "not_reported"},
		{"empty plan", "", "", "not_reported"},
		{"atx h2 to EOF", "# Plan\n\n## Cross-check dispositions\n\n- R1: fixed\n- R2: declined\n", "- R1: fixed\n- R2: declined", "reported"},
		{"atx h2 ends at next h2", "## Cross-check dispositions\nfixed\n## Milestones\nm1\n", "fixed", "reported"},
		{"atx h2 ends at higher level h1", "## Cross-check dispositions\nfixed\n# Next\nx\n", "fixed", "reported"},
		{"atx h2 keeps a deeper h3 subsection", "## Cross-check dispositions\nfixed\n### R1\ndetail\n## Milestones\nm1\n", "fixed\n### R1\ndetail", "reported"},
		{"atx h3 ends at the next h2", "### Cross-check dispositions\nfixed\n## Milestones\nm1\n", "fixed", "reported"},
		{"atx closing hashes", "## Cross-check dispositions ##\nfixed\n", "fixed", "reported"},
		{"atx bold title", "## **Cross-check dispositions**\nfixed\n", "fixed", "reported"},
		{"case-insensitive is NOT accepted", "## cross-check dispositions\nfixed\n", "", "not_reported"},
		{"bold form to EOF", "Plan\n\n**Cross-check dispositions**\n\n- R1: fixed\n", "- R1: fixed", "reported"},
		{"bold form ends at the next bold-only line", "**Cross-check dispositions**\nfixed\n**Milestones**\nm1\n", "fixed", "reported"},
		{"bold form ends at any atx heading", "**Cross-check dispositions**\nfixed\n### Detail\nx\n", "fixed", "reported"},
		{"bold form keeps inline bold lines", "**Cross-check dispositions**\n- **R1** fixed\n", "- **R1** fixed", "reported"},
		{"plain line is not a heading", "Cross-check dispositions\nfixed\n", "", "not_reported"},
		{"longer title is not the heading", "## Cross-check dispositions and notes\nfixed\n", "", "not_reported"},
		{"heading inside a backtick fence is ignored", "```\n## Cross-check dispositions\nfake\n```\n", "", "not_reported"},
		{"heading inside a tilde fence is ignored", "~~~md\n## Cross-check dispositions\nfake\n~~~\n", "", "not_reported"},
		{"a longer fence is not closed by a shorter one", "````\n```\n## Cross-check dispositions\nfake\n````\n", "", "not_reported"},
		{"a real heading after a closed fence", "```\ncode\n```\n## Cross-check dispositions\nreal\n", "real", "reported"},
		{"a heading in a fence inside the section does not end it", "## Cross-check dispositions\nfixed\n```\n## Not a heading\n```\nmore\n## Milestones\n", "fixed\n```\n## Not a heading\n```\nmore", "reported"},
		{"the first heading wins", "## Cross-check dispositions\nfirst\n## Cross-check dispositions\nsecond\n", "first", "reported"},
		{"older entries are kept", "## Cross-check dispositions\nRound 2: fixed\nRound 1: declined\n", "Round 2: fixed\nRound 1: declined", "reported"},
		{"empty section is not reported", "## Cross-check dispositions\n\n## Milestones\nm1\n", "", "not_reported"},
		{"CRLF line endings", "## Cross-check dispositions\r\nfixed\r\n## Milestones\r\n", "fixed", "reported"},
		{"over the cap is truncated", "## Cross-check dispositions\n" + long + "\n", long[:crossCheckResponseMaxBytes], "truncated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, state := ExtractCrossCheckDispositions(tc.plan)
			if text != tc.wantText || state != tc.wantState {
				t.Fatalf("got (%.60q, %q), want (%.60q, %q)", text, state, tc.wantText, tc.wantState)
			}
		})
	}
}

// The cap lands on a UTF-8 boundary: a multi-byte rune straddling the limit is dropped whole,
// never split into an invalid sequence.
func TestExtractCrossCheckDispositionsCapIsRuneSafe(t *testing.T) {
	// 'é' is two bytes. An odd prefix puts the rune across the 8192-byte boundary.
	body := "x" + strings.Repeat("é", crossCheckResponseMaxBytes)
	text, state := ExtractCrossCheckDispositions("## Cross-check dispositions\n" + body)
	if state != "truncated" {
		t.Fatalf("state %q", state)
	}
	if !utf8.ValidString(text) || len(text) > crossCheckResponseMaxBytes {
		t.Fatalf("valid=%v len=%d", utf8.ValidString(text), len(text))
	}
	if want := 1 + 2*((crossCheckResponseMaxBytes-1)/2); len(text) != want {
		t.Fatalf("len=%d, want %d (whole runes up to the cap)", len(text), want)
	}
	// A four-byte emoji straddling the cap is also dropped whole.
	body = strings.Repeat("a", crossCheckResponseMaxBytes-2) + "😀😀"
	text, state = ExtractCrossCheckDispositions("## Cross-check dispositions\n" + body)
	if state != "truncated" || !utf8.ValidString(text) || text != strings.Repeat("a", crossCheckResponseMaxBytes-2) {
		t.Fatalf("emoji cut: state=%q len=%d valid=%v", state, len(text), utf8.ValidString(text))
	}
}

func roundRow(round int32, verdict string, generation int64, plan string) store.ListPlanCrossCheckRoundsRow {
	return store.ListPlanCrossCheckRoundsRow{Round: round, Verdict: verdict, LeadClaimGeneration: generation, PlanMd: pgtype.Text{String: plan, Valid: true}}
}

// TestCrossCheckRoundResponseStates is one case per response state, plus the guards that keep
// a state from being inferred from the wrong rows.
func TestCrossCheckRoundResponseStates(t *testing.T) {
	dispositions := "plan\n\n## Cross-check dispositions\nR1: fixed\n"
	atCap := strings.Repeat("a", planCandidateNormalizationCap)
	presented := store.Run{PlanMd: pgtype.Text{String: "revised\n## Cross-check dispositions\nR1: done\n", Valid: true}, ClaimGeneration: 1}
	cases := []struct {
		name      string
		row       store.ListPlanCrossCheckRoundsRow
		following *store.ListPlanCrossCheckRoundsRow
		lead      *store.Run
		wantState string
		wantText  string
	}{
		{"reported", roundRow(1, "revise", 1, "p1"), rowPtr(roundRow(2, "approve", 1, dispositions)), nil, apitypes.PlanCrossCheckResponseReported, "R1: fixed"},
		{"truncated", roundRow(1, "revise", 1, "p1"), rowPtr(roundRow(2, "approve", 1, "## Cross-check dispositions\n"+strings.Repeat("b", 9000))), nil, apitypes.PlanCrossCheckResponseTruncated, strings.Repeat("b", crossCheckResponseMaxBytes)},
		{"not_reported", roundRow(1, "revise", 1, "p1"), rowPtr(roundRow(2, "approve", 1, "plan without heading")), nil, apitypes.PlanCrossCheckResponseNotReported, ""},
		{"unknown at the normalization cap", roundRow(1, "revise", 1, "p1"), rowPtr(roundRow(2, "approve", 1, atCap)), nil, apitypes.PlanCrossCheckResponseUnknown, ""},
		{"a heading found in a capped candidate is still reported", roundRow(1, "revise", 1, "p1"), rowPtr(roundRow(2, "approve", 1, "## Cross-check dispositions\nok\n## Milestones\n"+atCap)), nil, apitypes.PlanCrossCheckResponseReported, "ok"},
		{"replanned_after_interruption", roundRow(1, "revise", 1, "p1"), rowPtr(roundRow(2, "approve", 2, dispositions)), nil, apitypes.PlanCrossCheckResponseReplannedAfterInterrupted, ""},
		{"reported_in_presented_plan", roundRow(1, "revise", 1, "p1"), nil, &presented, apitypes.PlanCrossCheckResponseReportedInPresentedPlan, "R1: done"},
		{"presented plan without a heading is not reported", roundRow(1, "revise", 1, "p1"), nil, &store.Run{PlanMd: pgtype.Text{String: "revised", Valid: true}, ClaimGeneration: 1}, apitypes.PlanCrossCheckResponseNotReported, ""},
		{"no_following_candidate: nothing presented", roundRow(1, "revise", 1, "p1"), nil, nil, apitypes.PlanCrossCheckResponseNoFollowingCandidate, ""},
		{"no_following_candidate: presented equals the round", roundRow(1, "revise", 1, presented.PlanMd.String), nil, &presented, apitypes.PlanCrossCheckResponseNoFollowingCandidate, ""},
		{"no_following_candidate: human revised", roundRow(1, "revise", 1, "p1"), nil, &store.Run{PlanMd: presented.PlanMd, ClaimGeneration: 1, ReviseCount: 1}, apitypes.PlanCrossCheckResponseNoFollowingCandidate, ""},
		{"no_following_candidate: other generation", roundRow(1, "revise", 1, "p1"), nil, &store.Run{PlanMd: presented.PlanMd, ClaimGeneration: 2}, apitypes.PlanCrossCheckResponseNoFollowingCandidate, ""},
		{"no_following_candidate: only a decided REVISE has findings to answer", roundRow(1, "approve", 1, "p1"), nil, &presented, apitypes.PlanCrossCheckResponseNoFollowingCandidate, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := crossCheckRoundResponse(tc.row, tc.following, tc.lead)
			if got.State != tc.wantState || got.Text != tc.wantText {
				t.Fatalf("got (%q, %.60q), want (%q, %.60q)", got.State, got.Text, tc.wantState, tc.wantText)
			}
		})
	}
}

func rowPtr[T any](v T) *T { return &v }
