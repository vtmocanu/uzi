package main

// run_limits.go holds the run queue/limit verbs — expedite/resume-now/mr-rework
// (PRD #1009 M4).

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunExpediteCmd builds `uzi run expedite`.
func newRunExpediteCmd(env Env, gf *globalFlags) *cobra.Command {
	expedite := &cobra.Command{
		Use:   "expedite <run-id>",
		Short: "Bump a queued run to the front of the claim queue (or --clear to undo)",
		Long: "Bump ONE queued run to the front of the claim queue so a worker picks it up before " +
			"the rest (PRD #320). It matters only before a run is claimed: ordering is fixed once a " +
			"worker takes the run, so a non-queued run is a 409 (exit 5). A foreign or unknown run is " +
			"a 404 (exit 4).\n\n" +
			"`--clear` undoes it — it removes the manual override and returns the run to its kind " +
			"default priority (it does NOT demote the run below normal).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// --clear removes the manual override (expedite=false); its absence expedites.
			clear, _ := cmd.Flags().GetBool("clear")
			run, err := c.SetRunPriority(cmd.Context(), args[0], !clear)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	expedite.Flags().Bool("clear", false, "clear the manual expedite (undo), returning the run to its kind default priority")
	return expedite
}

// newRunResumeNowCmd builds `uzi run resume-now`.
func newRunResumeNowCmd(env Env, gf *globalFlags) *cobra.Command {
	resumeNow := &cobra.Command{
		Use:   "resume-now <run-id>",
		Short: "Resume a run held waiting for a pooled Anthropic token, without waiting for the sweeper",
		Long: "Resume ONE run held in `pool_wait` — an `auto` run parked because its owner's Anthropic " +
			"token pool was empty when it claimed (PRD #754). It flips the hold straight to `queued` " +
			"instead of waiting up to a sweeper tick for the reactive pass to notice a token was pooled.\n\n" +
			"A run that is NOT held is a 409 (exit 5); a foreign or unknown run is a 404 (exit 4). No " +
			"token is spent and nothing is written to the forge — it only releases the hold.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.ResumeRunNow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	return resumeNow
}

// newRunResumeCmd builds `uzi run resume` (PRD #1190 M4).
//
// It posts to the SAME widened /runs/{id}/resume-now endpoint `run resume-now` uses (D14:
// one resume mechanism), which now moves a `paused` run back to `queued` as well as a
// `pool_wait` hold. A plain `resume` verb is added beside `resume-now` because "resume-now"
// reads wrong for a run nobody else was going to resume; `resume-now` keeps its name and its
// pool-hold wording.
//
// It prints a one-line confirmation rather than the full detail table. The resume response is
// a plain RunDTO that carries neither the worker's NAME nor its liveness (draining/gone),
// which the richer "pinned to worker <name> …" wording would need — and the PRD is explicit
// that a round-trip must not be added just for that wording — so the plain form is what a CLI
// resume prints today. --json emits the run object for the agent contract.
func newRunResumeCmd(env Env, gf *globalFlags) *cobra.Command {
	resume := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Resume a paused run, moving it back to the queue with its remaining budget preserved",
		Long: "Resume ONE run the owner paused (`paused`, PRD #1190): it flips the hold back to `queued`, " +
			"keeps the worker pin, and banks the parked time so the remaining budget is what it was — the " +
			"clock stopped while paused. The claim then continues the SDK session on the same worker if it " +
			"is still alive, or recovers the branch from the checkpoint on another worker if it is gone.\n\n" +
			"A run that is NOT paused is a 409 (exit 5); a foreign or unknown run is a 404 (exit 4). It " +
			"posts to the same endpoint as `uzi run resume-now`, which also resumes a pool-held run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.ResumeRunNow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			if !gf.quiet {
				p.Printf("Resumed %s: queued.\n", args[0])
			}
			return nil
		},
	}
	return resume
}

