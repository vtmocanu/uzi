package main

// run_export.go is `uzi run export` (PRD #1296 M5, D7): the owner-only self-serve download
// of a run's durable-recovery archive to a local file. It fetches the run's owner-scoped
// archive summary (metadata), resolves which capture to export (never silently picking one
// when more than one is available), refuses a capture that is not in a downloadable state,
// then streams the verified bytes into an atomic, no-clobber local file. No raw bytes ever
// reach stdout; --json prints the metadata result only.

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// recoveryStateAvailable is the ONE capture lifecycle state whose bytes are downloadable
// (PRD #1296 D7). preparing/uploading are not ready; needs_action/expired/discarded cannot
// be served. Named so the gate compares against one literal.
const recoveryStateAvailable = "available"

// newRunExportCmd builds `uzi run export`.
func newRunExportCmd(env Env, gf *globalFlags) *cobra.Command {
	export := &cobra.Command{
		Use:   "export <run-id> --output <path> [--capture <id>]",
		Short: "Download a run's durable-recovery archive to a local file",
		Long: "Download the durable-recovery archive (a Git bundle of the run's original committed " +
			"history) captured when the run failed to publish, and write it to --output.\n\n" +
			"Owner-only: you can export only your own runs. When more than one archive is available " +
			"you must choose one with --capture <id>; export never silently picks an attempt. The " +
			"download's byte count and checksum are verified against the server manifest before the " +
			"file is published, and the destination is never overwritten (an existing file or symlink " +
			"there is refused). An interrupted, corrupt or expired download exits nonzero and leaves " +
			"no file at the destination.\n\n" +
			"The original history may contain secrets: review it before publishing anywhere, and if you " +
			"find a real credential, revoke/rotate it and remove it from the affected history.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			output := strings.TrimSpace(mustFlagString(cmd, "output"))
			if output == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--output <path> is required")
			}
			captureID := strings.TrimSpace(mustFlagString(cmd, "capture"))
			runID := args[0]

			// Fetch the metadata summary first — no bytes. This is also the ownership gate:
			// a foreign/absent run is a 404 (exit 4) here, before any download is attempted.
			summary, err := c.RecoveryArchives(cmd.Context(), runID)
			if err != nil {
				return err
			}
			chosen, err := selectExportCapture(runID, summary, captureID)
			if err != nil {
				return err
			}
			// A capture the server reports as available must also carry a bound byte manifest;
			// without it there is nothing to verify against, so refuse rather than write
			// unverified bytes.
			if chosen.ByteSize == nil || strings.TrimSpace(chosen.Checksum) == "" {
				return uzicli.Exitf(uzicli.ExitConflict,
					"capture %s is not ready to download yet (its verified byte manifest is not bound)", chosen.ID)
			}

			// Stream + verify + publish atomically. SafeWriteVerifiedFile refuses to clobber
			// an existing file/symlink, verifies size+checksum before the destination name
			// appears, and leaves NO file on any error (interrupt/corrupt/expired).
			if err := uzicli.SafeWriteVerifiedFile(output, *chosen.ByteSize, chosen.Checksum,
				func(w io.Writer) (int64, error) {
					return c.DownloadRecoveryArchive(cmd.Context(), runID, chosen.ID, w)
				}); err != nil {
				return err
			}

			return renderExportResult(env, gf, runID, chosen, output)
		},
	}
	export.Flags().String("output", "", "write the archive to this path (required; refuses to overwrite an existing file or symlink)")
	export.Flags().String("capture", "", "capture id to export (required when more than one archive is available)")
	return export
}

