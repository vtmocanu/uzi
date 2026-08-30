package main

import (
	"context"
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
		newScheduleCatalogCmd(env, gf),
		newScheduleResetCmd(env, gf),
		newScheduleCloneCmd(env, gf),
		newScheduleAddRepoCmd(env, gf),
	)
	return cmd
}

// newScheduleCatalogCmd — `uzi schedule catalog` and its verbs (PRD #589 M3): the builtin
// default-schedule catalog and enabling a default job on one or more repos.
func newScheduleCatalogCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Browse and enable the builtin default scheduled jobs",
	}
	cmd.AddCommand(
		newScheduleCatalogListCmd(env, gf),
		newScheduleCatalogEnableCmd(env, gf),
	)
	return cmd
}

func newScheduleCatalogListCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the builtin default scheduled jobs and where you've enabled them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			cat, err := c.ListScheduleCatalog(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(cat)
			}
			// Count enabled repos per slug so the ON column reads "N repo(s)" or "-".
			enabledBySlug := map[string]int{}
			for _, en := range cat.Enablements {
				enabledBySlug[en.Slug]++
			}
			rows := make([][]string, 0, len(cat.Entries))
			for _, e := range cat.Entries {
				rows = append(rows, []string{
					e.Slug,
					e.Target,
					e.Cron,
					catalogEnabledOn(enabledBySlug[e.Slug]),
					e.Name,
				})
			}
			return p.Table([]string{"SLUG", "TARGET", "CRON", "ENABLED", "NAME"}, rows)
		},
	}
}

// catalogEnabledOn renders the ENABLED column of `catalog list`: how many of the caller's
// repos have this default enabled, or "-" when none.
func catalogEnabledOn(n int) string {
	if n == 0 {
		return "-"
	}
	if n == 1 {
		return "1 repo"
	}
	return fmt.Sprintf("%d repos", n)
}

func newScheduleCatalogEnableCmd(env Env, gf *globalFlags) *cobra.Command {
	enable := &cobra.Command{
		Use:   "enable <slug>",
		Short: "Enable a builtin default job on one or more repos",
		Long: "Enable a builtin default scheduled job (by SLUG from `uzi schedule catalog list`)\n" +
			"on one or more repos. Multi-repo enablement is a CLIENT-SIDE fan-out: this issues one\n" +
			"idempotent per-repo enable call per --repo, so a partial retry is safe and re-enabling\n" +
			"a repo simply reports it as already enabled.\n\n" +
			"For a SWEEP default (one that selects issues by label), the enable first checks each\n" +
			"target repo for the selector label and WARNS on any that is missing (the schedule is\n" +
			"still created — the sweep just will not match until the label exists). Pass\n" +
			"--create-missing-labels to create the missing labels on the forge FIRST, then enable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := strings.TrimSpace(args[0])
			if slug == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a catalog slug is required (list them with the catalog list command)")
			}
			repos, _ := cmd.Flags().GetStringArray("repo")
			repos = nonBlankTrimmed(repos)
			if len(repos) == 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "at least one --repo is required (a repo id from `uzi repo list`)")
			}
			createMissing, _ := cmd.Flags().GetBool("create-missing-labels")
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// Sweep-label guardrail (PRD #589 M4): resolve the selector labels for this slug
			// from the catalog (a default row stores NULL labels; the catalog is the source of
			// truth). Only a sweep default carries labels — for a prompt default this is empty
			// and the guardrail is a no-op.
			selector, err := resolveCatalogSweepLabels(cmd.Context(), c, slug)
			if err != nil {
				return err
			}
			runSweepLabelGuardrail(cmd, env, gf, c, repos, selector, createMissing)
			// Client-side fan-out: one idempotent per-repo enable per --repo. Accumulate the
			// per-repo results so --json returns them all and a mid-loop failure still reports
			// what already landed BEFORE the error propagates (the per-repo endpoint is
			// idempotent, so a partial landing plus a retry is safe).
			results := make([]catalogEnableResult, 0, len(repos))
			for _, repoID := range repos {
				s, created, eerr := c.EnableCatalogSchedule(cmd.Context(), repoID, slug)
				if eerr != nil {
					_ = renderCatalogEnableResults(env, gf, results)
					return eerr
				}
				results = append(results, catalogEnableResult{RepoID: repoID, ScheduleID: s.ID, Created: created})
			}
			return renderCatalogEnableResults(env, gf, results)
		},
	}
	enable.Flags().StringArray("repo", nil, "a repo id to enable this default on (repeatable for multi-repo enablement)")
	enable.Flags().Bool("create-missing-labels", false, "for a sweep default: create any selector label missing on a target repo before enabling (default: warn only)")
	return enable
}

