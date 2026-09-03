package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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

	// schedOriginDefault is the ScheduleDTO.Origin value for a catalog-seeded default row
	// (PRD #589); "user" is the owner-authored counterpart. A default row takes the
	// catalog-editable-only edit path (buildDefaultScheduleEditRequest).
	schedOriginDefault = "default"
)

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
		newSchedulePauseAllCmd(env, gf),
		newScheduleResumeAllCmd(env, gf),
		newSchedulePauseStatusCmd(env, gf),
		newScheduleRunNowCmd(env, gf),
		newScheduleDeleteCmd(env, gf),
		newScheduleCatalogCmd(env, gf),
		newScheduleResetCmd(env, gf),
		newScheduleCloneCmd(env, gf),
		newScheduleAddRepoCmd(env, gf),
	)
	return cmd
}

func newScheduleResetCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reset <schedule-id>",
		Short: "Reset a default schedule's edited fields back to the catalog defaults",
		Long: "Restore a default-origin schedule's editable fields (cron, timezone, model,\n" +
			"auto-approve, wait-on-limit, mr-rework, max-issues) to the builtin catalog values and clear its\n" +
			"customized flag. Only a default-origin schedule can be reset; a user-origin one is a\n" +
			"conflict (there is nothing to reset to).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			s, err := c.ResetSchedule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(s)
			}
			if !gf.quiet {
				p.Printf("reset %s to catalog defaults · %s · next %s\n", s.ID, scheduleWhen(s), scheduleNext(s, time.Now()))
			}
			return nil
		},
	}
}

func newScheduleCloneCmd(env Env, gf *globalFlags) *cobra.Command {
	clone := &cobra.Command{
		Use:   "clone <schedule-id>",
		Short: "Clone a schedule into a new fully-editable copy",
		Long: "Copy a schedule into a new schedule you fully own and can edit. Cloning a DEFAULT\n" +
			"schedule lifts its catalog prompt lock — the baked prompt (or sweep labels/guidance)\n" +
			"is copied into the new row, which becomes a normal user schedule. Pass --repo to clone\n" +
			"into a DIFFERENT repo you own (the replication path); omit it to clone into the source\n" +
			"schedule's own repo.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoID, _ := cmd.Flags().GetString("repo")
			s, err := c.CloneSchedule(cmd.Context(), args[0], strings.TrimSpace(repoID))
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(s)
			}
			if !gf.quiet {
				p.Printf("cloned %s → %s · %s · next %s\n", args[0], s.ID, scheduleWhen(s), scheduleNext(s, time.Now()))
			}
			return nil
		},
	}
	clone.Flags().String("repo", "", "clone into this repo id instead of the source's repo (must be one you own)")
	return clone
}

// newScheduleAddRepoCmd — `uzi schedule add-repo <id> --repo <repoID>` (PRD #636 M4,
// Decision 5): replicate an existing user schedule's current config onto ANOTHER repo you
// own as a new grouped sibling. It stamps both the source and the new row with one shared
// display-only sibling_group_id (allocated server-side, race-safely), so they render as one
// expandable group — the CLI twin of the web "Add another repo" action. Only a user-origin
// schedule can be added onto; a foreign source or target repo is a 404.
//
// A 409 means the schedule already has a sibling on that repo (the (sibling_group_id,
// repo_id) unique index): the desired end state already holds, so it is reported as a clean
// no-op and exits 0 rather than as a conflict error.
func newScheduleAddRepoCmd(env Env, gf *globalFlags) *cobra.Command {
	addRepo := &cobra.Command{
		Use:   "add-repo <schedule-id>",
		Short: "Replicate a schedule onto another repo as a grouped sibling",
		Long: "Replicate an existing schedule you own onto ANOTHER repo you own as a new sibling,\n" +
			"grouped with the source so they render as one expandable group. The new row is an\n" +
			"independent, fully-editable copy of the source's current config (edit/pause/remove it\n" +
			"on its own). Pass --repo <repoID> for the target repo (from `uzi repo list`). If the\n" +
			"schedule already has a sibling on that repo this is a clean no-op.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoID, _ := cmd.Flags().GetString("repo")
			repoID = strings.TrimSpace(repoID)
			if repoID == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--repo is required (the target repo id from `uzi repo list`)")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			s, err := c.AddScheduleRepo(cmd.Context(), args[0], repoID)
			if err != nil {
				// A 409 (unique-index conflict) means a sibling already exists on that repo.
				// The desired state already holds, so report it as a clean no-op and exit 0.
				// The notice goes to stderr so a --json stdout stays empty/clean.
				if uzicli.ExitCodeFor(err) == uzicli.ExitConflict {
					if !gf.quiet {
						_, _ = fmt.Fprintf(env.Stderr, "%s is already on repo %s — nothing to do\n", args[0], repoID)
					}
					return nil
				}
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(s)
			}
			if !gf.quiet {
				p.Printf("added repo %s to %s → sibling %s · %s · next %s\n",
					repoID, args[0], s.ID, scheduleWhen(s), scheduleNext(s, time.Now()))
			}
			return nil
		},
	}
	addRepo.Flags().String("repo", "", "the target repo id to replicate this schedule onto as a new grouped sibling (required)")
	return addRepo
}

