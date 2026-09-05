package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
)

// TestDefaultEditableDivergesOutputMode pins PRD #929 M1's output_mode arm of the
// Customized-divergence check for a default prompt schedule. The catalog baseline is
// job.OutputMode(); a NULL stored column (Valid=false) means "inherit the catalog default"
// and so must NOT flag divergence, an explicit value equal to the baseline must NOT diverge,
// and an explicit value differing from the baseline MUST flag Customized. Every other
// editable field is held at its catalog baseline so output_mode alone drives the result.
func TestDefaultEditableDivergesOutputMode(t *testing.T) {
	// Prompt job whose baseline output mode is "issues" (frontmatter `output: issues`).
	job := schedtmpl.DefaultJob{
		Slug:     "example-prompt",
		Target:   "prompt",
		Cron:     "0 9 * * 1",
		Timezone: schedtmpl.DefaultTimezone,
		Output:   schedtmpl.OutputModeIssues,
	}
	if job.OutputMode() != schedtmpl.OutputModeIssues {
		t.Fatalf("baseline OutputMode() = %q, want %q", job.OutputMode(), schedtmpl.OutputModeIssues)
	}

	// Helper: call with every non-output field pinned to the baseline so only outputMode moves.
	diverges := func(outputMode pgtype.Text) bool {
		return defaultEditableDiverges(
			job,
			job.Cron,
			job.Timezone,
			pgtype.Text{}, // model: inherit (== job.Model "")
			schedtmpl.AutoApprove,
			schedtmpl.WaitOnLimit,
			pgtype.Bool{}, // mr_rework: inherit
			pgtype.Int4{}, // max_issues: unset (prompt has no cap)
			outputMode,
		)
	}

	// NULL stored column inherits the catalog default => not customized.
	if diverges(pgtype.Text{}) {
		t.Errorf("NULL output_mode: diverges=true, want false (inherits catalog default)")
	}
	// Explicit value EQUAL to the baseline => not customized.
	if diverges(pgtype.Text{String: schedtmpl.OutputModeIssues, Valid: true}) {
		t.Errorf("output_mode==baseline: diverges=true, want false")
	}
	// Explicit value DIFFERING from the baseline => customized.
	if !diverges(pgtype.Text{String: schedtmpl.OutputModeMR, Valid: true}) {
		t.Errorf("output_mode!=baseline: diverges=false, want true (Customized)")
	}
}
