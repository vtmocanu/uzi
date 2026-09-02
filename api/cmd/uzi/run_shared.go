package main

// run_shared.go holds small helpers shared across the `uzi run` verbs (PRD #1009 M1):
// seeded-plan file reading, message resolution, and the mr-rework flag wiring.

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// maxPlanReadBytes bounds a seeded plan read from STDIN (PRD #209 M3) so a hostile
// pipe cannot make the CLI allocate without bound. It sits ABOVE the server's
// create-time cap (workersvc.MaxSeededPlanBytes, 256 KiB) on purpose: an over-cap plan
// is then read whole and rejected by the server's 422, never silently truncated here
// into a shorter plan that would look valid.
const maxPlanReadBytes = 1 << 20 // 1 MiB

// readPlanFile reads a seeded plan (PRD #209 M3) from a file, or from STDIN when the
// path is "-". The file is the user's own named local input, so it is read whole; stdin
// is bounded (maxPlanReadBytes), like resolveMessage, because a pipe is the one source
// whose size the caller does not control.
func readPlanFile(env Env, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage, "--plan-file needs a path (use '-' to read the plan from stdin)")
	}
	if path == "-" {
		if env.Stdin == nil {
			return "", uzicli.Exitf(uzicli.ExitUsage, "no stdin to read the plan from")
		}
		b, err := io.ReadAll(io.LimitReader(env.Stdin, maxPlanReadBytes))
		if err != nil {
			return "", uzicli.Exitf(uzicli.ExitGeneric, "reading plan from stdin: %v", err)
		}
		return string(b), nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the operator's own --plan-file argument to their local CLI; reading the file they named is the intended behaviour, not untrusted inclusion.
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "cannot read plan file: %v", err)
	}
	return string(b), nil
}

// resolveMessage returns the -m flag value when set, else a message piped on stdin
// (non-TTY only, mirroring `uzi auth token`), capped so a hostile pipe cannot make
// the CLI allocate without bound.
func resolveMessage(env Env, flagVal string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal
	}
	if env.Stdin != nil && !env.StdinTTY {
		b, _ := io.ReadAll(io.LimitReader(env.Stdin, 1<<20))
		return strings.TrimSpace(string(b))
	}
	return ""
}

// mrReworkFlag resolves `--mr-rework` into the same TRI-STATE the create endpoint takes
// for the per-run MR-rework override (PRD #841 M3): nil omits the key so the server
// stamps the run by inheriting the owner default, &true opts this run's MR into
// auto-rework, &false opts it explicitly out.
//
// It mirrors waitOnLimitFlag exactly, and for the same reason: `Bool("mr-rework", false,
// …)` makes GetBool return false when the flag is absent, indistinguishable from
// `--mr-rework=false`. Passing that straight through would send `"mr_rework_enabled":
// false` on EVERY CLI-created run and silently override the owner's default. Changed() is
// what separates "the user said false" from "the user said nothing" — pflag sets it for
// `--mr-rework` and `--mr-rework=false` alike and leaves it false when the flag is absent.
// (`--mr-rework false` with a SPACE reads false as a positional, a loud usage error under
// create's cobra.NoArgs, so it needs no guard of its own — same as wait-on-limit.)
func mrReworkFlag(cmd *cobra.Command) *bool {
	if !cmd.Flags().Changed("mr-rework") {
		return nil
	}
	v, err := cmd.Flags().GetBool("mr-rework")
	if err != nil {
		return nil
	}
	return &v
}
