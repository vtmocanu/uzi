package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
		newScheduleEditCmd(env, gf),
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
	create.Flags().Int("max-issues", 10, "for --sweep: cap on issues started per fire, oldest-first (default 10; ignored for non-sweep targets)")
	create.Flags().String("guidance", "", "optional owner guidance injected into the run instruction (--issue/--sweep only)")
	create.Flags().String("model", "", "model alias (opus/sonnet/haiku/fable) or a custom model ID for runs this schedule fires; empty inherits your Worker-model default (valid on all targets)")
	create.Flags().Bool("apply-model-to-agents", false, "also apply the schedule's model to every subagent (overrides each agent's own model pin); default off keeps per-agent pins")
	create.Flags().String("at", "", "fire once at this RFC3339 time (one of --at/--cron)")
	create.Flags().String("cron", "", "recurring 5-field cron expression (one of --at/--cron)")
	create.Flags().String("tz", "UTC", "IANA timezone the --cron expression is interpreted in")
	create.Flags().Bool("auto-approve", true, "proceed past the plan gate unattended; pass --auto-approve=false to keep the gate")
	create.Flags().Bool("wait-on-limit", true, "park a fired run until the Anthropic usage window reopens instead of failing it; pass --wait-on-limit=false to fail on limit")
	create.Flags().Bool("enabled", true, "create the schedule enabled; pass --enabled=false to create it paused")
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
	// --max-issues is sweep-only; reject an EXPLICIT set on a non-sweep target (an
	// unchanged default is silently ignored, mirroring the --label rule above).
	if cmd.Flags().Changed("max-issues") && !sweep {
		return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--max-issues is only valid with --sweep")
	}
	// --guidance is issue/sweep-only; reject an EXPLICIT set on the prompt target (a prompt
	// carries its own text). --guidance is distinct from the --prompt target selector.
	guidanceSet := cmd.Flags().Changed("guidance")
	if guidanceSet && !issueSet && !sweep {
		return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--guidance is only valid with --issue or --sweep")
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
		// Only a sweep sends max_issues. The flag default (10) matches the server default,
		// so a plain `--sweep` naturally requests a bounded fan-out; --max-issues overrides.
		maxIssues, _ := cmd.Flags().GetInt("max-issues")
		req.MaxIssues = &maxIssues
	case promptSet:
		if strings.TrimSpace(prompt) == "" {
			return apitypes.ScheduleRequest{}, "", uzicli.Exitf(uzicli.ExitUsage, "--prompt needs task text")
		}
		req.Target = schedTargetPrompt
		req.Prompt = prompt
	}

	// Guidance rides only on issue/sweep targets (guarded above). Send it only when set so
	// an unset flag stays absent (nil) rather than clearing on a PATCH-shaped payload.
	if guidanceSet && (issueSet || sweep) {
		guidance, _ := cmd.Flags().GetString("guidance")
		req.Guidance = &guidance
	}

	// --model is valid on every target (a run's model is orthogonal to what it works on),
	// so unlike --guidance/--max-issues it carries no target guard. Send it only when set so
	// an unset flag stays absent (nil) rather than clearing.
	if cmd.Flags().Changed("model") {
		model, _ := cmd.Flags().GetString("model")
		req.Model = &model
	}

	// PRD #305: opt-in to override every subagent's model with the run model. Only set
	// when the caller passed the flag, so an omitted flag stays nil (server default false).
	if cmd.Flags().Changed("apply-model-to-agents") {
		v, _ := cmd.Flags().GetBool("apply-model-to-agents")
		req.OverrideSubagentModel = &v
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

	// --enabled is only sent when the caller passed it, so an omitted flag stays nil and
	// the server's create default (enabled=true) applies. Use Changed() rather than the
	// always-send pointer pattern of --auto-approve so today's default behavior is byte-identical.
	if cmd.Flags().Changed("enabled") {
		enabled, _ := cmd.Flags().GetBool("enabled")
		req.Enabled = &enabled
	}
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

func newScheduleEditCmd(env Env, gf *globalFlags) *cobra.Command {
	edit := &cobra.Command{
		Use:   "edit <schedule-id>",
		Short: "Edit a schedule's mutable config in place",
		Long: "Edit the mutable configuration of an existing schedule WITHOUT churning its id\n" +
			"or run history (unlike a delete-and-recreate). Any field you do not pass keeps its\n" +
			"stored value, so you can change one thing without restating the rest. Editing config\n" +
			"REVIVES a terminal schedule (status returns to active) — a recurring one resumes on\n" +
			"its next fire, while a fired one-shot needs a fresh `--at` in the future. It does NOT\n" +
			"un-pause: a paused schedule (enabled=false) stays paused after an edit; turning a\n" +
			"schedule off or back on is `schedule pause`/`resume`, which this verb never touches.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			id := args[0]
			s, err := c.GetSchedule(cmd.Context(), id)
			if err != nil {
				return err
			}
			req, err := buildScheduleEditRequest(cmd, s)
			if err != nil {
				return err
			}
			updated, err := c.PatchSchedule(cmd.Context(), id, req)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(updated)
			}
			if !gf.quiet {
				p.Printf("updated %s · %s · next %s\n", updated.ID, scheduleWhen(updated), scheduleNext(updated, time.Now()))
			}
			return nil
		},
	}
	edit.Flags().String("cron", "", "change to this recurring 5-field cron expression (switches timing to recurring)")
	edit.Flags().String("at", "", "change to fire once at this RFC3339 time (switches timing to once)")
	edit.Flags().String("tz", "", "change the IANA timezone the --cron expression is interpreted in")
	edit.Flags().String("prompt", "", "change the ad-hoc prompt text (prompt-target schedules only)")
	edit.Flags().StringArray("label", nil, "replace the sweep label selector (repeatable; sweep-target schedules only)")
	edit.Flags().Bool("auto-approve", true, "set whether a fired run proceeds past the plan gate unattended")
	edit.Flags().Bool("wait-on-limit", true, "set whether a fired run parks on the usage limit instead of failing")
	edit.Flags().String("guidance", "", "change owner guidance injected into the run instruction (issue/sweep targets only)")
	edit.Flags().Int("max-issues", 10, "change the per-fire sweep cap, oldest-first (sweep target only)")
	edit.Flags().Bool("clear-guidance", false, "clear stored guidance back to none (issue/sweep targets only)")
	edit.Flags().Bool("clear-max-issues", false, "clear the sweep cap back to unlimited (sweep target only)")
	edit.Flags().Bool("apply-model-to-agents", false, "set whether the schedule's model also overrides every subagent's model pin")
	edit.Flags().String("repo", "", "repoint the schedule to another repo by id (sweep/prompt targets; an issue-target schedule cannot be repointed)")
	return edit
}

