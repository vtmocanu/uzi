package workersvc

// Pure-unit coverage for the two untrusted-input line formatters in claim_assembly.go
// that feed the self_improve avoid-set (issue #297) and the open-MR "what was proposed"
// context (PRD #686 D11): formatInflightLine and firstNonEmptyLine. Both render UNTRUSTED
// repo content (issue titles, plan/issue markdown) into single-line coordinate strings
// carried on the claim payload to the worker/model.
//
// The integration fixture (inflight_targets_test.go) drives a full svc.Claim over one
// under-300-byte fixture with substring assertions, so it never exercises the rune-boundary
// truncation, the %q anti-forgery quoting of an untrusted title, the issue-less kind
// fallback, multi-milestone joining, the malformed-milestones degrade, or any of
// firstNonEmptyLine's blank-skip / truncation branches. These direct white-box tests pin
// those, following the idiom of checkpoint_branch_contract_test.go (which tests the
// unexported checkpointBranch directly). All inputs are in-code literals, so there is no
// cross-module -count=1 concern. mustMilestonesJSON is shared from inflight_targets_test.go.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestFormatInflightLine(t *testing.T) {
	twoMilestones := mustMilestonesJSON(t,
		apitypes.Milestone{ID: "m1", Title: "First"},
		apitypes.Milestone{ID: "m2", Title: "Second"},
	)

	tests := []struct {
		name string
		run  store.Run
		want string
	}{
		{
			// The issue title is %q-quoted; kind and status trail in parens. Defect a
			// failing exact-match would catch: %q -> %s on the title, or dropping the
			// title append, yields a different string.
			name: "issue with title",
			run: store.Run{
				IssueIid:   pgtype.Int8{Int64: 42, Valid: true},
				IssueTitle: "Fix the bug",
				Kind:       "issue",
				Status:     "running",
			},
			want: `issue #42 "Fix the bug" (kind=issue, status=running)`,
		},
		{
			// IssueIid invalid (a self_improve/ci_fix row) leads with "<kind> run" and no
			// "#<iid>". Defect: swapping the if/else, or "%s run" -> "issue #%d", flips this.
			name: "issue-less kind falls back to <kind> run",
			run: store.Run{
				IssueIid: pgtype.Int8{Valid: false},
				Kind:     "self_improve",
				Status:   "running",
			},
			want: "self_improve run (kind=self_improve, status=running)",
		},
		{
			// Valid iid but empty title: the " %q" append is guarded by IssueTitle != "".
			// Defect: dropping that guard emits an empty quoted title `""`.
			name: "valid iid with empty title omits the quoted title",
			run: store.Run{
				IssueIid: pgtype.Int8{Int64: 3, Valid: true},
				Kind:     "issue",
				Status:   "queued",
			},
			want: "issue #3 (kind=issue, status=queued)",
		},
		{
			// %q of an untrusted title neutralizes an embedded newline (it renders as the
			// two characters backslash-n), so a hostile title cannot forge an extra avoid-set
			// line. Defect: %q -> %s emits a raw newline (see the count/validity checks below).
			name: "untrusted title newline is escaped, not raw",
			run: store.Run{
				IssueIid:   pgtype.Int8{Int64: 7, Valid: true},
				IssueTitle: "evil\ntitle",
				Kind:       "issue",
				Status:     "queued",
			},
			want: `issue #7 "evil\ntitle" (kind=issue, status=queued)`,
		},
		{
			// Two frozen milestones join with ';' between entries, each title %q-quoted.
			// Defect: dropping the `if i>0 { WriteByte(';') }` loses the separator.
			name: "two milestones join with a semicolon",
			run: store.Run{
				IssueIid:         pgtype.Int8{Int64: 10, Valid: true},
				IssueTitle:       "T",
				Kind:             "issue",
				Status:           "running",
				MilestonesFrozen: twoMilestones,
			},
			want: `issue #10 "T" (kind=issue, status=running) — milestones: m1 "First"; m2 "Second"`,
		},
		{
			// MilestonesFrozen is data a prior write left behind, not an invariant of this
			// read: malformed jsonb must degrade to no milestone tail, never crash. Defect:
			// a refactor that assumes the column always decodes (panic/partial parse) breaks
			// this clean end-state. Paired with the two-milestone case above, which proves
			// the tail DOES appear when the decode succeeds.
			name: "malformed milestones_frozen degrades to no tail",
			run: store.Run{
				IssueIid:         pgtype.Int8{Int64: 5, Valid: true},
				IssueTitle:       "t",
				Kind:             "issue",
				Status:           "running",
				MilestonesFrozen: []byte("{not json"),
			},
			want: `issue #5 "t" (kind=issue, status=running)`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatInflightLine(tc.run)
			if got != tc.want {
				t.Errorf("formatInflightLine =\n  %q\nwant\n  %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("formatInflightLine produced invalid UTF-8: %q", got)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("formatInflightLine must stay a single line, got a raw newline: %q", got)
			}
		})
	}
}

