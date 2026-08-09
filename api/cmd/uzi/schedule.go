package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// Schedule target + timing wire strings (apitypes.ScheduleRequest.Target/Timing).
// Named so a typo is a compile error rather than a silent server 400.
const (
	schedTargetIssue  = "issue"
	schedTargetSweep  = "sweep"
	schedTargetPrompt = "prompt"

	schedTimingOnce      = "once"
	schedTimingRecurring = "recurring"
)

// atLayouts are the timestamp forms `--at` accepts, tried in order. RFC3339 is the
// canonical one; the minute-precision variant (no seconds) is accepted because that
// is the shape a human writes and the mock's own example uses
// ("2026-08-08T09:00+03:00"). The parsed instant is forwarded as-is; the server is
// the authority on "must be in the future" (422).
var atLayouts = []string{time.RFC3339, "2006-01-02T15:04Z07:00"}

// newScheduleCmd — `uzi schedule` and its verbs, the CLI twin of the web Schedules
// surface (PRD #241 M6). It mirrors `uzi run` throughout: a Client per command,
// `--json` dumping the raw DTO, and every failure an *ExitError carrying a documented
// exit code. A schedule fires through the same shared run-creation seam a manual
// `run create` uses, so it can do nothing a manual start cannot.
func newScheduleCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Create and manage time-driven run schedules",
	}

	cmd.AddCommand(
		newScheduleCreateCmd(env, gf),
		newScheduleListCmd(env, gf),
		newScheduleGetCmd(env, gf),
		newSchedulePauseCmd(env, gf),
		newScheduleResumeCmd(env, gf),
		newScheduleRunNowCmd(env, gf),
		newScheduleDeleteCmd(env, gf),
	)
	return cmd
}

func newScheduleCreateCmd(env Env, gf *globalFlags) *cobra.Command {
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a one-time or recurring schedule on a repo",
		Long: "Create a schedule that starts run(s) at future time(s). Pick exactly one\n" +
			"TARGET — --issue <iid> (a pinned issue), --sweep (every eligible issue matching\n" +
			"the --label selector, default the PRD label), or --prompt <text> (an issue-less\n" +
			"repo→MR run) — and exactly one TIMING — --at <RFC3339> (fires once) or --cron\n" +
			"<expr> (recurring, interpreted in --tz).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, repoID, err := buildScheduleRequest(cmd)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			s, err := c.CreateSchedule(cmd.Context(), repoID, req)
			if err != nil {
				return err
			}
			return renderCreatedSchedule(env, gf, s)
		},
	}
	create.Flags().String("repo", "", "repo id to schedule against (see 'uzi repo list')")
	create.Flags().Int64("issue", 0, "pinned issue IID target (one of --issue/--sweep/--prompt)")
	create.Flags().Bool("sweep", false, "label-sweep target: every eligible matching issue (one of --issue/--sweep/--prompt)")
	create.Flags().String("prompt", "", "ad-hoc prompt target: an issue-less repo→MR run (one of --issue/--sweep/--prompt)")
	create.Flags().StringArray("label", nil, "a label to select for --sweep (repeatable; empty defaults to the PRD label)")
	create.Flags().String("at", "", "fire once at this RFC3339 time (one of --at/--cron)")
	create.Flags().String("cron", "", "recurring 5-field cron expression (one of --at/--cron)")
	create.Flags().String("tz", "UTC", "IANA timezone the --cron expression is interpreted in")
	create.Flags().Bool("auto-approve", true, "proceed past the plan gate unattended; pass --auto-approve=false to keep the gate")
	create.Flags().Bool("wait-on-limit", false, "park a fired run until the Anthropic usage window reopens instead of failing it")
	return create
}

