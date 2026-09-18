package main

// run_recovery.go is `uzi run recovery` and `uzi run discard` (PRD #1349 M5, D7/D9): the
// owner-side custody-hold LIST and exact hold DISCARD. `run recovery` fetches the owner-wide
// custody holds (GET /api/recovery/holds), narrows them to one run client-side, and renders
// each hold's exact id, generation, server-derived disposition (attention) and latest capture
// state. `run discard` targets ONE exact hold (DELETE .../recovery-holds/<hold>?confirm=discard,
// which the client always sends); it requires an interactive confirmation when --yes is absent
// and HARD-REFUSES when --yes is absent and stdin is not a TTY, so a possible only copy is never
// destroyed without a human decision. A cancelled/declined prompt performs NO mutation.

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunRecoveryCmd builds `uzi run recovery <run-id> [--json]`.
func newRunRecoveryCmd(env Env, gf *globalFlags) *cobra.Command {
	recovery := &cobra.Command{
		Use:   "recovery <run-id>",
		Short: "List a run's retained custody holds and their dispositions",
		Long: "Show the durable-recovery custody holds retained for a run: each hold's exact id, " +
			"claim generation, server-derived disposition, and the latest capture's state.\n\n" +
			"Owner-only: you see only your own runs' holds. A disposition of `source_only` or " +
			"`needs_action` awaits your decision — recover the archive with `run export` when one " +
			"is available, or discard the held source with `run discard <run-id> --hold <hold-id> " +
			"--yes`. An `archive_ready` hold releases itself once its archive is durable; `active` is " +
			"healthy protection of a still-running run and needs nothing.\n\n" +
			"--json emits the run's raw hold DTOs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			runID := args[0]
			holds, err := c.RecoveryHolds(cmd.Context())
			if err != nil {
				return err
			}
			// The endpoint is owner-wide; narrow to this run client-side.
			var forRun []apitypes.RecoveryCustodyHoldDTO
			for _, h := range holds.Holds {
				if h.RunID == runID {
					forRun = append(forRun, h)
				}
			}
			return renderRunRecovery(env, gf, runID, forRun)
		},
	}
	return recovery
}

// renderRunRecovery emits a run's custody holds. --json prints the raw filtered hold DTOs;
// the human form prints one table row per hold with its exact id, generation, disposition and
// capture state, plus a one-line hint when a hold needs an owner decision.
func renderRunRecovery(env Env, gf *globalFlags, runID string, holds []apitypes.RecoveryCustodyHoldDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		// Never emit a null slice: an empty result is [] so a consuming agent iterates it
		// unconditionally.
		if holds == nil {
			holds = []apitypes.RecoveryCustodyHoldDTO{}
		}
		return p.JSON(holds)
	}
	if len(holds) == 0 {
		if !gf.quiet {
			p.Printf("run %s has no retained custody holds\n", sanitizeTTY(runID))
		}
		return nil
	}
	rows := make([][]string, 0, len(holds))
	decisionNeeded := 0
	for _, h := range holds {
		if h.Attention == "source_only" || h.Attention == "needs_action" {
			decisionNeeded++
		}
		rows = append(rows, []string{
			h.ID,
			fmt.Sprintf("%d", h.Generation),
			sanitizeTTY(h.Attention),
			sanitizeTTY(h.State),
			captureCell(h.CaptureState),
			cellText(recoveryWorkerLabel(h)),
		})
	}
	if err := p.Table([]string{"HOLD ID", "GEN", "DISPOSITION", "STATE", "CAPTURE", "WORKER"}, rows); err != nil {
		return err
	}
	if decisionNeeded > 0 && !gf.quiet {
		p.Printf("\n%d hold(s) await a decision: recover with `uzi run export` or discard with "+
			"`uzi run discard %s --hold <hold-id> --yes`\n", decisionNeeded, sanitizeTTY(runID))
	}
	return nil
}