// nonBlankTrimmed trims each entry and drops the blanks, so a `--repo ""` or a stray space
// does not reach the fan-out as an empty repo id.
func nonBlankTrimmed(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func newScheduleCreateCmd(env Env, gf *globalFlags) *cobra.Command {
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a one-time or recurring schedule on a repo",
		Long: "Create a schedule that starts run(s) at future time(s). Pick exactly one\n" +
			"TARGET — --issue <iid> (a pinned issue), --sweep (every eligible issue matching\n" +
			"the --label selector, default the uzi label), or --prompt <text> (an issue-less\n" +
			"repo→MR run) — and exactly one TIMING — --at <RFC3339> (fires once) or --cron\n" +
			"<expr> (recurring, interpreted in --tz). Repeat --repo to create the same schedule\n" +
			"on N repos at once: a CLIENT-SIDE fan-out of one independent create per --repo.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, repos, err := buildScheduleRequest(cmd)
			if err != nil {
				return err
			}
			if req.Target == schedTargetIssue && len(repos) > 1 {
				return uzicli.Exitf(uzicli.ExitUsage,
					"an issue-target schedule cannot be created on multiple repos at once; issue numbers are repo-relative, so create it on one repo, then re-create it against each other repo")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// Sweep-label guardrail (PRD #589 M4): for a --sweep target, warn on (or, with
			// --create-missing-labels, create) any explicitly-named --label missing on a
			// target repo BEFORE creating the schedule. Never blocks the create; an empty
			// --label (server defaults to the uzi label) makes it a no-op.
			if req.Target == schedTargetSweep {
				createMissing, _ := cmd.Flags().GetBool("create-missing-labels")
				runSweepLabelGuardrail(cmd, env, gf, c, repos, req.Labels, createMissing)
			}
			// Single-repo: the unchanged path and output (one create, one DTO dumped).
			if len(repos) == 1 {
				s, cerr := c.CreateSchedule(cmd.Context(), repos[0], req)
				if cerr != nil {
					return cerr
				}
				return renderCreatedSchedule(env, gf, s)
			}
			// Multi-repo client-side fan-out: one independent create per --repo. Generate ONE
			// display-only sibling_group_id (uuid v4) here and stamp it on every create body so
			// the N rows share a group and the web renders them as one expandable summary (PRD
			// #636 Decision 4). The single-repo fast path above leaves it nil (a standalone row).
			// The group id is cosmetic — the rows stay fully independent; owner-scoping bounds
			// its blast radius to the caller's own rows.
			groupID := uuid.NewString()
			req.SiblingGroupID = &groupID
			// Accumulate the created schedules so --json returns them all and a mid-loop failure
			// still reports what already landed BEFORE the error propagates (a partial landing is
			// safe to retry).
			created := make([]apitypes.ScheduleDTO, 0, len(repos))
			for _, repoID := range repos {
				s, cerr := c.CreateSchedule(cmd.Context(), repoID, req)
				if cerr != nil {
					_ = renderCreatedSchedules(env, gf, created)
					return cerr
				}
				created = append(created, s)
			}
			return renderCreatedSchedules(env, gf, created)
		},
	}
	create.Flags().StringArray("repo", nil, "repo id to schedule against (see 'uzi repo list'); repeatable to create the schedule on N repos at once")
	create.Flags().Int64("issue", 0, "pinned issue IID target (one of --issue/--sweep/--prompt)")
	create.Flags().Bool("sweep", false, "label-sweep target: every eligible matching issue (one of --issue/--sweep/--prompt)")
	create.Flags().String("prompt", "", "ad-hoc prompt target: an issue-less repo→MR run (one of --issue/--sweep/--prompt)")
	create.Flags().StringArray("label", nil, "a label to select for --sweep (repeatable; empty defaults to the uzi label)")
	create.Flags().Int("max-issues", 10, "for --sweep: cap on issues started per fire, oldest-first (default 10; ignored for non-sweep targets)")
	create.Flags().String("guidance", "", "optional owner guidance injected into the run instruction (--issue/--sweep only)")
	create.Flags().String("model", "", "model alias (opus/sonnet/haiku/fable) or a custom model ID for runs this schedule fires; empty inherits your Worker-model default (valid on all targets)")
	create.Flags().Bool("apply-model-to-agents", false, "also apply the schedule's model to every subagent (overrides each agent's own model pin); default off keeps per-agent pins")
	create.Flags().String("at", "", "fire once at this RFC3339 time (one of --at/--cron)")
	create.Flags().String("cron", "", "recurring 5-field cron expression (one of --at/--cron)")
	create.Flags().String("tz", "UTC", "IANA timezone the --cron expression is interpreted in")
	create.Flags().Bool("auto-approve", true, "proceed past the plan gate unattended; pass --auto-approve=false to keep the gate")
	create.Flags().Bool("wait-on-limit", true, "park a fired run until the Anthropic usage window reopens instead of failing it; pass --wait-on-limit=false to fail on limit")
	create.Flags().Bool("mr-rework", false, "enable or disable auto-rework of fired runs' MR review comments; omit to inherit the account default, or pass --mr-rework=false to force off")
	create.Flags().Bool("enabled", true, "create the schedule enabled; pass --enabled=false to create it paused")
	create.Flags().Bool("create-missing-labels", false, "for a --sweep target: create any --label missing on a target repo before creating the schedule (default: warn only)")
	return create
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
				// list --json stays a BARE array of ScheduleDTO (unchanged): only the
				// human table reflects the user-wide pause, so the pause state is fetched
				// only below, in the table branch.
				return p.JSON(scheds)
			}
			// Fetch the user-level pause-all state once (PRD #1093 Solution 5). While it
			// is active, every row's NEXT reads "paused (all) …" regardless of the row's
			// own next fire; an expired until is server-normalized to not-paused, so the
			// normal next fire renders with no client-side special-casing.
			pause, err := c.GetSchedulePause(cmd.Context())
			if err != nil {
				return err
			}
			now := time.Now()
			rows := make([][]string, 0, len(scheds))
			for _, s := range scheds {
				rows = append(rows, []string{
					s.ID,
					scheduleTarget(s),
					strOr(&s.RepoPath, "-"),
					scheduleWhen(s),
					scheduleNextCell(s, now, pause),
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
			// Sweep-label guardrail (PRD #589 M4): when the edit CHANGES a sweep schedule's
			// --label selector, warn on (or, with --create-missing-labels, create) any newly
			// named label missing on the sweep's EFFECTIVE repo — the same advisory guardrail
			// `catalog enable`/`create` run. No extra fetch: s is already in hand, so its repo
			// id and target come for free. Skipped when the edit doesn't touch --label or the
			// target isn't sweep. Purely advisory — never blocks the edit.
			if s.Target == schedTargetSweep && cmd.Flags().Changed("label") {
				createMissing, _ := cmd.Flags().GetBool("create-missing-labels")
				newLabels, _ := cmd.Flags().GetStringArray("label")
				// If this same edit repoints the sweep via --repo, check/create labels on the
				// NEW target repo (where the sweep will actually run), not the one it is
				// leaving. req.RepoID is the trimmed --repo value; it is keep-on-empty in the
				// server merge, so an empty value does not repoint — fall back to s.RepoID.
				targetRepo := s.RepoID
				if cmd.Flags().Changed("repo") && req.RepoID != "" {
					targetRepo = req.RepoID
				}
				runSweepLabelGuardrail(cmd, env, gf, c, []string{targetRepo}, newLabels, createMissing)
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
	edit.Flags().Bool("mr-rework", false, "set whether fired runs' MR review comments are auto-reworked; pass --mr-rework=false to force off (an unset flag leaves the stored value unchanged)")
	edit.Flags().String("guidance", "", "change owner guidance injected into the run instruction (issue/sweep targets, or a prompt-target or sweep-target default)")
	edit.Flags().Int("max-issues", 10, "change the per-fire sweep cap, oldest-first (sweep target only)")
	edit.Flags().Bool("clear-guidance", false, "clear stored guidance back to none (issue/sweep targets, or a prompt-target or sweep-target default)")
	edit.Flags().Bool("clear-max-issues", false, "clear the sweep cap back to unlimited (sweep target only)")
	edit.Flags().Bool("clear-mr-rework", false, "clear the stored MR-rework override back to inherit (the account default)")
	edit.Flags().Bool("apply-model-to-agents", false, "set whether the schedule's model also overrides every subagent's model pin")
	edit.Flags().String("model", "", "change the model alias (opus/sonnet/haiku/fable) or custom model ID for runs this schedule fires; empty string clears it back to your Worker-model default (valid on every target)")
	edit.Flags().String("repo", "", "repoint the schedule to another repo by id (sweep/prompt targets; an issue-target schedule cannot be repointed)")
	edit.Flags().Bool("create-missing-labels", false, "for a sweep target: create any newly-set --label missing on the schedule's repo before saving the edit (default: warn only)")
	return edit
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

// newSchedulePauseAllCmd — `uzi schedule pause-all --until <when>` (PRD #1093 M3): the
// user-level kill switch that pauses EVERY schedule the caller owns, on every repo,
// with an auto-resume instant. Named pause-all (not pause) to avoid clashing with the
// per-row `pause <id>` verb. --until is REQUIRED so a bare pause-all can never silently
// mean forever; its relative forms are resolved client-side in the local timezone (D8)
// and sent as an absolute instant, while `never` sends until=null (indefinite).
func newSchedulePauseAllCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause-all --until <when>",
		Short: "Pause every schedule you own until a time (or until you resume)",
		Long: "Pause every schedule you own, on every repo — default jobs and your own alike —\n" +
			"until --until. Per-schedule on/off switches are left exactly as they are, so resuming\n" +
			"restores the prior set. Run-now still fires while paused; runs already in flight are\n" +
			"not stopped. --until accepts an RFC3339 time, a Go duration (24h, 12h30m), " +
			"`tomorrow[ HH:MM]`,\na weekday name `[ HH:MM]` (default 09:00, next occurrence), or `never` " +
			"(pause until you\nresume). Relative forms are resolved in your local timezone.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			when, _ := cmd.Flags().GetString("until")
			at, indefinite, err := resolveUntil(when, time.Now(), time.Local)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid --until: %v", err)
			}
			var untilPtr *time.Time
			if !indefinite {
				untilPtr = &at
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			state, err := c.SetSchedulePause(cmd.Context(), untilPtr)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(state)
			}
			if !gf.quiet {
				p.Printf("%s Run-now still works; resume early with `uzi schedule resume-all`.\n",
					pausedSentence(state, time.Now()))
			}
			return nil
		},
	}
	cmd.Flags().String("until", "", "when to auto-resume: an RFC3339 time, a duration (24h), tomorrow[ HH:MM], a weekday[ HH:MM] (default 09:00), or never (required)")
	_ = cmd.MarkFlagRequired("until")
	return cmd
}

