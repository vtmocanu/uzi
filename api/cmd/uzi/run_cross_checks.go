package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newRunCrossChecksCmd(env Env, gf *globalFlags) *cobra.Command {
	var opts uzicli.CrossCheckHistoryOptions
	cmd := &cobra.Command{
		Use:   "cross-checks",
		Short: "Show one page of plan and code cross-check history",
		Long: "Show one bounded page of cross-check history grouped by parent run. " +
			"Use --cursor for more groups, or repeat --attempt-cursor for up to eight older histories. " +
			"Keep lifecycle, stage and outcome filters unchanged when continuing. Cursors are never followed automatically.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Lifecycle == "" || opts.Stage == "" || opts.Outcome == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "lifecycle, stage and outcome must be nonempty")
			}
			if opts.Limit < 1 || opts.AttemptLimit < 2 ||
				(cmd.Flags().Changed("cursor") && opts.Cursor == "") {
				return uzicli.Exitf(uzicli.ExitUsage, "limit must be 1..25, attempt-limit 2..20, and provided cursors nonempty")
			}
			if err := opts.Validate(); err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			page, err := c.CrossCheckHistory(cmd.Context(), opts)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(page)
			}
			return printCrossCheckHistory(p, page, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Lifecycle, "lifecycle", "active", "Group lifecycle: active or past (independent of parent status)")
	cmd.Flags().StringVar(&opts.Stage, "stage", "all", "Stage: all, plan or code")
	cmd.Flags().StringVar(&opts.Outcome, "outcome", "any", "Past only: didnt_finish, raised_issues, passed or replaced; otherwise any")
	cmd.Flags().StringVar(&opts.Cursor, "cursor", "", "Next group page cursor; cannot combine with attempt-cursor")
	cmd.Flags().IntVar(&opts.Limit, "limit", 25, "Maximum groups per page (1..25)")
	cmd.Flags().StringArrayVar(&opts.AttemptCursors, "attempt-cursor", nil, "Older attempts cursor; repeat up to eight times, without cursor")
	cmd.Flags().IntVar(&opts.AttemptLimit, "attempt-limit", 10, "Maximum attempts per parent (2..20)")
	return cmd
}

// crossCheckText scrubs the complete single-line value before bounding human prose.
func crossCheckText(s string) string {
	s = secretscrub.Scrub(termsafe.CellText(s))
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return s
}

func crossCheckLabel(s string, labels map[string]string) string {
	if label, ok := labels[s]; ok {
		return label
	}
	return "Unknown"
}

func crossCheckStatus(s string) string {
	return crossCheckLabel(s, map[string]string{
		"queued": "Queued", "claimed": "Claimed", "running": "Running",
		"awaiting_approval": "Awaiting approval", "completed": "Completed",
		"awaiting_input": "Awaiting input", "awaiting_followup": "Awaiting follow-up",
		"failed": "Failed", "cancelled": "Cancelled", "paused": "Paused",
		"limit_wait": "Limit wait", "pool_wait": "Pool wait", "recovery_wait": "Recovery wait",
	})
}

func printCrossCheckHistory(p *uzicli.Printer, page apitypes.CrossCheckHistoryDTO, opts uzicli.CrossCheckHistoryOptions) error {
	p.Printf("Cross-checks: lifecycle %s, stage %s, outcome %s\n", crossCheckText(opts.Lifecycle), crossCheckText(opts.Stage), crossCheckText(opts.Outcome))
	p.Printf("Groups on this page: %d\n", len(page.Groups))
	if len(page.Groups) == 0 {
		p.Printf("No cross-check history on this page.\n")
	}
	for _, group := range page.Groups {
		lifecycle := "Past"
		if group.Active {
			lifecycle = "Active"
		}
		p.Printf("\nParent: %s: %s\n", crossCheckText(group.ParentID), crossCheckText(group.ParentTitle))
		p.Printf("Parent lifecycle: %s\nGroup lifecycle: %s\n", crossCheckStatus(group.ParentStatus), lifecycle)
		p.Printf("Attempts shown on this page: %d; matching retained: %d; filtered out: %d; total retained: %d\n",
			len(group.Attempts), group.MatchingAttempts, max(int64(0), group.TotalAttempts-group.MatchingAttempts), group.TotalAttempts)
		if group.ActiveHidden > 0 {
			p.Printf("Hidden active attempts: %d: active attempts may be outside this page or excluded by filters.\n", group.ActiveHidden)
		}
		if group.ActiveContext != "" {
			p.Printf("Active context: %s\n", crossCheckText(group.ActiveContext))
		}
		attempts := append([]apitypes.CrossCheckHistoryAttemptDTO(nil), group.Attempts...)
		// The page remains bounded by the API envelope. Sort only its attempts,
		// with Plan before Code and ascending rounds within each stage.
		sort.SliceStable(attempts, func(i, j int) bool {
			rank := func(stage string) int {
				switch stage {
				case "plan":
					return 0
				case "code":
					return 1
				default:
					return 2
				}
			}
			if rank(attempts[i].Stage) != rank(attempts[j].Stage) {
				return rank(attempts[i].Stage) < rank(attempts[j].Stage)
			}
			return attempts[i].Round < attempts[j].Round
		})
		for _, attempt := range attempts {
			if err := printCrossCheckAttempt(p, attempt); err != nil {
				return err
			}
		}
		if len(attempts) == 0 {
			p.Printf("No attempts on this page.\n")
		}
		if group.Omission != "" || group.OlderCursor != "" {
			p.Printf("Earlier rounds omitted: load older attempts\n")
		}
		if group.Omission != "" {
			p.Printf("Omission: %s\n", crossCheckText(group.Omission))
		}
		if group.RefreshNeeded {
			p.Printf("Refresh needed: reload without cursors using the same filters.\n")
		}
		if group.OlderCursor != "" {
			printCrossCheckCursor(p, "--attempt-cursor", group.OlderCursor, opts)
		}
	}
	if page.NextCursor != "" {
		printCrossCheckCursor(p, "--cursor", page.NextCursor, opts)
	}
	return nil
}