// captureCell renders a hold's latest capture state, or "-" when the hold has no capture yet.
func captureCell(state string) string {
	if strings.TrimSpace(state) == "" {
		return "-"
	}
	return sanitizeTTY(state)
}

// recoveryWorkerLabel is the owner-safe worker display for a hold: its bounded name, falling
// back to the opaque worker id when the worker row is gone. Raw provenance is never exposed.
func recoveryWorkerLabel(h apitypes.RecoveryCustodyHoldDTO) string {
	if strings.TrimSpace(h.WorkerName) != "" {
		return h.WorkerName
	}
	return h.WorkerID
}

// newRunDiscardCmd builds `uzi run discard <run-id> --hold <hold-id> [--yes]`.
func newRunDiscardCmd(env Env, gf *globalFlags) *cobra.Command {
	discard := &cobra.Command{
		Use:   "discard <run-id> --hold <hold-id> [--yes]",
		Short: "Discard one exact retained custody hold (a held source)",
		Long: "Discard ONE exact custody hold — the held source of a run's unpublished committed " +
			"work — so a blocked worker/PVC can be torn down and new runs can start.\n\n" +
			"This is destructive. The worker-local source may be the ONLY copy of that work, and no " +
			"server archive can restore it after discard; discard permits worker/PVC teardown that can " +
			"permanently destroy the work. An available recovery archive is NEVER deleted by this " +
			"command (export it first with `run export`, or delete it separately); only a hold's " +
			"in-progress/failed captures are settled.\n\n" +
			"You must confirm interactively, or pass --yes for an unattended run. Without a terminal " +
			"and without --yes the command refuses and changes nothing. List a run's holds with " +
			"`run recovery <run-id>`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := args[0]
			holdID := strings.TrimSpace(mustFlagString(cmd, "hold"))
			if holdID == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--hold <hold-id> is required (see `uzi run recovery %s`)", runID)
			}
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				// No --yes: an interactive confirmation is required. Without a TTY there is no
				// prompt to show, so HARD-REFUSE and mutate NOTHING (D9) rather than silently
				// destroying a possible only copy.
				if !env.StdinTTY {
					return uzicli.Exitf(uzicli.ExitUsage,
						"refusing to discard hold %s without confirmation: pass --yes (stdin is not a "+
							"terminal, so an interactive confirmation cannot be shown)", holdID)
				}
				ok, err := confirmDiscardHold(env, runID, holdID)
				if err != nil {
					return err
				}
				if !ok {
					if !gf.quiet {
						_, _ = fmt.Fprintln(env.Stderr, "aborted")
					}
					return nil
				}
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if err := c.DiscardRecoveryHold(cmd.Context(), runID, holdID); err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"run_id": runID, "hold_id": holdID, "discarded": true})
			}
			if !gf.quiet {
				p.Printf("discarded custody hold %s on run %s\n", holdID, sanitizeTTY(runID))
			}
			return nil
		},
	}
	discard.Flags().String("hold", "", "exact custody hold id to discard (required)")
	discard.Flags().BoolP("yes", "y", false, "skip the interactive confirmation (required for a non-interactive run)")
	return discard
}

// confirmDiscardHold prompts on stderr and reads a single line from stdin, returning true only
// for an explicit y/yes (D9). An empty line or EOF declines, so a bare Enter aborts — the safe
// default for an operation that can destroy a possible only copy. The prompt names the exact
// run and hold and states the strongest recoverability warning.
func confirmDiscardHold(env Env, runID, holdID string) (bool, error) {
	_, _ = fmt.Fprintf(env.Stderr,
		"Discard custody hold %s on run %s?\n"+
			"The worker-local source may be the ONLY copy of this work; no server archive can restore "+
			"it after discard, and discard permits worker/PVC teardown that can permanently destroy it. "+
			"[y/N]: ",
		holdID, sanitizeTTY(runID))
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, uzicli.Exitf(uzicli.ExitGeneric, "reading confirmation from stdin: %v", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