// mustFlagString reads a string flag, ignoring the never-nil error a defined flag returns.
func mustFlagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// selectExportCapture resolves which capture to export (PRD #1296 D7). It NEVER silently
// picks an attempt when more than one is available: with no --capture and >1 available
// capture, it returns a USAGE error listing every capture's id + state. With --capture it
// resolves that exact id and refuses (with the honest state) anything not available.
func selectExportCapture(runID string, summary apitypes.RecoveryArchiveSummaryDTO, captureID string) (apitypes.RecoveryArchiveDTO, error) {
	archives := summary.Archives
	if captureID != "" {
		for _, a := range archives {
			if a.ID == captureID {
				if a.State != recoveryStateAvailable {
					return apitypes.RecoveryArchiveDTO{}, uzicli.Exitf(uzicli.ExitConflict,
						"capture %s is %s, not available for download", a.ID, sanitizeTTY(a.State))
				}
				return a, nil
			}
		}
		return apitypes.RecoveryArchiveDTO{}, uzicli.Exitf(uzicli.ExitNotFound,
			"run %s has no capture %s", runID, captureID)
	}

	var available []apitypes.RecoveryArchiveDTO
	for _, a := range archives {
		if a.State == recoveryStateAvailable {
			available = append(available, a)
		}
	}
	switch len(available) {
	case 1:
		return available[0], nil
	case 0:
		return apitypes.RecoveryArchiveDTO{}, noAvailableCaptureError(runID, archives)
	default:
		// More than one available — refuse to choose. List every capture so the user can
		// pick one for --capture.
		var b strings.Builder
		fmt.Fprintf(&b, "run %s has %d recovery archives available; choose one with --capture <id>:", runID, len(available))
		for _, a := range archives {
			b.WriteString("\n  ")
			b.WriteString(captureListLine(a))
		}
		return apitypes.RecoveryArchiveDTO{}, uzicli.Exitf(uzicli.ExitUsage, "%s", b.String())
	}
}

// noAvailableCaptureError explains why nothing can be exported when --capture was omitted
// and no capture is available: either there are no captures at all (exit 4), or the ones
// that exist are in non-downloadable states — listed with their honest state (exit 5).
func noAvailableCaptureError(runID string, archives []apitypes.RecoveryArchiveDTO) error {
	if len(archives) == 0 {
		return uzicli.Exitf(uzicli.ExitNotFound, "run %s has no recovery archive to export", runID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "run %s has no downloadable recovery archive; its captures are:", runID)
	for _, a := range archives {
		b.WriteString("\n  ")
		b.WriteString(captureListLine(a))
	}
	return uzicli.Exitf(uzicli.ExitConflict, "%s", b.String())
}

// captureListLine renders one capture for an error/summary listing: id, state, the original
// committed head H (short), and the byte size when bound. The state enum and the hex SHA are
// safe, and the state is sanitized defensively; no raw bytes and no reason echo.
func captureListLine(a apitypes.RecoveryArchiveDTO) string {
	line := fmt.Sprintf("%s  %s", a.ID, sanitizeTTY(a.State))
	if sha := strings.TrimSpace(a.SourceSha); sha != "" {
		line += "  H=" + shortSHA(sha)
	}
	if a.ByteSize != nil {
		line += "  " + humanBytes(*a.ByteSize)
	}
	return line
}

// renderExportResult reports a completed export. --json emits the metadata result ONLY
// (never raw bytes); the human form prints one confirmation line. Either way, the
// secret-review warning goes to STDERR so it is seen on both and never corrupts the --json
// stdout contract.
func renderExportResult(env Env, gf *globalFlags, runID string, a apitypes.RecoveryArchiveDTO, output string) error {
	var size int64
	if a.ByteSize != nil {
		size = *a.ByteSize
	}
	_, _ = fmt.Fprintf(env.Stderr,
		"warning: this archive is the run's ORIGINAL committed history and may contain secrets — "+
			"review it before publishing anywhere; a real credential must be revoked/rotated and removed from the history\n")

	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{
			"run_id":     runID,
			"capture_id": a.ID,
			"output":     output,
			"byte_size":  size,
			"checksum":   strings.ToLower(a.Checksum),
			"source_sha": a.SourceSha,
			"verified":   true,
		})
	}
	if !gf.quiet {
		p.Printf("exported recovery archive %s to %s (%s, verified)\n", a.ID, output, humanBytes(size))
	}
	return nil
}

// shortSHA trims a git SHA to its first 12 hex chars for display.
func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

// humanBytes renders a byte count in binary units (KiB/MiB/GiB), matching the storage
// vocabulary the recovery quotas use (D4). Bytes below 1 KiB print as a raw "N B".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderRunRecoverySummary appends a metadata-only durable-recovery block to `uzi run get`'s
// human output (PRD #1296 D7). It is BEST-EFFORT: a fetch failure (an older server without
// the endpoint, a transient error) is swallowed so `run get` still succeeds — the block is
// an enrichment, not a gate. It NEVER widens raw access (metadata only) and NEVER claims an
// archive is available when it is not; it prints nothing at all unless there is genuine
// recovery content, and names each capture's honest state.
func renderRunRecoverySummary(ctx context.Context, env Env, gf *globalFlags, c uzicli.Client, run apitypes.RunDTO) apitypes.RecoveryArchiveSummaryDTO {
	// A recovery archive is captured at a finalization FAILURE, so only a terminal run can
	// carry recoverable content. Skipping the fetch for a live run keeps every non-terminal
	// `run get` a single round-trip and byte-for-byte unchanged.
	if !terminalRunStatuses[run.Status] {
		return apitypes.RecoveryArchiveSummaryDTO{}
	}
	summary, err := c.RecoveryArchives(ctx, run.ID)
	if err != nil {
		return apitypes.RecoveryArchiveSummaryDTO{}
	}
	// Return the summary so renderLandingHint can word its recovery pointer by what actually
	// exists (an available archive vs a preserved diff) without a second round-trip.
	lines := recoverySummaryLines(summary)
	if len(lines) == 0 {
		return summary
	}
	p := env.printer(gf)
	for _, l := range lines {
		p.Printf("%s\n", l)
	}
	return summary
}

