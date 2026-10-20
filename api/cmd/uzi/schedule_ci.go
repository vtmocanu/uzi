package main

// ci_watch (default-branch CI repair, PRD #2865) pieces of the schedule commands: the
// workflow-selector flag helpers, the episode listing seam and the detail rendering.

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const (
	// ciAutoApproveNote is printed for every ci_watch schedule in `schedule get`.
	ciAutoApproveNote = "Auto-approve is off by default for CI repair: CI logs can contain text from dependencies, tests or outside contributors. Review the plan before the run changes code."
	// ciAllWorkflowsNote describes an empty workflow selector.
	ciAllWorkflowsNote = "all workflows in the listed window (newest 100 executions; 50 on Forgejo)"
)

// ciWorkflowFlags collects --workflow values (trimmed); a blank entry is a usage error.
func ciWorkflowFlags(cmd *cobra.Command) ([]string, error) {
	raw, _ := cmd.Flags().GetStringArray("workflow")
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		w = strings.TrimSpace(w)
		if w == "" {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "--workflow needs a workflow key or name")
		}
		out = append(out, w)
	}
	return out, nil
}

// applyCIWorkflowEditFlags overlays --workflow / --clear-workflows onto an edit request.
// Absence sends nothing (the server keeps the stored selector); --clear-workflows sends
// ci_workflows: null. Returns whether the request changed.
func applyCIWorkflowEditFlags(cmd *cobra.Command, target string, req *apitypes.ScheduleRequest) (bool, error) {
	artifactChanged, artifactErr := applyCIArtifactEditFlags(cmd, target, req)
	if artifactErr != nil {
		return false, artifactErr
	}
	f := cmd.Flags()
	set, clr := f.Changed("workflow"), f.Changed("clear-workflows")
	if !set && !clr {
		return artifactChanged, nil
	}
	if target != schedTargetCIWatch {
		return false, uzicli.Exitf(uzicli.ExitUsage, "--workflow/--clear-workflows are only valid on a ci_watch schedule")
	}
	if set && clr {
		return false, uzicli.Exitf(uzicli.ExitUsage, "--workflow and --clear-workflows are mutually exclusive")
	}
	if clr {
		req.CIWorkflows = apitypes.OptionalCIWorkflows{Present: true, Value: nil}
		return true, nil
	}
	ws, err := ciWorkflowFlags(cmd)
	if err != nil {
		return false, err
	}
	req.CIWorkflows = apitypes.OptionalCIWorkflows{Present: true, Value: ws}
	return true, nil
}

// renderCIWatchDetail prints the ci_watch section of `schedule get`.
func renderCIWatchDetail(p *uzicli.Printer, s apitypes.ScheduleDTO) {
	p.Println("")
	if len(s.CIWorkflows) == 0 {
		p.Printf("Watching: %s\n", ciAllWorkflowsNote)
	} else {
		p.Printf("Watching: %s\n", strings.Join(s.CIWorkflows, ", "))
	}
	if len(s.CIArtifacts) == 0 {
		p.Println("Artifacts: none")
	} else {
		p.Printf("Artifacts: %s\n", strings.Join(s.CIArtifacts, ", "))
	}
	p.Println(ciAutoApproveNote)
	if s.CIEpisodes != nil {
		renderCIEpisodes(p, *s.CIEpisodes)
	}
}

// renderCIEpisodes prints one page of episode state plus the next-cursor line.
func renderCIEpisodes(p *uzicli.Printer, page apitypes.CIEpisodePage) {
	if len(page.Episodes) == 0 {
		p.Println("Episodes: none")
	} else {
		p.Println("Episodes:")
		rows := make([][]string, 0, len(page.Episodes))
		for _, e := range page.Episodes {
			rows = append(rows, []string{
				e.WorkflowKey,
				ciEpisodeStatus(e),
				fmt.Sprintf("%d", e.Attempts),
				ciInt64(e.ObservedRunID),
				ciInt64(e.LastHandledRunID),
				e.UpdatedAt.UTC().Format(time.RFC3339),
			})
		}
		_ = p.Table([]string{"WORKFLOW", "STATE", "ATTEMPTS", "OBSERVED_RUN", "HANDLED_RUN", "UPDATED"}, rows)
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		p.Printf("next cursor: %s\n", string(*page.NextCursor))
	}
}

func ciEpisodeStatus(e apitypes.CIEpisodeDTO) string {
	if e.HaltedReason != nil && *e.HaltedReason != "" {
		return "halted: " + *e.HaltedReason
	}
	if e.Attempts > 0 {
		return fmt.Sprintf("repair attempted (%d)", e.Attempts)
	}
	if e.LastSignature == nil || *e.LastSignature == "" {
		return "passing again"
	}
	return "red observed"
}

func ciInt64(v *int64) string { return int64Or(v, "-") }

// runScheduleEpisodesPage implements `schedule get <id> --episodes-cursor <c>`.
func runScheduleEpisodesPage(cmd *cobra.Command, p *uzicli.Printer, c uzicli.Client, id, cursor string) error {
	page, err := c.ListScheduleCIEpisodes(cmd.Context(), id, cursor)
	if err != nil {
		return err
	}
	if p.Format == uzicli.FormatJSON {
		return p.JSON(page)
	}
	renderCIEpisodes(p, page)
	return nil
}

func applyCIArtifactEditFlags(cmd *cobra.Command, target string, req *apitypes.ScheduleRequest) (bool, error) {
	f := cmd.Flags()
	set, clr := f.Changed("artifact"), f.Changed("clear-artifacts")
	if !set && !clr {
		return false, nil
	}
	if target != schedTargetCIWatch {
		return false, uzicli.Exitf(uzicli.ExitUsage, "--artifact/--clear-artifacts are only valid on a ci_watch schedule")
	}
	if set && clr {
		return false, uzicli.Exitf(uzicli.ExitUsage, "--artifact and --clear-artifacts are mutually exclusive")
	}
	if clr {
		req.CIArtifacts = apitypes.OptionalCIArtifacts{Present: true, Value: nil}
		return true, nil
	}
	ws, err := ciArtifactFlags(cmd)
	if err != nil {
		return false, err
	}
	req.CIArtifacts = apitypes.OptionalCIArtifacts{Present: true, Value: ws}
	return true, nil
}

func ciArtifactFlags(cmd *cobra.Command) ([]string, error) {
	raw, _ := cmd.Flags().GetStringArray("artifact")
	clean, err := ciartifacts.NormalizeSelectors(raw)
	if err != nil {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "%s", err)
	}
	return clean, nil
}