// newScheduleResumeAllCmd — `uzi schedule resume-all` (PRD #1093 M3): lift the
// user-level pause, restoring every schedule to its own prior on/off state. Idempotent
// (resuming when not paused is a clean no-op).
func newScheduleResumeAllCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "resume-all",
		Short: "Resume all your schedules (lift a pause-all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			state, err := c.ClearSchedulePause(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(state)
			}
			if !gf.quiet {
				p.Printf("All schedules resumed.\n")
			}
			return nil
		},
	}
}

// newSchedulePauseStatusCmd — `uzi schedule pause-status` (PRD #1093 M3): read the
// user-level pause state. The server normalizes an expired until to not-paused, so this
// renders "not paused" for it with no client-side expiry check.
func newSchedulePauseStatusCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "pause-status",
		Short: "Show whether all your schedules are paused, and until when",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			state, err := c.GetSchedulePause(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(state)
			}
			if !gf.quiet {
				p.Printf("%s\n", pauseStatusLine(state, time.Now()))
			}
			return nil
		},
	}
}

// pausedSentence is the leading clause of `pause-all`'s confirmation: "All schedules
// paused until <stamp> (in Xh)." or "All schedules paused indefinitely." It reads the
// SERVER-returned (normalized) state so the confirmed instant matches what was stored.
func pausedSentence(state apitypes.SchedulePauseDTO, now time.Time) string {
	if state.Until == nil {
		return "All schedules paused indefinitely."
	}
	return fmt.Sprintf("All schedules paused until %s (in %s).", fmtStamp(*state.Until), fmtUntil(state.Until.Sub(now)))
}

