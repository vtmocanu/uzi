package main

// run_lifecycle.go holds the run lifecycle verbs — create/approve/reject/cancel/
// stop (PRD #1009 M4).

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunCreateCmd builds `uzi run create`.
func newRunCreateCmd(env Env, gf *globalFlags) *cobra.Command {
	create := &cobra.Command{
		Use:   "create",
		Short: "Start a run on a repo's PRD issue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoID, _ := cmd.Flags().GetString("repo")
			issue, _ := cmd.Flags().GetInt64("issue")
			if strings.TrimSpace(repoID) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--repo is required (a repo id from `uzi repo list`)")
			}
			if issue <= 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "--issue must be a positive issue IID")
			}
			seed, err := seededPlanFlag(env, cmd)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			credOverride, err := credentialOverrideFlag(cmd, c)
			if err != nil {
				return err
			}
			force, _ := cmd.Flags().GetBool("force")
			run, err := c.CreateRun(cmd.Context(), repoID, issue, waitOnLimitFlag(cmd), mrReworkFlag(cmd), force, seed, credOverride)
			if err != nil {
				return err
			}
			return renderCreatedRun(env, gf, run)
		},
	}
	create.Flags().String("repo", "", "repo id to run against (see 'uzi repo list')")
	create.Flags().Int64("issue", 0, "the PRD issue IID to run")
	create.Flags().Bool("wait-on-limit", false,
		"park this run until the Anthropic usage window reopens instead of failing it; "+
			"omit to inherit your Settings default, or pass --wait-on-limit=false to force off")
	create.Flags().Bool("mr-rework", false,
		"enable or disable auto-rework of this run's MR review comments; "+
			"omit to inherit the account default, or pass --mr-rework=false to force off")
	create.Flags().Bool("force", false,
		"re-run even if the issue already has an open MR from a completed run "+
			"(bypasses only the open-MR guard; a run already in progress is never bypassed)")
	// PRD #209 seeded plan. --agent-source/--exclude-agents reuse the plan gate's flag
	// names and validation (approveSelection); both are meaningful only alongside a plan.
	create.Flags().String("plan-file", "",
		"seed the run with a pre-written plan from this file (or '-' for stdin), skipping "+
			"the planning turn and the approval gate (PRD #209)")
	create.Flags().String("agent-source", "",
		"which subagent roster the seeded run uses: own|repo (requires --plan-file)")
	create.Flags().StringSlice("exclude-agents", nil,
		"subagents to drop from the chosen source (requires --agent-source and --plan-file)")
	// PRD #209 M4 staleness guard. --planned-commit records the commit the plan was
	// written against; the worker warns (or, with --require-base, fails) if the clone's
	// base has moved since. Both require --plan-file, and --require-base requires
	// --planned-commit.
	create.Flags().String("planned-commit", "",
		"the commit the seeded plan was written against; the worker warns if the clone's "+
			"base has moved since (requires --plan-file)")
	create.Flags().Bool("require-base", false,
		"fail the run instead of warning if the clone's base differs from --planned-commit "+
			"(requires --planned-commit)")
	// PRD #1247 M2: the per-run Anthropic credential choice. Three-way like --wait-on-limit
	// and --mr-rework: omitted inherits the worker binding; a keyword forces a mode; any
	// other value is a token label resolved to a pinned choice.
	create.Flags().String("token", "",
		"which Anthropic token this run spends: a token label (pins the run to it), "+
			"'auto' (auto-select from your pool), 'default' (your default token), or 'inherit' "+
			"(the worker's binding); omit to inherit")
	return create
}