// resolveCatalogSweepLabels returns the sweep selector labels for a catalog slug, resolved
// from the catalog list (a default schedule stores NULL labels, so the catalog is the
// source of truth). It returns an empty slice for a prompt default or an unknown slug — in
// which case the guardrail is a no-op and the enable's own call surfaces any real 404.
func resolveCatalogSweepLabels(ctx context.Context, c uzicli.Client, slug string) ([]string, error) {
	cat, err := c.ListScheduleCatalog(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range cat.Entries {
		if e.Slug == slug {
			if e.Target == schedTargetSweep {
				return e.Labels, nil
			}
			return nil, nil
		}
	}
	return nil, nil
}

// runSweepLabelGuardrail warns on (or, when createMissing is set, creates) any selector
// label missing on each target repo BEFORE the schedule is enabled/created/edited (PRD
// #589 M4).
//
// It is PURELY ADVISORY and NEVER blocks the write — not even on its own forge errors. A
// failed label CHECK (expired token, rate limit, forge unreachable) prints a WARNING to
// stderr and proceeds, so a transient forge outage can never abort an enable/create/edit
// that otherwise needs no forge read at all (a `catalog enable` computes next_fire_at from
// the catalog cron and did no forge read before this guardrail existed). A failed label
// CREATE (with --create-missing-labels) likewise warns and proceeds: the schedule is the
// primary goal and the label is idempotently retryable. A missing-but-checked label just
// means the sweep will not match until it exists. Diagnostics go to stderr so a --json
// stdout stays clean; an empty selector (a non-sweep target) makes this a no-op.
func runSweepLabelGuardrail(cmd *cobra.Command, env Env, gf *globalFlags, c uzicli.Client, repos, selector []string, createMissing bool) {
	if len(selector) == 0 {
		return
	}
	for _, repoID := range repos {
		missing, err := c.CheckRepoLabels(cmd.Context(), repoID, selector)
		if err != nil {
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stderr,
					"WARNING: could not check labels on repo %s: %v; proceeding anyway\n", repoID, err)
			}
			continue
		}
		if len(missing) == 0 {
			continue
		}
		if createMissing {
			if err := c.EnsureRepoLabels(cmd.Context(), repoID, missing); err != nil {
				if !gf.quiet {
					_, _ = fmt.Fprintf(env.Stderr,
						"WARNING: could not create label(s) %s on repo %s: %v; proceeding anyway (the label is idempotently retryable)\n",
						strings.Join(missing, ", "), repoID, err)
				}
				continue
			}
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stderr, "created missing label(s) %s on repo %s\n", strings.Join(missing, ", "), repoID)
			}
			continue
		}
		if !gf.quiet {
			_, _ = fmt.Fprintf(env.Stderr,
				"WARNING: label(s) %s do not exist on repo %s — the sweep will not match until they are created (re-run with --create-missing-labels to create them)\n",
				strings.Join(missing, ", "), repoID)
		}
	}
}

// catalogEnableResult is one per-repo outcome of the multi-repo `catalog enable` fan-out.
type catalogEnableResult struct {
	RepoID     string `json:"repo_id"`
	ScheduleID string `json:"schedule_id"`
	Created    bool   `json:"created"`
}