// buildScheduleEditRequest builds a full ScheduleRequest for `schedule edit` from the
// FETCHED schedule, then overlays only the flags the caller explicitly Changed().
//
// A typed rebuild from the DTO's config fields (rather than re-posting the raw DTO) is
// deliberate: the PATCH endpoint decodes with DisallowUnknownFields, so the DTO's
// response-only fields (id, status, created_at, updated_at, next_fire_at, last_fired_at,
// next_fires, repo_id, repo_path) would be rejected as unknown → 400. It also compensates
// for mergeSchedule, which is keep-on-empty for most fields but takes max_issues,
// guidance, model and override_subagent_model STRAIGHT from the request (nil clears them)
// — so those MUST be re-sent from the fetched row, or a --cron-only edit would silently
// wipe them. Enabled is left nil so a config edit never touches the pause flag
// (enable/disable is pause/resume's job).
func buildScheduleEditRequest(cmd *cobra.Command, s apitypes.ScheduleDTO) (apitypes.ScheduleRequest, error) {
	req := apitypes.ScheduleRequest{
		Target:    s.Target,
		IssueIID:  s.IssueIID,
		Labels:    s.Labels,
		Prompt:    s.Prompt,
		Timing:    s.Timing,
		CronExpr:  s.CronExpr,
		RunAt:     s.RunAt,
		Timezone:  s.Timezone,
		MaxIssues: s.MaxIssues,
		Guidance:  s.Guidance,
		// PRD #300 replace-semantics: restate or a partial edit (e.g. --cron only) wipes
		// the stored model, since mergeSchedule does m.Model = req.Model (pre-existing bug).
		Model: s.Model,
		// PRD #305 replace-semantics: same class — restate the subagent override or a
		// partial edit wipes it. The DTO always sets this bool non-nil.
		OverrideSubagentModel: s.OverrideSubagentModel,
	}
	// DTO carries these as plain bool; re-send as pointer copies (the config PATCH path
	// always restates them). Enabled stays nil — see the doc comment.
	autoApprove := s.AutoApprove
	waitOnLimit := s.WaitOnLimit
	req.AutoApprove = &autoApprove
	req.WaitOnLimit = &waitOnLimit

	f := cmd.Flags()
	cronSet := f.Changed("cron")
	atSet := f.Changed("at")
	tzSet := f.Changed("tz")
	promptSet := f.Changed("prompt")
	labelSet := f.Changed("label")
	autoSet := f.Changed("auto-approve")
	waitSet := f.Changed("wait-on-limit")
	guidanceSet := f.Changed("guidance")
	maxIssuesSet := f.Changed("max-issues")
	clearGuidance := f.Changed("clear-guidance")
	clearMaxIssues := f.Changed("clear-max-issues")
	repoSet := f.Changed("repo")

	// --cron and --at both restate TIMING; at most one may win.
	if cronSet && atSet {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"specify at most one timing: --cron <expr> (recurring) or --at <RFC3339> (once)")
	}
	// Target-scoped flags: reject an EXPLICIT set on the wrong target (mirrors create).
	if promptSet && s.Target != schedTargetPrompt {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--prompt is only valid on a prompt-target schedule")
	}
	if labelSet && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--label is only valid on a sweep-target schedule")
	}
	if (guidanceSet || clearGuidance) && s.Target != schedTargetIssue && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--guidance/--clear-guidance are only valid on an issue or sweep target")
	}
	if (maxIssuesSet || clearMaxIssues) && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues/--clear-max-issues are only valid on a sweep target")
	}
	// Set-vs-clear conflicts: a field cannot be both changed and cleared in one edit.
	if guidanceSet && clearGuidance {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--guidance and --clear-guidance are mutually exclusive")
	}
	if maxIssuesSet && clearMaxIssues {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues and --clear-max-issues are mutually exclusive")
	}

	changed := false
	if cronSet {
		cron, _ := f.GetString("cron")
		req.Timing = schedTimingRecurring
		req.CronExpr = cron
		req.RunAt = nil
		changed = true
	}
	if atSet {
		atStr, _ := f.GetString("at")
		runAt, err := parseAt(atStr)
		if err != nil {
			return apitypes.ScheduleRequest{}, err
		}
		req.Timing = schedTimingOnce
		req.RunAt = &runAt
		req.CronExpr = ""
		changed = true
	}
	if tzSet {
		tz, _ := f.GetString("tz")
		req.Timezone = strings.TrimSpace(tz)
		changed = true
	}
	if promptSet {
		req.Prompt, _ = f.GetString("prompt")
		changed = true
	}
	if labelSet {
		req.Labels, _ = f.GetStringArray("label")
		changed = true
	}
	if autoSet {
		v, _ := f.GetBool("auto-approve")
		req.AutoApprove = &v
		changed = true
	}
	if waitSet {
		v, _ := f.GetBool("wait-on-limit")
		req.WaitOnLimit = &v
		changed = true
	}
	if guidanceSet {
		v, _ := f.GetString("guidance")
		req.Guidance = &v
		changed = true
	}
	if clearGuidance {
		req.Guidance = nil
		changed = true
	}
	if maxIssuesSet {
		v, _ := f.GetInt("max-issues")
		req.MaxIssues = &v
		changed = true
	}
	if clearMaxIssues {
		req.MaxIssues = nil
		changed = true
	}
	if f.Changed("apply-model-to-agents") {
		v, _ := f.GetBool("apply-model-to-agents")
		req.OverrideSubagentModel = &v
		changed = true
	}
	if repoSet {
		v, _ := f.GetString("repo")
		// Keep-on-empty in the server merge: only a non-empty value repoints. Trim so a
		// stray space does not reach the server's uuid.Parse as a malformed id.
		req.RepoID = strings.TrimSpace(v)
		changed = true
	}
	if !changed {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "nothing to edit (pass at least one field to change)")
	}
	return req, nil
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
			renderRunNow(p, args[0], res)
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