// pauseStatusLine renders `pause-status`: "paused until <stamp> (in Xh)", "paused
// indefinitely", or "not paused", off the normalized state.
func pauseStatusLine(state apitypes.SchedulePauseDTO, now time.Time) string {
	if !state.Paused {
		return "not paused"
	}
	if state.Until == nil {
		return "paused indefinitely"
	}
	return fmt.Sprintf("paused until %s (in %s)", fmtStamp(*state.Until), fmtUntil(state.Until.Sub(now)))
}

// scheduleNextCell renders a list row's NEXT while honoring the user-wide pause: when
// pause-all is active every row reads "paused (all) until <stamp>" (or "paused (all)"
// for an indefinite pause), regardless of the row's own next fire. Not paused (incl. an
// expired, server-normalized until) falls back to the ordinary next-fire cell.
func scheduleNextCell(s apitypes.ScheduleDTO, now time.Time, pause apitypes.SchedulePauseDTO) string {
	if pause.Paused {
		if pause.Until != nil {
			return "paused (all) until " + fmtStamp(*pause.Until)
		}
		return "paused (all)"
	}
	return scheduleNext(s, now)
}

// fmtStamp renders an absolute pause instant in the caller's local zone, minute
// precision with the zone abbreviation, e.g. "2026-09-04 09:00 EDT".
func fmtStamp(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04 MST")
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