func printCrossCheckAttempt(p *uzicli.Printer, a apitypes.CrossCheckHistoryAttemptDTO) error {
	stage := crossCheckLabel(a.Stage, map[string]string{"plan": "Plan", "code": "Code"})
	execution := crossCheckLabel(a.Execution, map[string]string{
		"pending": "Pending", "running": "Running", "finished": "Finished",
		"didnt_finish": "Didn't finish", "couldnt_start": "Couldn't start",
		"replaced": "Replaced", "interrupted": "Interrupted",
	})
	verdict := crossCheckLabel(a.Review, map[string]string{
		"none": "No verdict recorded", "approved": "Approved",
		"changes_requested": "Changes requested", "blocked": "Blocked",
	})
	p.Printf("%s round %d\n", stage, a.Round)
	reason := a.Reason
	if strings.TrimSpace(termsafe.CellText(reason)) == "" {
		reason = "Cause not recorded for this attempt"
	}
	effect := a.Effect
	if strings.TrimSpace(termsafe.CellText(effect)) == "" {
		effect = "Effect not recorded"
	}
	action := a.NextAction
	if strings.TrimSpace(termsafe.CellText(action)) == "" {
		action = "No action recorded"
	}
	if err := p.Table(nil, [][]string{
		{"Check:", execution},
		{"Verdict:", verdict},
		{"Effect on run:", crossCheckText(effect)},
	}); err != nil {
		return err
	}
	p.Printf("Cause: %s\nNext action: %s\n", crossCheckText(reason), crossCheckText(action))
	if a.Historical {
		if a.Stage == "plan" && a.Review == "approved" {
			p.Printf("Historical: earlier candidate evidence\n")
		} else {
			p.Printf("Historical: retained check evidence\n")
		}
	}
	if a.ChildID == nil || *a.ChildID == "" {
		p.Printf("No checker run available\n")
	} else {
		status := "Unknown"
		if a.ChildStatus != nil {
			status = crossCheckStatus(*a.ChildStatus)
		}
		p.Printf("Checker run: %s: %s\n", crossCheckText(*a.ChildID), status)
		if a.ChildContext != "" {
			p.Printf("Checker context: %s\n", crossCheckText(a.ChildContext))
		}
	}
	if a.Model != nil {
		p.Printf("Model: %s\n", crossCheckText(*a.Model))
	}
	if a.Effort != nil {
		p.Printf("Effort: %s\n", crossCheckText(*a.Effort))
	}
	if a.FindingsCount != nil {
		p.Printf("Findings: %d\n", *a.FindingsCount)
	}
	return nil
}

func printCrossCheckCursor(p *uzicli.Printer, flag, cursor string, opts uzicli.CrossCheckHistoryOptions) {
	// Opaque tokens must not use the prose cap or secret redactor: valid tokens
	// can be 512 bytes. Shell quoting preserves punctuation in copyable values.
	token := termsafe.CellText(cursor)
	token = "'" + strings.ReplaceAll(token, "'", "'\"'\"'") + "'"
	p.Printf("Continue: uzi run cross-checks --lifecycle %s --stage %s --outcome %s --limit %d --attempt-limit %d %s %s\n",
		crossCheckText(opts.Lifecycle), crossCheckText(opts.Stage), crossCheckText(opts.Outcome),
		opts.Limit, opts.AttemptLimit, flag, token)
}
