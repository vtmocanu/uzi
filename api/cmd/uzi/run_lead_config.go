package main

// run_lead_config.go implements `uzi run lead-config <run-id> [--json]` (PRD #3244 M3): the history
// of the lead agent's model and effort for a run, one row per recorded phase entry across every
// claim attempt, newest attempt first. Every model string and label is worker-attested or
// server text, so each cell goes through sanitizeTTY before it is printed.

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runpresentation"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// leadConfigListTruncatedNote is the visible truncation line (the server keeps the first 256 rows of the
// newest-attempt-first order and drops the tail), and leadConfigRowTruncatedSuffix matches the run
// header tooltip ("later changes not recorded").
const (
	leadConfigRowTruncatedSuffix = " (later changes not recorded)"
	leadConfigListTruncatedNote  = "showing the first 256 entries (newest attempt first); the rest are not listed"
)

func newRunLeadConfigCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "lead-config <run-id>",
		Short: "Show the lead model and effort history of a run",
		Long: "List every recorded lead-agent configuration of a run: the model and effort that were requested " +
			"and where each came from, and what the runtime reported, per claim attempt (GEN), phase and phase " +
			"entry. The newest attempt is listed first, plan before implement.\n\n" +
			"A run without a lead agent, or an attempt whose worker did not record, has no rows. " +
			"At most 256 entries are listed; a note says when the rest were left out. --json prints " +
			"the server response ({entries, truncated}).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			got, err := c.RunLeadConfigHistory(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return renderLeadConfigHistory(env, gf, args[0], got)
		},
	}
}

// renderLeadConfigHistory prints the history: --json the response (entries never null), the human
// form one row per entry plus a truncation note when the server capped the list.
func renderLeadConfigHistory(env Env, gf *globalFlags, runID string, h apitypes.LeadConfigHistoryDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		if h.Entries == nil {
			h.Entries = []apitypes.LeadConfigHistoryEntryDTO{}
		}
		return p.JSON(h)
	}
	if len(h.Entries) == 0 {
		if !gf.quiet {
			p.Printf("run %s has no recorded lead config\n", sanitizeTTY(runID))
		}
		return nil
	}
	rows := make([][]string, 0, len(h.Entries))
	for _, e := range h.Entries {
		cells := runpresentation.LeadHistoryRowCells(e.LeadPhaseConfigDTO)
		reported := e.ReportedAt.UTC().Format(time.RFC3339)
		if e.Truncated {
			reported += leadConfigRowTruncatedSuffix
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", e.ClaimGeneration),
			leadCell(e.Phase),
			fmt.Sprintf("%d", e.PhaseEntry),
			fmt.Sprintf("%d", e.Seq),
			leadCell(cells.Requested),
			leadCell(cells.Source),
			leadCell(cells.Observed),
			leadCell(cells.Evidence),
			reported,
		})
	}
	if err := p.Table([]string{"GEN", "PHASE", "ENTRY", "SEQ", "REQUESTED", "SOURCE", "OBSERVED", "EVIDENCE", "REPORTED"}, rows); err != nil {
		return err
	}
	if h.Truncated {
		p.Printf("\n%s\n", leadConfigListTruncatedNote)
	}
	return nil
}

// leadCell makes one untrusted string safe for a single table cell: terminal-unsafe characters are
// folded by sanitizeTTY and line breaks and tabs by cellText.
func leadCell(s string) string {
	return strings.TrimSpace(cellText(sanitizeTTY(s)))
}