// maxIssuesStr renders the sweep cap for the detail block: the number when set, or
// "unlimited" when nil (NULL = no cap, PRD #274 M2).
func maxIssuesStr(p *int) string {
	if p == nil {
		return "unlimited"
	}
	return fmt.Sprintf("%d", *p)
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
	if s.Target == schedTargetSweep {
		rows = append(rows, []string{"MAX_ISSUES", maxIssuesStr(s.MaxIssues)})
	}
	if s.Target == schedTargetIssue || s.Target == schedTargetSweep {
		rows = append(rows, []string{"GUIDANCE", strOr(s.Guidance, "-")})
	}
	rows = append(rows,
		[]string{"MODEL", strOr(s.Model, "-")},
		[]string{"APPLY_MODEL_TO_AGENTS", boolStr(s.OverrideSubagentModel != nil && *s.OverrideSubagentModel)},
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
	if err := p.Table(nil, rows); err != nil {
		return err
	}
	renderLastFire(p, s.LastFire)
	return nil
}

// skipReasonLabels maps a schedsvc.SkipReason wire string to a short human label for CLI
// output (PRD #308 M5). This is PRESENTATIONAL only — it is NOT the cross-language drift
// guard (that is the Go↔TS test in web/src/lib/scheduleSkipReasons.test.ts). An unknown
// reason falls back to the raw wire string in skipReasonLabel, so a new server-side reason
// degrades gracefully rather than rendering blank.
var skipReasonLabels = map[string]string{
	"no_prd_link":           "no PRD link",
	"not_eligible":          "not eligible",
	"already_running":       "already running",
	"description_too_large": "description too large",
	"fetch_failed":          "fetch failed",
}

// skipReasonLabel renders a skip reason as its human label, falling back to the raw wire
// string for an unmapped value (graceful degradation — the wire is the source of truth).
func skipReasonLabel(reason string) string {
	if label, ok := skipReasonLabels[reason]; ok {
		return label
	}
	return reason
}

// skipReasonHints carries an optional remediation hint per skip reason for the run-now
// per-candidate breakdown. A reason with no actionable hint is absent (empty), and the
// caller omits the trailing `# …` for it.
var skipReasonHints = map[string]string{
	"no_prd_link": "add PRDLESS / a prds link, or raise --max-issues",
}

// skipReasonHint returns the remediation hint for a skip reason, or "" when none applies.
func skipReasonHint(reason string) string { return skipReasonHints[reason] }

// lastFireCappedHint is the one-line steer shown when a capped fire started nothing and
// every examined candidate was skipped — the newest issues were never reached.
const lastFireCappedHint = "newer issues not reached — raise --max-issues or add PRDLESS / a PRD link"

// fireCandidateLabel renders a started/skipped candidate's identity: "#<iid>" for an
// issue/sweep candidate, or "prompt" for a prompt schedule (which carries a nil iid).
func fireCandidateLabel(iid *int64) string {
	if iid == nil {
		return "prompt"
	}
	return fmt.Sprintf("#%d", *iid)
}

// renderLastFire appends the "Last fire" block to a schedule detail (PRD #308 M5),
// summarising the schedule's most recent persisted fire: a one-line summary, the runs it
// started, the candidates it skipped (with human reason labels), and — when a capped fire
// reached nobody — the raise-the-cap hint. A nil last_fire means the schedule never fired.
func renderLastFire(p *uzicli.Printer, lf *apitypes.LastFire) {
	if lf == nil {
		p.Printf("Last fire: never fired\n")
		return
	}
	p.Printf("Last fire:\n")
	p.Printf("  fired %s · examined %d · started %d · skipped %d\n",
		lf.FiredAt.UTC().Format(time.RFC3339), lf.Matched, len(lf.Started), len(lf.Skips))
	for _, st := range lf.Started {
		p.Printf("    %s → run %s  %s\n", fireCandidateLabel(st.IssueIID), st.RunID, st.Title)
	}
	for _, sk := range lf.Skips {
		p.Printf("    %s  %s  %s\n", fireCandidateLabel(sk.IssueIID), skipReasonLabel(sk.Reason), sk.Title)
	}
	if lf.Capped && len(lf.Skips) > 0 && len(lf.Started) == 0 {
		p.Printf("  %s\n", lastFireCappedHint)
	}
}

// renderRunNow prints the human outcome of a `schedule run-now` fire (PRD #308 M5) from
// the widened RunNowResponse: a header with the started run ids, a per-started line, and —
// when candidates were skipped — the examined/skipped tally with a human reason label and
// an optional remediation hint per skip. A fire that started nothing AND skipped nothing is
// a benign dedup (a prior run still live), reported as such rather than as "started 0".
func renderRunNow(p *uzicli.Printer, id string, res apitypes.RunNowResponse) {
	if res.Created == 0 && len(res.Skips) == 0 {
		p.Printf("no run started from %s (a matching run may already be active)\n", id)
		return
	}
	if res.Created == 0 {
		// The flagship case (a sweep that skipped every candidate): lead with a clean
		// period-terminated clause rather than "Started 0 run(s) from <id>" trailing into
		// the skip breakdown below.
		p.Printf("Started 0 runs from %s.\n", id)
	} else {
		p.Printf("Started %d run(s) from %s", res.Created, id)
		if len(res.RunIDs) > 0 {
			p.Printf(": %s", strings.Join(res.RunIDs, ", "))
		}
		p.Printf("\n")
	}
	for _, st := range res.Started {
		p.Printf("  %s → run %s  %s\n", fireCandidateLabel(st.IssueIID), st.RunID, st.Title)
	}
	if len(res.Skips) > 0 {
		p.Printf("Examined %d candidate(s), skipped %d:\n", res.Matched, len(res.Skips))
		for _, sk := range res.Skips {
			line := fmt.Sprintf("  %s  %s", fireCandidateLabel(sk.IssueIID), skipReasonLabel(sk.Reason))
			if hint := skipReasonHint(sk.Reason); hint != "" {
				line += "   # " + hint
			}
			p.Printf("%s\n", line)
		}
	}
}