func TestFormatInflightLineTruncation(t *testing.T) {
	// (a) All-ASCII over-length line: the assembled string is ~440 bytes, cut at exactly
	// maxInflightLineLen (300). The prefix `issue #7 "` is 10 bytes, so 290 title bytes
	// survive. Defect a failing exact-match catches: dropping the truncation, or a wrong
	// bound, changes the surviving prefix or its length.
	asciiRun := store.Run{
		IssueIid:   pgtype.Int8{Int64: 7, Valid: true},
		IssueTitle: strings.Repeat("a", 400),
		Kind:       "issue",
		Status:     "running",
	}
	wantASCII := `issue #7 "` + strings.Repeat("a", 290)
	gotASCII := formatInflightLine(asciiRun)
	if gotASCII != wantASCII {
		t.Errorf("ASCII truncation: len=%d\n got=%q\nwant=%q", len(gotASCII), gotASCII, wantASCII)
	}
	// Independent length gate (kept unconditional, like the straddle cases below), so a
	// regression that returns the right prefix at the wrong length is caught on its own.
	if len(gotASCII) != maxInflightLineLen {
		t.Errorf("ASCII truncation: len(gotASCII)=%d, want %d", len(gotASCII), maxInflightLineLen)
	}

	// (b) A 3-byte rune (世 = E4 B8 96) placed so its bytes occupy indices 298-300: byte
	// 300, the initial cut point, is a continuation byte, so the rune walk-back must drop
	// the whole rune rather than slice it in half. The prefix is 10 bytes, so 288 filler
	// bytes put 世 at index 298; the walk-back leaves `issue #7 "` + 288 a's (298 bytes),
	// valid UTF-8. Defect: replacing the walk-back with line[:300] ships invalid UTF-8 and
	// a different prefix.
	straddleRun := store.Run{
		IssueIid:   pgtype.Int8{Int64: 7, Valid: true},
		IssueTitle: strings.Repeat("a", 288) + "世" + strings.Repeat("a", 50),
		Kind:       "issue",
		Status:     "running",
	}
	wantStraddle := `issue #7 "` + strings.Repeat("a", 288)
	got := formatInflightLine(straddleRun)
	if got != wantStraddle {
		t.Errorf("rune-straddle truncation:\n got=%q\nwant=%q", got, wantStraddle)
	}
	if !utf8.ValidString(got) {
		t.Errorf("rune-straddle truncation produced invalid UTF-8: %q", got)
	}
	if len(got) > maxInflightLineLen {
		t.Errorf("rune-straddle truncation: len(got)=%d exceeds %d", len(got), maxInflightLineLen)
	}
}

func TestFirstNonEmptyLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// Leading empty and whitespace-only lines are skipped; the first non-blank line
			// is returned trimmed. Defect: not skipping blanks returns "" (the first line).
			name: "skips leading blank and whitespace-only lines",
			in:   "\n   \n\tfirst real line\nsecond",
			want: "first real line",
		},
		{
			name: "trims surrounding whitespace on the returned line",
			in:   "   padded   \nnext",
			want: "padded",
		},
		{
			name: "all-blank input yields empty",
			in:   "\n  \n\t\n",
			want: "",
		},
		{
			name: "empty input yields empty",
			in:   "",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstNonEmptyLine(tc.in); got != tc.want {
				t.Errorf("firstNonEmptyLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFirstNonEmptyLineTruncation(t *testing.T) {
	// A 3-byte rune (世) straddling byte 300: the walk-back drops the whole rune, never
	// half of it, leaving 298 a's — valid UTF-8 bounded at a rune boundary. Defect:
	// replacing the walk-back with line[:300] ships invalid UTF-8 and a different result.
	straddle := strings.Repeat("a", 298) + "世" // 298 + 3 = 301 bytes, over the 300 bound
	wantStraddle := strings.Repeat("a", 298)
	got := firstNonEmptyLine(straddle)
	if got != wantStraddle {
		t.Errorf("rune-straddle:\n got=%q\nwant=%q", got, wantStraddle)
	}
	if !utf8.ValidString(got) {
		t.Errorf("rune-straddle produced invalid UTF-8: %q", got)
	}
	if len(got) != 298 {
		t.Errorf("rune-straddle: len(got)=%d, want 298", len(got))
	}

	// Boundary: a line of exactly maxInflightLineLen bytes is returned unchanged. The
	// guard is `len(line) > maxInflightLineLen` (strictly greater), so it is NOT entered.
	// Defect: a '>=' regression would enter and index line[300] out of range (panic).
	exact := strings.Repeat("a", maxInflightLineLen)
	if got := firstNonEmptyLine(exact); got != exact {
		t.Errorf("exact-bound line was altered: len(got)=%d, want %d unchanged", len(got), maxInflightLineLen)
	}
}