// buildScheduleRequest assembles the ScheduleRequest from the create flags, enforcing
// the one-of TARGET and one-of TIMING constraints client-side so a bad invocation is a
// clean exit-2 usage error before any request is sent (the server also enforces them).
// It returns the request and the repo id.
func buildScheduleRequest(cmd *cobra.Command) (apitypes.ScheduleRequest, string, error) {
	repoID, _ := cmd.Flags().GetString("repo")
	if strings.TrimSpace(repoID) == "" {
		return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--repo is required (a repo id from `uzi repo list`)")
	}

	issue, _ := cmd.Flags().GetInt64("issue")
	sweep, _ := cmd.Flags().GetBool("sweep")
	prompt, _ := cmd.Flags().GetString("prompt")
	labels, _ := cmd.Flags().GetStringArray("label")

	// Exactly one target. Use Changed() for --issue/--prompt so an explicit --issue 0
	// is still rejected below (positive-IID check) rather than read as "unset".
	issueSet := cmd.Flags().Changed("issue")
	promptSet := cmd.Flags().Changed("prompt")
	targets := 0
	for _, on := range []bool{issueSet, sweep, promptSet} {
		if on {
			targets++
		}
	}
	if targets != 1 {
		return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage,
			"specify exactly one target: --issue <iid>, --sweep, or --prompt <text>")
	}
	if len(labels) > 0 && !sweep {
		return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--label is only valid with --sweep")
	}

	req := apitypes.ScheduleRequest{}
	switch {
	case issueSet:
		if issue <= 0 {
			return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--issue must be a positive issue IID")
		}
		req.Target = schedTargetIssue
		req.IssueIID = &issue
	case sweep:
		req.Target = schedTargetSweep
		req.Labels = labels
	case promptSet:
		if strings.TrimSpace(prompt) == "" {
			return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--prompt needs task text")
		}
		req.Target = schedTargetPrompt
		req.Prompt = prompt
	}

	// Exactly one timing.
	atStr, _ := cmd.Flags().GetString("at")
	cron, _ := cmd.Flags().GetString("cron")
	atSet := cmd.Flags().Changed("at")
	cronSet := cmd.Flags().Changed("cron")
	if atSet == cronSet {
		return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage,
			"specify exactly one timing: --at <RFC3339> (once) or --cron <expr> (recurring)")
	}
	tz, _ := cmd.Flags().GetString("tz")
	req.Timezone = strings.TrimSpace(tz)
	if atSet {
		runAt, err := parseAt(atStr)
		if err != nil {
			return apitypes.ScheduleRequest{}, "", err
		}
		req.Timing = schedTimingOnce
		req.RunAt = &runAt
	} else {
		req.Timing = schedTimingRecurring
		req.CronExpr = cron
	}

	// auto_approve and wait_on_limit carry a client-side default (on / off), so they are
	// always sent as pointers rather than omitted — the flag value IS the statement.
	autoApprove, _ := cmd.Flags().GetBool("auto-approve")
	waitOnLimit, _ := cmd.Flags().GetBool("wait-on-limit")
	req.AutoApprove = &autoApprove
	req.WaitOnLimit = &waitOnLimit
	return req, repoID, nil
}

// parseAt parses a --at timestamp, tolerating both RFC3339 and its minute-precision
// form. A malformed value is a usage error before any request (the server is the
// authority on whether a well-formed time is in the future).
func parseAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range atLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, uzicli.Exitf(uzicli.ExitUsage,
		"--at must be an RFC3339 timestamp (e.g. 2026-08-08T09:00:00Z)")
}

func newScheduleListCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List your schedules",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			scheds, err := c.ListSchedules(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(scheds)
			}
			rows := make([][]string, 0, len(scheds))
			for _, s := range scheds {
				rows = append(rows, []string{
					s.ID,
					scheduleTarget(s),
					strOr(&s.RepoPath, "-"),
					scheduleWhen(s),
					scheduleNext(s, time.Now()),
					scheduleOn(s),
				})
			}
			return p.Table([]string{"ID", "TARGET", "REPO", "WHEN", "NEXT", "ON"}, rows)
		},
	}
}

func newScheduleGetCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "get <schedule-id>",
		Short: "Show one schedule's configuration and next fires",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			s, err := c.GetSchedule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(s)
			}
			return renderScheduleDetail(p, s)
		},
	}
}

func newSchedulePauseCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "pause <schedule-id>",
		Short: "Pause a schedule (stop firing without deleting it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setScheduleEnabled(env, gf, cmd, args[0], false)
		},
	}
}

func newScheduleResumeCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <schedule-id>",
		Short: "Resume a paused schedule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setScheduleEnabled(env, gf, cmd, args[0], true)
		},
	}
}

// setScheduleEnabled backs pause/resume: a PATCH carrying only {enabled}. --json dumps
// the updated schedule; the human line reads "paused"/"resumed".
func setScheduleEnabled(env Env, gf *globalFlags, cmd *cobra.Command, id string, enabled bool) error {
	c, err := env.client(gf)
	if err != nil {
		return err
	}
	s, err := c.SetScheduleEnabled(cmd.Context(), id, enabled)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(s)
	}
	if !gf.quiet {
		verb := "resumed"
		if !enabled {
			verb = "paused"
		}
		p.Printf("%s %s\n", verb, id)
	}
	return nil
}

func newScheduleRunNowCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run-now <schedule-id>",
		Short: "Fire a schedule immediately without disturbing its cadence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			res, err := c.RunScheduleNow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(res)
			}
			if gf.quiet {
				return nil
			}
			// A benign dedup skip (a prior run still live) creates nothing — say so
			// rather than printing "started run" with no id.
			if res.Created == 0 {
				p.Printf("no run started from %s (a matching run may already be active)\n", args[0])
				return nil
			}
			p.Printf("started %d run(s) from %s: %s\n", res.Created, args[0], strings.Join(res.RunIDs, ", "))
			return nil
		},
	}
}

func newScheduleDeleteCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <schedule-id>",
		Short: "Delete a schedule (run history is preserved)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if err := c.DeleteSchedule(cmd.Context(), args[0]); err != nil {
				return err
			}
			if !gf.quiet {
				env.printer(gf).Printf("deleted %s\n", args[0])
			}
			return nil
		},
	}
}

// scheduleTarget renders the TARGET column: "#<iid>" for a pinned issue, "sweep" or
// "sweep:<labels>" for a label sweep, "prompt" for the ad-hoc prompt target.
func scheduleTarget(s apitypes.ScheduleDTO) string {
	switch s.Target {
	case schedTargetIssue:
		return "#" + int64Or(s.IssueIID, "?")
	case schedTargetSweep:
		if len(s.Labels) > 0 {
			return "sweep:" + strings.Join(s.Labels, ",")
		}
		return "sweep"
	case schedTargetPrompt:
		return "prompt"
	default:
		return s.Target
	}
}

// scheduleWhen renders the WHEN column: the cron expression for a recurring schedule,
// "once" for a one-time one.
func scheduleWhen(s apitypes.ScheduleDTO) string {
	if s.Timing == schedTimingRecurring {
		return s.CronExpr
	}
	return schedTimingOnce
}

// scheduleNext renders the NEXT column as a forward-looking "in <dur>", or "—" when
// there is no upcoming fire to show (paused, terminal, or an unset/past next_fire_at).
func scheduleNext(s apitypes.ScheduleDTO, now time.Time) string {
	if !s.Enabled || s.NextFireAt == nil {
		return "—"
	}
	d := s.NextFireAt.Sub(now)
	if d <= 0 {
		return "due"
	}
	return "in " + fmtUntil(d)
}

// scheduleOn renders the ON column: "yes" when enabled, "paused" otherwise (mirroring
// the mock's list, where a disabled row reads "paused").
func scheduleOn(s apitypes.ScheduleDTO) string {
	if s.Enabled {
		return "yes"
	}
	return "paused"
}

// renderCreatedSchedule prints a freshly created schedule. Under --json it dumps the
// DTO; in human mode it confirms the id and when it fires next.
func renderCreatedSchedule(env Env, gf *globalFlags, s apitypes.ScheduleDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(s)
	}
	if gf.quiet {
		return nil
	}
	when := scheduleWhen(s)
	next := scheduleNext(s, time.Now())
	p.Printf("created schedule %s · %s · next %s\n", s.ID, when, next)
	return nil
}

// renderScheduleDetail prints a schedule as an aligned key/value block, mirroring
// renderRunDetail. Optional rows (issue/labels/prompt, cron/run_at, last fired, next
// fires) are emitted only when set, so a row is never blank.
func renderScheduleDetail(p *uzicli.Printer, s apitypes.ScheduleDTO) error {
	rows := [][]string{
		{"ID", s.ID},
		{"TARGET", scheduleTarget(s)},
		{"REPO", strOr(&s.RepoPath, "-")},
		{"TIMING", s.Timing},
		{"WHEN", scheduleWhen(s)},
	}
	if s.Timing == schedTimingOnce && s.RunAt != nil {
		rows = append(rows, []string{"RUN_AT", s.RunAt.UTC().Format(time.RFC3339)})
	}
	if s.Timing == schedTimingRecurring {
		rows = append(rows, []string{"TIMEZONE", s.Timezone})
	}
	if s.Target == schedTargetPrompt && s.Prompt != "" {
		rows = append(rows, []string{"PROMPT", s.Prompt})
	}
	rows = append(rows,
		[]string{"AUTO_APPROVE", boolStr(s.AutoApprove)},
		[]string{"WAIT_ON_LIMIT", boolStr(s.WaitOnLimit)},
		[]string{"ENABLED", boolStr(s.Enabled)},
		[]string{"STATUS", s.Status},
	)
	if s.NextFireAt != nil {
		rows = append(rows, []string{"NEXT_FIRE_AT", s.NextFireAt.UTC().Format(time.RFC3339)})
	}
	if s.LastFiredAt != nil {
		rows = append(rows, []string{"LAST_FIRED_AT", s.LastFiredAt.UTC().Format(time.RFC3339)})
	}
	for i, f := range s.NextFires {
		rows = append(rows, []string{fmt.Sprintf("  next[%d]", i), f.UTC().Format(time.RFC3339)})
	}
	return p.Table(nil, rows)
}