// hasAvailableArchive reports whether the summary carries a capture in the downloadable
// 'available' state — the only state `uzi run export` can actually fetch.
func hasAvailableArchive(summary apitypes.RecoveryArchiveSummaryDTO) bool {
	for _, a := range summary.Archives {
		if a.State == recoveryStateAvailable {
			return true
		}
	}
	return false
}

// renderLandingHint appends the issue #1418 human-landing hint to `uzi run get`'s human output,
// printed ONLY when the run's server-derived landing_state is "needs_landing" — a failed run
// whose committed work is human-landable. It names the recovery command that actually applies,
// so it never points the operator at a command that would error:
//   - an available recovery archive exists  → `uzi run export` (its id is in the block above);
//   - otherwise the branch diff is preserved → `uzi run get --field preserved_patch`;
//   - neither is visible here (a still-preparing capture, or a transient archives-fetch miss,
//     though the server derived needs_landing) → `uzi run recovery`, which always lists state.
//
// DeriveLandingState reaches needs_landing via an available capture OR a preserved_patch, so at
// most one of the first two branches names the wrong tool; branching on what the fetched summary
// and the run row actually carry keeps the guidance correct for every needs_landing sub-case.
// Every other run — a failed run whose landing_state is "unrecoverable" or "none", and every
// non-failed run — prints nothing, so ordinary `run get` output is byte-for-byte unchanged.
// run.ID is a server UUID (safe), sanitized defensively for consistency with the sibling hint.
func renderLandingHint(env Env, gf *globalFlags, run apitypes.RunDTO, summary apitypes.RecoveryArchiveSummaryDTO) {
	if run.LandingState != landingStateNeedsLanding {
		return
	}
	p := env.printer(gf)
	id := sanitizeTTY(run.ID)
	switch {
	case hasAvailableArchive(summary):
		p.Printf("\nthis failed run's committed work is landable by hand: export the recovery archive with `uzi run export %s`\n", id)
	case run.PreservedPatch != nil && strings.TrimSpace(*run.PreservedPatch) != "":
		p.Printf("\nthis failed run's committed work is landable by hand: its diff is preserved — view it with `uzi run get %s --field preserved_patch`\n", id)
	default:
		p.Printf("\nthis failed run's committed work is landable by hand: see recovery options with `uzi run recovery %s`\n", id)
	}
}

// recoverySummaryLines is the metadata-only recovery block for `uzi run get` (PRD #1296 D7):
// a RECOVERY header with the capture count + per-state tally, then one line per available
// capture (id, source SHA, size) so the owner knows what `uzi run export --capture` can
// fetch. It returns nothing for a run with no captures and no open hold — a normal run whose
// custody was released at successful completion prints no block, so nothing false is implied.
func recoverySummaryLines(s apitypes.RecoveryArchiveSummaryDTO) []string {
	if len(s.Archives) == 0 {
		if s.HasOpenHold {
			return []string{"RECOVERY  capture pending (unpublished committed work is being preserved)"}
		}
		return nil
	}
	lines := []string{fmt.Sprintf("RECOVERY  %d archive(s): %s", len(s.Archives), stateTally(s.Counts))}
	for _, a := range s.Archives {
		if a.State == recoveryStateAvailable {
			lines = append(lines, "  "+captureListLine(a))
		}
	}
	return lines
}

// stateTally renders the closed per-state capture counts as a compact, stable list, showing
// only the non-zero states in lifecycle order.
func stateTally(c apitypes.RecoveryArchiveStateCountsDTO) string {
	var parts []string
	add := func(label string, n int) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	add("preparing", c.Preparing)
	add("uploading", c.Uploading)
	add("available", c.Available)
	add("needs action", c.NeedsAction)
	add("expired", c.Expired)
	add("discarded", c.Discarded)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