// renderCatalogEnableResults prints the per-repo enable outcomes: under --json the whole
// slice, in human mode one "enabled/already enabled on <repo>" line per row. Called both on
// success and, with the partial slice, on a mid-loop failure so what already landed is
// legible before the error propagates.
func renderCatalogEnableResults(env Env, gf *globalFlags, results []catalogEnableResult) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(results)
	}
	if gf.quiet {
		return nil
	}
	for _, res := range results {
		state := "already enabled"
		if res.Created {
			state = "enabled"
		}
		p.Printf("%s on %s · schedule %s\n", state, res.RepoID, res.ScheduleID)
	}
	return nil
}

func newScheduleResetCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reset <schedule-id>",
		Short: "Reset a default schedule's edited fields back to the catalog defaults",
		Long: "Restore a default-origin schedule's editable fields (cron, timezone, model,\n" +
			"auto-approve, wait-on-limit, max-issues) to the builtin catalog values and clear its\n" +
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

// buildScheduleRequest assembles the ScheduleRequest from the create flags, enforcing
// the one-of TARGET and one-of TIMING constraints client-side so a bad invocation is a
// clean exit-2 usage error before any request is sent (the server also enforces them).
// It returns the request and the (one or more) repo ids to fan the create out across.
func buildScheduleRequest(cmd *cobra.Command) (apitypes.ScheduleRequest, []string, error) {
	repos, _ := cmd.Flags().GetStringArray("repo")
	repos = nonBlankTrimmed(repos)
	if len(repos) == 0 {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--repo is required (a repo id from `uzi repo list`)")
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
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage,
			"specify exactly one target: --issue <iid>, --sweep, or --prompt <text>")
	}
	if len(labels) > 0 && !sweep {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--label is only valid with --sweep")
	}
	// --max-issues is sweep-only; reject an EXPLICIT set on a non-sweep target (an
	// unchanged default is silently ignored, mirroring the --label rule above).
	if cmd.Flags().Changed("max-issues") && !sweep {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--max-issues is only valid with --sweep")
	}
	// --guidance is issue/sweep-only; reject an EXPLICIT set on the prompt target (a prompt
	// carries its own text). --guidance is distinct from the --prompt target selector.
	guidanceSet := cmd.Flags().Changed("guidance")
	if guidanceSet && !issueSet && !sweep {
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--guidance is only valid with --issue or --sweep")
	}

	req := apitypes.ScheduleRequest{}
	switch {
	case issueSet:
		if issue <= 0 {
			return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--issue must be a positive issue IID")
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
			return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage, "--prompt needs task text")
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
		return apitypes.ScheduleRequest{}, nil, uzicli.Exitf(uzicli.ExitUsage,
			"specify exactly one timing: --at <RFC3339> (once) or --cron <expr> (recurring)")
	}
	tz, _ := cmd.Flags().GetString("tz")
	req.Timezone = strings.TrimSpace(tz)
	if atSet {
		runAt, err := parseAt(atStr)
		if err != nil {
			return apitypes.ScheduleRequest{}, nil, err
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

	// mr_rework is tri-state and its schedule default is INHERIT (nil), not on (PRD #841
	// D5) — so it is Changed()-gated via mrReworkFlag rather than always-sent like
	// wait_on_limit above: an omitted flag stays nil so the fired jobs follow the owner's
	// global setting, and only an explicit --mr-rework[=false] stamps the schedule.
	req.MrReworkEnabled = mrReworkFlag(cmd)

	// --enabled is only sent when the caller passed it, so an omitted flag stays nil and
	// the server's create default (enabled=true) applies. Use Changed() rather than the
	// always-send pointer pattern of --auto-approve so today's default behavior is byte-identical.
	if cmd.Flags().Changed("enabled") {
		enabled, _ := cmd.Flags().GetBool("enabled")
		req.Enabled = &enabled
	}
	return req, repos, nil
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
	edit.Flags().Bool("apply-model-to-agents", false, "set whether the schedule's model also overrides every subagent's model pin")
	edit.Flags().String("model", "", "change the model alias (opus/sonnet/haiku/fable) or custom model ID for runs this schedule fires; empty string clears it back to your Worker-model default (valid on every target)")
	edit.Flags().String("repo", "", "repoint the schedule to another repo by id (sweep/prompt targets; an issue-target schedule cannot be repointed)")
	edit.Flags().Bool("create-missing-labels", false, "for a sweep target: create any newly-set --label missing on the schedule's repo before saving the edit (default: warn only)")
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
	if s.Origin == schedOriginDefault {
		return buildDefaultScheduleEditRequest(cmd, s)
	}
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
	// mr_rework is a tri-state *bool (PRD #841): RESTATE the fetched value so a partial edit
	// (e.g. --cron only) does not wipe the stored override under the server's replace-semantics
	// — mirroring model/wait_on_limit. Its inherit state is nil, and restating nil re-sends
	// nil, so an inherit schedule stays inherit. An explicit --mr-rework overrides below.
	req.MrReworkEnabled = s.MrReworkEnabled

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
	if f.Changed("mr-rework") {
		v, _ := f.GetBool("mr-rework")
		req.MrReworkEnabled = &v
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
	if f.Changed("model") {
		v, _ := f.GetString("model")
		req.Model = &v
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

// buildDefaultScheduleEditRequest builds the PATCH body for a `schedule edit` on a
// DEFAULT-origin schedule (PRD #589). The server routes a default row to
// patchDefaultScheduleConfig, whose guard rejects any request carrying a catalog-owned
// field (prompt/labels/guidance/target/repo/issue/timing/run_at). So — unlike the user
// path's full rebuild — this constructs a FRESH minimal request with ONLY the
// catalog-editable fields set, leaving the catalog-owned ones at their zero value so the
// guard passes. A copy-then-zero approach would NOT work: a prompt-target default's DTO
// surfaces Guidance as a non-nil &"" (scheduleDTO), which trips the guard's
// `req.Guidance != nil` check even when the user passed no --guidance.
//
// User-editable on a default: cron, timezone, auto_approve, wait_on_limit, and — for a
// sweep — max_issues. For a sweep, max_issues is RESTATED from the fetched row because
// patchDefaultScheduleConfig uses replace-semantics on it (an omitted value clears the cap
// to unlimited). model and override_subagent_model ARE now owner-editable on a default
// (issue #691): --model sets the run model (empty string clears it back to the Worker-model
// default) and --apply-model-to-agents sets override_subagent_model, since
// patchDefaultScheduleConfig reads both req.Model and req.OverrideSubagentModel. Both are
// restated from the fetched row (replace-semantics) so a partial edit does not drop them,
// and an explicit flag overrides the restated value.
//
// Guidance is owner-editable on a PROMPT-target default (PRD #662 M1) and a SWEEP-target
// default (issue #675, where it is an overlay composed onto the baked catalog guidance at
// fire time): --guidance sets it and --clear-guidance blanks it, and the fetched value is
// RESTATED so a partial edit does not wipe it under the server's replace-semantics (for a
// sweep default the restated value is the OVERLAY, never the baked catalog value). On an
// issue/self_improve default guidance stays catalog-owned, so those flags are rejected
// client-side. The remaining catalog-owned flags (--prompt/--label/--repo/--at) are likewise
// rejected client-side with a usage error pointing at `schedule clone`.
func buildDefaultScheduleEditRequest(cmd *cobra.Command, s apitypes.ScheduleDTO) (apitypes.ScheduleRequest, error) {
	f := cmd.Flags()

	// Catalog-owned fields cannot be edited on a default; fail fast client-side with a
	// message naming clone, rather than forwarding to a server 400. Guidance is the one
	// exception: on a PROMPT-target default (PRD #662 M1) or a SWEEP-target default (issue
	// #675) the owner may edit it, so it is handled below rather than in this blanket reject.
	for _, flag := range []string{"prompt", "label", "repo", "at"} {
		if f.Changed(flag) {
			return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
				"--%s is catalog-owned on a default schedule; clone it first with `uzi schedule clone`", flag)
		}
	}
	// Guidance is owner-editable on a prompt-target default (issue #662) and on a sweep-target
	// default (issue #675: an owner overlay composed onto the baked catalog guidance at fire
	// time). On an issue/self_improve default it stays catalog-owned (the server still 400s a
	// guidance edit), so reject it here.
	guidanceSet := f.Changed("guidance")
	clearGuidance := f.Changed("clear-guidance")
	if (guidanceSet || clearGuidance) && s.Target != schedTargetPrompt && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"--guidance/--clear-guidance are catalog-owned on this default schedule; clone it first with `uzi schedule clone`")
	}
	if guidanceSet && clearGuidance {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--guidance and --clear-guidance are mutually exclusive")
	}
	maxIssuesSet := f.Changed("max-issues")
	clearMaxIssues := f.Changed("clear-max-issues")
	if (maxIssuesSet || clearMaxIssues) && s.Target != schedTargetSweep {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues/--clear-max-issues are only valid on a sweep target")
	}
	if maxIssuesSet && clearMaxIssues {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage, "--max-issues and --clear-max-issues are mutually exclusive")
	}

	// FRESH minimal request: only catalog-editable fields. Restate model/override from the
	// fetched row so a partial edit keeps them; an explicit --model/--apply-model-to-agents
	// overrides below (issue #691 — see the doc comment). Leave
	// Target/Labels/Prompt/Guidance/IssueIID/Timing/RunAt/RepoID at zero so the guard passes.
	req := apitypes.ScheduleRequest{
		CronExpr:              s.CronExpr,
		Timezone:              s.Timezone,
		Model:                 s.Model,
		OverrideSubagentModel: s.OverrideSubagentModel,
	}
	autoApprove := s.AutoApprove
	waitOnLimit := s.WaitOnLimit
	req.AutoApprove = &autoApprove
	req.WaitOnLimit = &waitOnLimit
	// mr_rework (PRD #841): patchDefaultScheduleConfig uses replace-semantics on it, so RESTATE
	// the fetched tri-state value or a partial edit (e.g. --cron alone) sends nil and wipes the
	// stored override to inherit. Restating nil re-sends inherit; an explicit --mr-rework overrides.
	req.MrReworkEnabled = s.MrReworkEnabled
	// max_issues is meaningful only for a sweep default; restate it so a partial edit keeps
	// the stored cap (the server clears it to unlimited on an omitted value).
	if s.Target == schedTargetSweep {
		req.MaxIssues = s.MaxIssues
	}
	// Guidance on a PROMPT-target (issue #662) or SWEEP-target (issue #675) default is
	// owner-editable and uses replace-semantics on the server, so RESTATE the fetched value —
	// otherwise a partial edit (e.g. --cron alone) would send a nil guidance and wipe the
	// stored value. For a sweep default s.Guidance is the OVERLAY (nil when no overlay is set;
	// the baked catalog value is in s.BakedGuidance and is NEVER restated), so this never
	// echoes the baked value back into the column. The server guard accepts a nil guidance
	// (keeps NULL, no wipe). Issue/self_improve defaults never reach here with a guidance flag,
	// so they still send no guidance.
	if s.Target == schedTargetPrompt || s.Target == schedTargetSweep {
		req.Guidance = s.Guidance
	}

	changed := false
	if f.Changed("cron") {
		cron, _ := f.GetString("cron")
		// Leave Timing empty: patchDefaultScheduleConfig always writes recurring.
		req.CronExpr = cron
		changed = true
	}
	if f.Changed("tz") {
		tz, _ := f.GetString("tz")
		req.Timezone = strings.TrimSpace(tz)
		changed = true
	}
	if f.Changed("auto-approve") {
		v, _ := f.GetBool("auto-approve")
		req.AutoApprove = &v
		changed = true
	}
	if f.Changed("wait-on-limit") {
		v, _ := f.GetBool("wait-on-limit")
		req.WaitOnLimit = &v
		changed = true
	}
	if f.Changed("mr-rework") {
		v, _ := f.GetBool("mr-rework")
		req.MrReworkEnabled = &v
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
	// Guidance overlay (prompt- or sweep-target default; guarded above). Restating the fetched
	// value does NOT by itself count as a change — only an explicit flag flips `changed`.
	if guidanceSet {
		v, _ := f.GetString("guidance")
		req.Guidance = &v
		changed = true
	}
	if clearGuidance {
		// Empty string, not nil: the server treats a blank guidance as NULL, and a non-nil
		// pointer keeps the request acceptable to the prompt-default guard.
		empty := ""
		req.Guidance = &empty
		changed = true
	}
	// --model is owner-editable on a default (issue #691): patchDefaultScheduleConfig reads
	// req.Model. Restated from the fetched row above; an explicit flag overrides (empty clears).
	if f.Changed("model") {
		v, _ := f.GetString("model")
		req.Model = &v
		changed = true
	}
	// --apply-model-to-agents is owner-editable on a default (issue #691):
	// patchDefaultScheduleConfig reads req.OverrideSubagentModel. Restated from the fetched
	// row above; an explicit flag overrides.
	if f.Changed("apply-model-to-agents") {
		v, _ := f.GetBool("apply-model-to-agents")
		req.OverrideSubagentModel = &v
		changed = true
	}
	if !changed {
		return apitypes.ScheduleRequest{}, uzicli.Exitf(uzicli.ExitUsage,
			"nothing to edit (pass at least one editable field: --cron, --tz, --auto-approve, --wait-on-limit, --mr-rework, --max-issues, --guidance, --model, --apply-model-to-agents)")
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

// renderCreatedSchedules prints the schedules created by a multi-repo `schedule create`
// fan-out: under --json the whole slice, in human mode one "created schedule …" line per
// row. Called both on success and, with the partial slice, on a mid-loop failure so the
// schedules that already landed are reported before the error propagates.
func renderCreatedSchedules(env Env, gf *globalFlags, created []apitypes.ScheduleDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(created)
	}
	if gf.quiet {
		return nil
	}
	for _, s := range created {
		p.Printf("created schedule %s · %s · next %s\n", s.ID, scheduleWhen(s), scheduleNext(s, time.Now()))
	}
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
	// A sweep default surfaces the read-only baked catalog guidance separately from the owner
	// overlay (issue #675); BakedGuidance is nil for every other row, so the row is emitted
	// only when present.
	if s.BakedGuidance != nil {
		rows = append(rows, []string{"BAKED_GUIDANCE", strOr(s.BakedGuidance, "-")})
	}
	rows = append(rows,
		[]string{"MODEL", strOr(s.Model, "-")},
		[]string{"APPLY_MODEL_TO_AGENTS", boolStr(s.OverrideSubagentModel != nil && *s.OverrideSubagentModel)},
		[]string{"AUTO_APPROVE", boolStr(s.AutoApprove)},
		[]string{"WAIT_ON_LIMIT", boolStr(s.WaitOnLimit)},
		[]string{"MR_REWORK", triStateStr(s.MrReworkEnabled)},
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
	"not_eligible": "add the configured uzi label or assign the issue to uzi, or raise --max-issues",
}

// skipReasonHint returns the remediation hint for a skip reason, or "" when none applies.
func skipReasonHint(reason string) string { return skipReasonHints[reason] }

// lastFireCappedHint is the one-line steer shown when a capped fire started nothing and
// every examined candidate was skipped — the newest issues were never reached.
const lastFireCappedHint = "newer issues not reached — raise --max-issues, or add the configured uzi label / assign the issue to uzi"

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
