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
	"time"

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
			"--json emits each of the run's hold DTOs plus a `captures` array listing that hold's " +
			"recovery captures (id, state, source_sha, byte_size, created_at); a capture id is what " +
			"`run export --capture` takes. A hold that outlived its deleted run lists `captures: []`.",
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
			// --json joins each hold's captures (#1417) so an agent reads the capture ids as
			// data. The human table is unchanged and makes no extra call; a run with no holds
			// has nothing to join onto, so it skips the archives read too.
			var archives []apitypes.RecoveryArchiveDTO
			if gf.json && len(forRun) > 0 {
				summary, err := c.RecoveryArchives(cmd.Context(), runID)
				switch {
				case uzicli.ExitCodeFor(err) == uzicli.ExitNotFound:
					// A released/discarded hold outlives its run (runs cascade-delete with
					// their repo; holds keep a plain run_id), so the holds list still names a
					// run whose archives read 404s. Degrade to every hold listing no captures
					// rather than failing the whole listing.
				case err != nil:
					return err
				default:
					archives = summary.Archives
				}
			}
			return renderRunRecovery(env, gf, runID, forRun, archives)
		},
	}
	return recovery
}

// recoveryHoldCapture is one capture listed under its hold in `run recovery --json` (#1417):
// the metadata an agent needs to pick a capture for `run export --capture`, nothing more.
type recoveryHoldCapture struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	SourceSha string    `json:"source_sha"`
	ByteSize  *int64    `json:"byte_size,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// recoveryHoldJSON is one hold in `run recovery --json`: the hold DTO's keys, flat and
// unchanged, plus the captures reserved under it.
type recoveryHoldJSON struct {
	apitypes.RecoveryCustodyHoldDTO
	Captures []recoveryHoldCapture `json:"captures"`
}

// holdsWithCaptures attaches each archive to the hold whose id it names, in archive order.
// Every hold gets a non-nil captures slice, so the JSON is [] and never null. An archive
// whose hold_id names no listed hold (including an empty hold_id from a server predating
// the field) is attached nowhere.
func holdsWithCaptures(holds []apitypes.RecoveryCustodyHoldDTO, archives []apitypes.RecoveryArchiveDTO) []recoveryHoldJSON {
	out := make([]recoveryHoldJSON, 0, len(holds))
	for _, h := range holds {
		caps := []recoveryHoldCapture{}
		for _, a := range archives {
			if a.HoldID != h.ID {
				continue
			}
			caps = append(caps, recoveryHoldCapture{
				ID: a.ID, State: a.State, SourceSha: a.SourceSha, ByteSize: a.ByteSize, CreatedAt: a.CreatedAt,
			})
		}
		out = append(out, recoveryHoldJSON{RecoveryCustodyHoldDTO: h, Captures: caps})
	}
	return out
}

// unattributedCaptures counts archives with an empty hold_id: those holdsWithCaptures cannot
// attach anywhere because the server predates the field.
func unattributedCaptures(archives []apitypes.RecoveryArchiveDTO) int {
	n := 0
	for _, a := range archives {
		if a.HoldID == "" {
			n++
		}
	}
	return n
}

// renderRunRecovery emits a run's custody holds. --json prints the filtered hold DTOs, each
// with its captures joined from archives; the human form prints one table row per hold with
// its exact id, generation, disposition and capture state, plus a one-line hint when a hold
// needs an owner decision.
func renderRunRecovery(env Env, gf *globalFlags, runID string, holds []apitypes.RecoveryCustodyHoldDTO,
	archives []apitypes.RecoveryArchiveDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		// Never emit a null slice: an empty result is [] so a consuming agent iterates it
		// unconditionally.
		if n := unattributedCaptures(archives); n > 0 {
			// An older server sends no hold_id, so its captures join onto no hold and the
			// JSON would read as "no captures" (#1417). Say so on stderr, leaving stdout's
			// shape unchanged (the plan degrades without failing), and name the listing that
			// shows available ids without downloading; run export needs --output and fetches a sole capture.
			_, _ = fmt.Fprintf(env.Stderr,
				"uzi: %d capture(s) carry no hold id (server predates it) and are not listed; 'uzi run get %s' lists the available ones\n",
				n, sanitizeTTY(runID))
		}
		return p.JSON(holdsWithCaptures(holds, archives))
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