// newRunSetTokenCmd builds `uzi run set-token` (PRD #1247 M4, D12).
//
// `uzi run set-token <run-id> <label>|--auto|--default|--inherit` re-points which Anthropic
// token ONE run spends: the positional LABEL pins the run to that token; the three flags
// force a mode. Exactly one of {label, --auto, --default, --inherit} is required and they
// are mutually exclusive (mirroring `--token` on `run create`, D12). A label is resolved to
// an anthropic_token id CLIENT-SIDE (ListSecrets + findSecretByLabel), so an unknown or
// wrong-kind label is refused before any request; the keywords need no round-trip.
//
// On a queued run the switch takes effect at the next claim; on a parked run it promotes
// the run back to queued at once; on a RUNNING or gated run held by a capability worker it
// REQUESTS the switch, which takes effect when the worker releases its claim (PRD #1247 M5).
// The server may return a D6 WARNING (no headroom, auto will hold in pool_wait) on the 200
// without refusing — it is printed to STDERR regardless of --format (mirroring
// renderCreatedRun), so a scripted/--json consumer still sees the advisory while stdout
// stays a bare RunDTO.
func newRunSetTokenCmd(env Env, gf *globalFlags) *cobra.Command {
	setToken := &cobra.Command{
		Use:   "set-token <run-id> [<label>]",
		Short: "Switch which Anthropic token a run spends",
		Long: "Re-point which Anthropic token ONE run spends (PRD #1247). A positional token " +
			"LABEL pins the run to that token; `--auto` auto-selects from your pool, `--default` " +
			"uses your default token, and `--inherit` clears the override back to the worker's " +
			"binding. Exactly one of {label, --auto, --default, --inherit} is required and they " +
			"are mutually exclusive.\n\n" +
			"On a QUEUED run it takes effect at the next claim; on a PARKED run (limit_wait, " +
			"pool_wait, recovery_wait, paused) it promotes the run back to `queued` at once so the " +
			"next claim spends the chosen token. On a RUNNING or gated run held by a capable worker " +
			"the switch is REQUESTED and takes effect when the worker releases its claim; it is refused " +
			"(409, exit 5) only when no live worker holds the run or the holding worker predates the " +
			"credential_switch capability. A foreign or unknown run — or an unknown token label — is " +
			"refused (404 / usage); a codex run is 422.\n\n" +
			"It may print a warning (the token has no headroom, or auto will hold in pool_wait) " +
			"WITHOUT refusing — the switch still applies.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			override, err := setTokenOverride(cmd, args, c)
			if err != nil {
				return err
			}
			run, warning, err := c.SetRunCredential(cmd.Context(), args[0], override)
			if err != nil {
				return err
			}
			// Route the D6 warning to STDERR regardless of --format so a --json consumer
			// still sees it while stdout stays a bare RunDTO, matching renderCreatedRun's
			// convention (server strings are sanitized before printing).
			if warning != "" {
				_, _ = fmt.Fprintf(env.Stderr, "warning: %s\n", sanitizeTTY(warning))
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	setToken.Flags().Bool("auto", false, "auto-select the token from your pool (mutually exclusive with a label / --default / --inherit)")
	setToken.Flags().Bool("default", false, "use your default token (mutually exclusive with a label / --auto / --inherit)")
	setToken.Flags().Bool("inherit", false, "clear the per-run override back to the worker binding (mutually exclusive with a label / --auto / --default)")
	return setToken
}

// setTokenOverride resolves `uzi run set-token`'s positional label + mode flags into the
// wire override (PRD #1247 M4, D12): exactly one of {label, --auto, --default, --inherit},
// mutually exclusive. A label is resolved CLIENT-SIDE to {pinned, secret_id} via
// ListSecrets + findSecretByLabel (kind-filtered to anthropic_token); an unknown label is a
// clean usage refusal naming `uzi token list`, and a wrong-kind label names its kind. The
// three keywords ride as a bare mode with no server round-trip.
func setTokenOverride(cmd *cobra.Command, args []string, c uzicli.Client) (uzicli.SetRunCredentialOverride, error) {
	auto, _ := cmd.Flags().GetBool("auto")
	def, _ := cmd.Flags().GetBool("default")
	inherit, _ := cmd.Flags().GetBool("inherit")
	var label string
	if len(args) == 2 {
		label = strings.TrimSpace(args[1])
	}

	// Exactly one of {label, --auto, --default, --inherit} (D12): count and gate.
	chosen := 0
	for _, on := range []bool{label != "", auto, def, inherit} {
		if on {
			chosen++
		}
	}
	if chosen == 0 {
		return uzicli.SetRunCredentialOverride{}, uzicli.Exitf(uzicli.ExitUsage,
			"set-token needs exactly one of: a token label, --auto, --default or --inherit")
	}
	if chosen > 1 {
		return uzicli.SetRunCredentialOverride{}, uzicli.Exitf(uzicli.ExitUsage,
			"a token label, --auto, --default and --inherit are mutually exclusive; pass exactly one")
	}

	switch {
	case auto:
		return uzicli.SetRunCredentialOverride{Mode: "auto"}, nil
	case def:
		return uzicli.SetRunCredentialOverride{Mode: "default"}, nil
	case inherit:
		return uzicli.SetRunCredentialOverride{Mode: "inherit"}, nil
	}

	// A token LABEL → a pinned choice, resolved client-side like `uzi run create --token`.
	secrets, err := c.ListSecrets(cmd.Context())
	if err != nil {
		return uzicli.SetRunCredentialOverride{}, err
	}
	target, ok := findSecretByLabel(secrets, kindAnthropicToken, label)
	if !ok {
		// A NON-anthropic secret carrying this label: name its kind so the refusal reads true.
		if other, found := findSecretByLabel(secrets, "", label); found {
			return uzicli.SetRunCredentialOverride{}, uzicli.Exitf(uzicli.ExitUsage,
				"set-token accepts only Anthropic tokens; %q is a %s credential", label, tokenKindAlias(other.Kind))
		}
		return uzicli.SetRunCredentialOverride{}, uzicli.Exitf(uzicli.ExitUsage,
			"no Anthropic token labelled %q; `uzi token list` shows yours", label)
	}
	return uzicli.SetRunCredentialOverride{Mode: "pinned", SecretID: target.ID}, nil
}

// newRunMrReworkCmd builds `uzi run mr-rework`.
func newRunMrReworkCmd(env Env, gf *globalFlags) *cobra.Command {
	mrRework := &cobra.Command{
		Use:   "mr-rework <run-id>",
		Short: "Set whether this run's MR review comments are auto-reworked (--enabled[=false], or --clear to inherit)",
		Long: "Set the per-run override for the MR review-rework watcher (PRD #841): whether new review " +
			"comments on this run's open MR are auto-reworked. Tri-state, editable on a COMPLETED run for as " +
			"long as its MR is still open (the watcher acts after the run finishes):\n\n" +
			"  --enabled            turn auto-rework ON for this run\n" +
			"  --enabled=false      turn it OFF (its MR is never auto-reworked)\n" +
			"  --clear              clear the override back to inherit (follow the account default)\n\n" +
			"A foreign or unknown run is a 404 (exit 4). The write is inert once the MR is merged or closed. " +
			"Prints the run's resulting MR_REWORK state (inherit/on/off).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// Three-way: --clear sends null (inherit); otherwise --enabled's value (default
			// true for a bare `mr-rework <id>`) is sent explicitly. --clear and --enabled
			// together is a usage error — they express opposite intents.
			clear, _ := cmd.Flags().GetBool("clear")
			var enabled *bool
			if clear {
				if cmd.Flags().Changed("enabled") {
					return uzicli.Exitf(uzicli.ExitUsage, "--clear and --enabled are mutually exclusive")
				}
				enabled = nil
			} else {
				v, _ := cmd.Flags().GetBool("enabled")
				enabled = &v
			}
			run, err := c.SetRunMrRework(cmd.Context(), args[0], enabled)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	mrRework.Flags().Bool("enabled", true, "whether this run's MR review comments are auto-reworked; pass --enabled=false to turn it off")
	mrRework.Flags().Bool("clear", false, "clear the per-run override back to inherit (follow the account default)")
	return mrRework
}

// newRunReworkCmd builds `uzi run rework`.
func newRunReworkCmd(env Env, gf *globalFlags) *cobra.Command {
	rework := &cobra.Command{
		Use:   "rework <run-id>",
		Short: "Start one on-demand MR-rework cycle on a completed run past the automatic cap",
		Long: "Start ONE on-demand MR-rework cycle on a completed run whose MR is open, PAST the " +
			"automatic cap (PRD #1202). It is how an owner reworks an MR after the automatic watcher has " +
			"stopped: it SKIPS the cap, the quiet-period debounce, the head-SHA staleness check and the " +
			"green-pipeline gate, but KEEPS the branch guard, the one-active-rework guard, the admin " +
			"kill-switch, the owner token and the open-MR requirement. The cycle does NOT count against " +
			"the automatic cap.\n\n" +
			"Guidance is optional: pass -m <text> (or pipe it on stdin) to steer the rework; an empty " +
			"guidance is a valid trigger as long as there is a new review comment (the server answers 409 " +
			"\"nothing to rework\" when neither a guidance nor a new comment applies). A foreign or unknown " +
			"run is a 404 (exit 4); a refusal — disabled, not reworkable, already running, or nothing new — " +
			"is a 409 (exit 5). Prints the created mr_rework run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			// Guidance is OPTIONAL here (unlike revise/follow-up, which reject an empty
			// message): an empty guidance is a valid trigger server-side, so resolveMessage's
			// result — the -m value, or a body piped on stdin when -m is absent, "" when
			// neither — is forwarded verbatim with no emptiness guard.
			guidance := resolveMessage(env, msg)
			run, err := c.RunRework(cmd.Context(), args[0], guidance)
			if err != nil {
				return err
			}
			// renderCreatedRun is `run create`'s renderer: renderRunDetail for humans and the
			// SAME {"run": <dto>} envelope for --json, since this creates a new mr_rework run.
			return renderCreatedRun(env, gf, run)
		},
	}
	rework.Flags().StringP("message", "m", "", "optional guidance to steer the rework (or pipe it on stdin)")
	return rework
}