// credentialOverrideFlag resolves `uzi run create --token` into the wire credential override
// (PRD #1247 M2). It is three-way like waitOnLimitFlag/mrReworkFlag and mirrors `worker
// set-token` / `token pool`'s CLIENT-SIDE label resolution:
//
//   - flag OMITTED                    → nil (send no credential_override; inherit the worker
//     binding, byte-identical to a pre-#1247 create)
//   - --token auto|default|inherit    → that mode, carrying no secret
//   - --token <label>                 → resolve the label to an anthropic_token id via
//     ListSecrets + findSecretByLabel; a no-match is a clean CLIENT-SIDE refusal naming the
//     read commands (never a round-trip), and a match sends {pinned, secret_id}
//
// ListSecrets is called ONLY for a label (the three keywords need no server round-trip).
func credentialOverrideFlag(cmd *cobra.Command, c uzicli.Client) (*uzicli.CreateRunCredentialOverride, error) {
	if !cmd.Flags().Changed("token") {
		return nil, nil
	}
	val, _ := cmd.Flags().GetString("token")
	switch val {
	case "auto", "default", "inherit":
		return &uzicli.CreateRunCredentialOverride{Mode: val}, nil
	}
	// Any other value is a token LABEL, resolved client-side to a pinned choice. The
	// resolution kind-filters to anthropic_token (a credential override re-points Anthropic
	// spend only), exactly as `uzi token pool` does now that ListSecrets returns codex kinds.
	secrets, err := c.ListSecrets(cmd.Context())
	if err != nil {
		return nil, err
	}
	target, ok := findSecretByLabel(secrets, kindAnthropicToken, val)
	if !ok {
		// A NON-anthropic secret carrying this label: name its kind so the refusal reads
		// true (an override applies only to Anthropic tokens) rather than "no such token".
		if other, found := findSecretByLabel(secrets, "", val); found {
			return nil, uzicli.Exitf(uzicli.ExitUsage,
				"--token accepts only Anthropic tokens; %q is a %s credential", val, tokenKindAlias(other.Kind))
		}
		// A usage error, not a 404: the label never reached the server, so report what
		// actually happened — the caller holds no Anthropic token by that name. Names the
		// read command, exactly like `uzi token pool`'s own unknown-label refusal.
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"no Anthropic token labelled %q; `uzi token list` shows yours", val)
	}
	return &uzicli.CreateRunCredentialOverride{Mode: "pinned", SecretID: target.ID}, nil
}

// newRunApproveCmd builds `uzi run approve`.
func newRunApproveCmd(env Env, gf *globalFlags) *cobra.Command {
	approve := &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			source, _ := cmd.Flags().GetString("agent-source")
			exclude, _ := cmd.Flags().GetStringSlice("exclude-agents")
			sel, err := approveSelection(source, exclude)
			if err != nil {
				return err
			}
			return submitInput(env, gf, c, cmd, args[0], kindApprovePlan, "", sel)
		},
	}
	approve.Flags().String("agent-source", "", "which subagent roster to run: own|repo (default: the run's own default)")
	approve.Flags().StringSlice("exclude-agents", nil, "subagents to drop from the chosen source (requires --agent-source)")
	return approve
}

// newRunRejectCmd builds `uzi run reject`.
func newRunRejectCmd(env Env, gf *globalFlags) *cobra.Command {
	reject := &cobra.Command{
		Use:   "reject <run-id>",
		Short: "Reject a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			if strings.TrimSpace(msg) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a rejection needs a reason: pass -m <reason> or pipe it on stdin")
			}
			return submitInput(env, gf, c, cmd, args[0], kindRejectPlan, msg, nil)
		},
	}
	reject.Flags().StringP("message", "m", "", "reason to send back to the agent (or pipe it on stdin)")
	return reject
}

// newRunCancelCmd builds `uzi run cancel`.
func newRunCancelCmd(env Env, gf *globalFlags) *cobra.Command {
	cancel := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// PRD #503 M3: the cancel reason is OPTIONAL — unlike reject, no empty check.
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			return submitInput(env, gf, c, cmd, args[0], kindCancel, msg, nil)
		},
	}
	cancel.Flags().StringP("message", "m", "", "reason for cancelling (optional; or pipe it on stdin)")
	return cancel
}

// newRunStopCmd builds `uzi run stop`.
func newRunStopCmd(env Env, gf *globalFlags) *cobra.Command {
	stop := &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Gracefully stop a run: interactive (finalize) or milestone (cap at completed count)",
		Long: "Gracefully wind down a run (PRD #517, #634). Unlike `cancel`, which aborts mid-turn, " +
			"`stop` lets the worker FINALIZE.\n\n" +
			"On an interactive task run it finishes the current turn, pushes the branch, opens the " +
			"merge request (when the run requested one), and reports `completed`. The stop is " +
			"serviced ahead of any buffered follow-up.\n\n" +
			"On a milestone-structured issue run it maps to a scope ceiling at the ALREADY-COMPLETED " +
			"milestone count: the run finalizes the committed slice (pushes the branch, opens the " +
			"merge request when requested) and starts no further milestone — the same graceful " +
			"finalize, just scoped to what is already done. Use `run scope --through N` instead to " +
			"complete through a later milestone before finalizing.\n\n" +
			"An optional message can accompany the stop (pass -m or pipe it on stdin). A stop on a " +
			"run that has already finished answers 409 (exit 5).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// The stop message is OPTIONAL, like a cancel reason — no empty check.
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			return submitInput(env, gf, c, cmd, args[0], kindStop, msg, nil)
		},
	}
	stop.Flags().StringP("message", "m", "", "an optional message to accompany the stop (or pipe it on stdin)")
	return stop
}
