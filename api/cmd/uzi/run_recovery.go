package main

// run_recovery.go is `uzi run recovery` and `uzi run discard` (PRD #1349 M5, D7/D9): the
// owner-side custody-hold LIST and exact hold DISCARD. `run recovery` fetches the owner-wide
// custody holds (GET /api/recovery/holds) once. With a run id it narrows them to that run
// client-side and renders each hold's exact id, generation, server-derived disposition
// (attention) and latest capture state; with none it lists every open hold across runs (all
// states under --json). It also reads each relevant run once via the existing GET run
// route to show a failed run's failure_reason. `run discard` targets ONE exact hold
// (DELETE .../recovery-holds/<hold>?confirm=discard, which the client always sends); it
// requires an interactive confirmation when --yes is absent and HARD-REFUSES when --yes is
// absent and stdin is not a TTY, so a possible only copy is never destroyed without a human
// decision. A cancelled/declined prompt performs NO mutation.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func parseRecoveryRunID(raw string) (string, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "invalid run id %q: the full run UUID is required", cellText(raw))
	}
	return id.String(), nil
}

// newRunRecoveryCmd builds `uzi run recovery [run-id] [--json]`.
func newRunRecoveryCmd(env Env, gf *globalFlags) *cobra.Command {
	recovery := &cobra.Command{
		Use:   "recovery [run-id | --user <uuid>]",
		Short: "List open custody holds across your runs or all holds for one run",
		Long: "Without a run id, show your open custody holds across all runs, oldest first, " +
			"with exact run and hold ids, disposition, archive availability, worker, age, and the " +
			"owner-wide custody summary. With a run id, show that run's retained holds: each hold's exact id, " +
			"claim generation, server-derived disposition, the latest capture state, and retained captures' recorded prerequisite pins.\n\n" +
			"Owner-only: you see only your own runs' holds, with one read-only exception. Admin run debug " +
			"(server flag UZI_ADMIN_RUN_DEBUG, a live admin's admin_ro (uza_) token, audited) lets a run id that " +
			"is not yours list that run's holds and captures, and `--user <uuid>` list another user's open holds " +
			"owner-wide; `--user` takes no run id. Those views are metadata-only: `run discard` and `run export` stay owner-only. " +
			"An `archive_ready` hold has a recovery " +
			"archive: download it with `run export`; thin archives also require external commits and destination import verification. An unguarded hold can release when the archive is " +
			"durable; guarded inventory requires verified final coverage or an explicit disposition. " +
			"Eligible guarded COMPLETED issue/mr_rework/self_improve runs may instead release after " +
			"the API proves publication of the fixed final SHA on the branch and MR. This exception " +
			"does not backfill old runs; failed/cancelled/parked runs and ci_fix/prompt/task are unchanged. " +
			"Publication releases custody, independently of capture availability or physical retirement. " +
			"A `source_only` or `needs_action` hold retains local inventory and awaits your " +
			"decision to discard it with `run discard <run-id> --hold <hold-id> --yes`. " +
			"Archive availability is independent of attention: export an available archive even for a " +
			"decision hold; it may not cover the latest work. A latest preparing/uploading capture is " +
			"not yet downloadable and does not settle custody. `source_only` " +
			"means the worker's local inventory remains in custody until a final disposition. For " +
			"`source_only` and `needs_action` holds the retained source may be the only copy, so " +
			"discarding one can destroy the work. `active` is healthy protection " +
			"of a still-running run and needs nothing.\n\n" +
			"A terminal_record_rejection of mac_failure is shown separately: " + terminalMACFailureCopy +
			". JSON also includes this fixed diagnostic as terminal_rejection; disposition and archive availability remain independent.\n\n" +
			"An additional read-only lookup of each held run shows its existing failure_reason when failed; " +
			"an unavailable run lookup leaves custody unchanged and warns on stderr.\n\n" +
			"Without a run id, --json emits the entire owner-wide aggregate and holds DTO, " +
			"including settled holds. With a run id, --json emits each of the run's hold DTOs " +
			"plus a `captures` array listing that hold's " +
			"recovery captures (id, state, source_sha, byte_size, created_at, checksum, ordered prerequisites, expiry, attempted head and optional prerequisite_closure); a capture id is what " +
			"`run export --capture` takes. A hold that outlived its deleted run lists `captures: []`. A capture whose hold " +
			"is not among the listed holds (for example after the hold was released) is not dropped: when there is at " +
			"least one, --json appends ONE final element `{\"run_id\": ..., \"unattached\": true, \"captures\": [...]}` " +
			"after the hold elements, with the same capture shape and no generation, id or state keys; the human form " +
			"prints a \"captures (no listed hold)\" table.\n\n" +
			"While the server retains the run's last published checkpoint on origin, the hold also " +
			"names where it lives: `refs/uzi-checkpoints/<checkpoint-key>`, or `refs/uzi-recovery/<run-id>` " +
			"once a newer run on the same branch superseded it, with its tip and retention state " +
			"(--json: checkpoint_ref, checkpoint_tip, checkpoint_state).",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return err
			}
			if cmd.Flags().Changed("user") && len(args) > 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "--user cannot be combined with a run id")
			}
			if len(args) == 1 {
				_, err := parseRecoveryRunID(args[0])
				return err
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("user") {
				raw, _ := cmd.Flags().GetString("user")
				id, err := uuid.Parse(raw)
				if err != nil {
					return uzicli.Exitf(uzicli.ExitUsage, "invalid --user %q: the full user UUID is required", cellText(raw))
				}
				c, err := env.client(gf)
				if err != nil {
					return err
				}
				return runRecoveryForUser(cmd.Context(), env, gf, c, id.String())
			}
			var runID string
			if len(args) == 1 {
				var err error
				runID, err = parseRecoveryRunID(args[0])
				if err != nil {
					return err
				}
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			holds, err := c.RecoveryHolds(cmd.Context())
			if err != nil {
				return err
			}
			if len(args) == 0 {
				reasons, err := recoveryFailureReasons(cmd.Context(), env, c, holds.Holds, true)
				if err != nil {
					return err
				}
				return renderOwnerRecovery(env, gf, holds, reasons, false)
			}
			return runRecoveryForRun(cmd.Context(), env, gf, c, runID, holds)
		},
	}
	recovery.Flags().String("user", "", "list another user's open custody holds (admin run debug, read-only); takes no run id")
	return recovery
}

// runRecoveryForUser is `run recovery --user <uuid>`: another user's holds under admin run debug.
// Every --user listing is treated as a read-only foreign view because the CLI does not know the
// caller's own id. A response without an owner_id naming exactly that user came from a server that
// ignored the query (it answered with the caller's own holds), so it is refused.
func runRecoveryForUser(ctx context.Context, env Env, gf *globalFlags, c uzicli.Client, userID string) error {
	holds, err := c.RecoveryHoldsScoped(ctx, userID, "")
	if err != nil {
		return hintOnForeignReadRefusal(env, err)
	}
	if holds.OwnerID == "" || !strings.EqualFold(holds.OwnerID, userID) {
		return uzicli.Exitf(uzicli.ExitGeneric, "the server did not return holds scoped to user %s; it may predate admin run debug", userID)
	}
	reasons, err := recoveryFailureReasons(ctx, env, c, holds.Holds, true)
	if err != nil {
		return err
	}
	if !gf.json {
		_, _ = fmt.Fprintln(env.Stderr, adminRunDebugReadOnlyLine)
	}
	return renderOwnerRecovery(env, gf, holds, reasons, true)
}

// runRecoveryForRun lists one run's holds and captures. own is the caller's owner-wide hold list.
// With no own hold for the run it asks the server for the run's holds (?run=), which serves
// another user's run under admin run debug; those holds are re-filtered by run id (a server that
// ignores the query answers owner-wide) and the view is read-only. Capture metadata is read even
// when no hold lists, so captures whose hold is gone still surface; that read is best-effort when
// there is no hold to join onto.
func runRecoveryForRun(ctx context.Context, env Env, gf *globalFlags, c uzicli.Client, runID string,
	own apitypes.RecoveryCustodyHoldsDTO) error {
	// The endpoint is owner-wide; narrow to this run client-side.
	forRun := holdsOfRun(own.Holds, runID)
	readOnly := false
	if len(forRun) == 0 {
		scoped, err := c.RecoveryHoldsScoped(ctx, "", runID)
		switch {
		case uzicli.ExitCodeFor(err) == uzicli.ExitNotFound:
			// Not the caller's run, or no such run; either way no holds to show. Say what a
			// foreign-run read needs, and carry on: the command still succeeds with nothing.
			_, _ = fmt.Fprintln(env.Stderr, adminRunDebugHint)
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			_, _ = fmt.Fprintf(env.Stderr, "warning: could not check admin run debug holds for run %s: %s\n", cellText(runID), cellText(err.Error()))
		default:
			forRun = holdsOfRun(scoped.Holds, runID)
			readOnly = len(forRun) > 0
		}
	}
	reasons, err := recoveryFailureReasons(ctx, env, c, forRun, false)
	if err != nil {
		return err
	}
	// --json joins each hold's captures (#1417) so an agent reads the capture ids as
	// data. Human output uses the same read for recorded pin observations.
	var archives []apitypes.RecoveryArchiveDTO
	summary, err := c.RecoveryArchives(ctx, runID)
	switch {
	case uzicli.ExitCodeFor(err) == uzicli.ExitNotFound:
		// A released/discarded hold outlives its run (runs cascade-delete with
		// their repo; holds keep a plain run_id), so the holds list still names a
		// run whose archives read 404s. Degrade to every hold listing no captures
		// rather than failing the whole listing.
	case err != nil:
		if len(forRun) > 0 && gf.json {
			return err
		}
		_, _ = fmt.Fprintln(env.Stderr, "warning: capture and prerequisite-pin metadata unavailable; protection unknown; custody remains as listed")
	default:
		archives = summary.Archives
		if summary.AdminDebugRead {
			readOnly = true
		}
	}
	if readOnly && !gf.json {
		_, _ = fmt.Fprintln(env.Stderr, adminRunDebugReadOnlyLine)
	}
	return renderRunRecovery(env, gf, runID, forRun, archives, reasons, readOnly)
}

// holdsOfRun keeps the holds that belong to runID.
func holdsOfRun(holds []apitypes.RecoveryCustodyHoldDTO, runID string) []apitypes.RecoveryCustodyHoldDTO {
	var out []apitypes.RecoveryCustodyHoldDTO
	for _, h := range holds {
		if h.RunID == runID {
			out = append(out, h)
		}
	}
	return out
}

// recoveryFailureReasons makes at most one read per distinct relevant run in the
// already-fetched hold list, with no retries. Each read uses the command context;
// one unavailable read does not block siblings or change custody. All unavailable
// reads share one static warning, including 404s: absence is not verified clearance.
func recoveryFailureReasons(ctx context.Context, env Env, c uzicli.Client,
	holds []apitypes.RecoveryCustodyHoldDTO, openOnly bool) (map[string]string, error) {
	reasons := make(map[string]string)
	seen := make(map[string]bool)
	unavailable := 0
	for _, h := range holds {
		if openOnly && h.State != "open" || seen[h.RunID] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seen[h.RunID] = true
		run, err := c.GetRun(ctx, h.RunID)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if err != nil {
			unavailable++
			continue
		}
		if run.Status == "failed" && run.FailureReason != nil && strings.TrimSpace(*run.FailureReason) != "" {
			reasons[h.RunID] = *run.FailureReason
		}
	}
	if unavailable > 0 {
		_, _ = fmt.Fprintf(env.Stderr, "warning: failure reason unavailable for %d held run(s); custody remains as listed, failure status is unverified\n", unavailable)
	}
	return reasons, nil
}

// printRecoveryFailureReasons prints one diagnostic per run even when several holds
// share it. Human output folds untrusted fields with cellText; JSON retains DTO values.
func printRecoveryFailureReasons(p *uzicli.Printer, holds []apitypes.RecoveryCustodyHoldDTO, reasons map[string]string) {
	seen := make(map[string]bool)
	for _, h := range holds {
		if reason := cellText(reasons[h.RunID]); reason != "" && !seen[h.RunID] {
			p.Printf("run %s: %s\n", cellText(h.RunID), reason)
			seen[h.RunID] = true
		}
	}
}

// renderOwnerRecovery shows only open holds in the human view. The JSON form preserves
// the server's entire owner response, including settled holds and server order.
//
// readOnly marks another user's holds (admin run debug): the export and discard hints are
// suppressed because both stay owner-only.
func renderOwnerRecovery(env Env, gf *globalFlags, dto apitypes.RecoveryCustodyHoldsDTO, reasons map[string]string, readOnly bool) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		if dto.Holds == nil {
			dto.Holds = []apitypes.RecoveryCustodyHoldDTO{}
		}
		rows := make([]recoveryOwnerHoldJSON, 0, len(dto.Holds))
		for _, h := range dto.Holds {
			rows = append(rows, recoveryOwnerHoldJSON{RecoveryCustodyHoldDTO: h, TerminalRejection: terminalRejectionCopy(h), FailureReason: reasons[h.RunID]})
		}
		return p.JSON(struct {
			// Embed the response to preserve aggregate keys while enriching hold rows.
			apitypes.RecoveryCustodyHoldsDTO
			Holds []recoveryOwnerHoldJSON `json:"holds"`
		}{RecoveryCustodyHoldsDTO: dto, Holds: rows})
	}
	open := make([]apitypes.RecoveryCustodyHoldDTO, 0, len(dto.Holds))
	for _, h := range dto.Holds {
		if h.State == "open" {
			open = append(open, h)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i], open[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Generation < b.Generation
	})
	rows := make([][]string, 0, len(open))
	decisionNeeded, exportable := 0, 0
	for _, h := range open {
		if h.Attention == "source_only" || h.Attention == "needs_action" {
			decisionNeeded++
		}
		if h.HasAvailableCapture {
			exportable++
		}
		rows = append(rows, []string{
			cellText(h.RunID), cellText(h.ID), strconv.FormatInt(h.Generation, 10),
			cellText(h.Attention), admissionCell(h), strconv.FormatBool(h.HasAvailableCapture),
			cellText(recoveryWorkerLabel(h)), relAge(h.CreatedAt),
		})
	}
	if len(rows) > 0 {
		if err := p.Table([]string{"RUN ID", "HOLD ID", "GEN", "DISPOSITION", "ADMISSION", "ARCHIVE", "WORKER", "AGE"}, rows); err != nil {
			return err
		}
	} else if !gf.quiet {
		p.Printf("no open custody holds\n")
	}
	for _, h := range open {
		if line := completedPublicationReasonLine(h); line != "" {
			p.Printf("%s\n", line)
		}
		if copy := terminalRejectionCopy(h); copy != "" {
			p.Printf("run %s hold %s gen %d: %s\n", cellText(h.RunID), cellText(h.ID), h.Generation, copy)
		}
	}
	printRecoveryFailureReasons(p, open, reasons)
	a := dto.Aggregate
	p.Printf("open_holds: %d  admission_units (admission_counted_holds): %d  custody_hold_limit: %d  decision_needed: %d  blocked_runs: %d\n",
		a.OpenHolds, a.AdmissionCountedHolds, a.CustodyHoldLimit, a.DecisionNeeded, a.BlockedRuns)
	if !gf.quiet && !readOnly {
		first := true
		for _, h := range open {
			if line := sourceOnlyLine(h); line != "" {
				if first {
					p.Printf("\n")
					first = false
				}
				p.Printf("run %s %s\n", cellText(h.RunID), line)
			}
		}
		printRecoveryHint(p, exportable, decisionNeeded, "<run-id>")
	}
	return nil
}

// completedPublicationReasonLine bounds and sanitizes the API's refusal diagnostic.
func completedPublicationReasonLine(h apitypes.RecoveryCustodyHoldDTO) string {
	if reason := cellText(h.CompletedPublicationReason); reason != "" {
		return fmt.Sprintf("run %s hold %s gen %d: completed publication refused: %s",
			cellText(h.RunID), cellText(h.ID), h.Generation, reason)
	}
	return ""
}

// completedPublicationLine keeps publication proof separate from capture and retirement.
func completedPublicationLine(h apitypes.RecoveryCustodyHoldDTO) string {
	r := h.CompletedPublicationReceipt
	if r == nil {
		return ""
	}
	custody := "custody release not confirmed"
	if h.State == "released" {
		custody = "custody released"
	}
	mr := "-"
	if r.MRIID != nil {
		mr = strconv.FormatInt(*r.MRIID, 10)
	}
	return fmt.Sprintf("hold %s completed publication: fixed final SHA %s; observed branch head %s; branch %s; MR %s; %s; archive available: %t; physical retirement not confirmed",
		cellText(h.ID), cellText(r.FinalHead), cellText(r.ObservedBranchHead), cellText(r.Branch), mr, custody, h.HasAvailableCapture)
}

const terminalMACFailureCopy = "terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody"

func terminalRejectionCopy(h apitypes.RecoveryCustodyHoldDTO) string {
	if h.TerminalRecordRejection == "mac_failure" {
		return terminalMACFailureCopy
	}
	return ""
}

type recoveryOwnerHoldJSON struct {
	apitypes.RecoveryCustodyHoldDTO
	TerminalRejection string `json:"terminal_rejection,omitempty"`
	FailureReason     string `json:"failure_reason,omitempty"`
}

// sourceOnlyLine explains retained local inventory, including a guarded hold with a
// downloadable archive and unresolved custody (incomplete coverage or external dependencies).
// Attention is server-authoritative; id and worker name pass cellText like the table cells.
func sourceOnlyLine(h apitypes.RecoveryCustodyHoldDTO) string {
	if h.Attention != "source_only" {
		return ""
	}
	if h.HasAvailableCapture && !h.InventoryGuarded {
		line := fmt.Sprintf("hold %s: recovery archive available to export; it may not cover the latest worker-local work; custody of worker %s is retained for your decision",
			cellText(h.ID), cellText(recoveryWorkerLabel(h)))
		if h.CaptureState == "preparing" || h.CaptureState == "uploading" {
			line += fmt.Sprintf("; latest capture is %s (coverage not yet verified)", cellText(h.CaptureState))
		}
		return line
	}
	if h.InventoryGuarded && h.HasAvailableCapture {
		line := fmt.Sprintf("hold %s: recovery archive available; custody of worker %s remains unresolved and its local source is retained",
			cellText(h.ID), cellText(recoveryWorkerLabel(h)))
		if h.CaptureState == "preparing" || h.CaptureState == "uploading" {
			line += fmt.Sprintf("; latest capture is %s (coverage not yet verified)", cellText(h.CaptureState))
		}
		return line
	}
	if h.CaptureState == "preparing" || h.CaptureState == "uploading" {
		return fmt.Sprintf("hold %s: no recovery archive available to export yet; latest capture is %s (coverage not yet verified); custody of worker %s's local source is retained for your decision (it may be the only copy)",
			cellText(h.ID), cellText(h.CaptureState), cellText(recoveryWorkerLabel(h)))
	}
	return fmt.Sprintf("hold %s: no recovery archive; custody of worker %s's local source is retained (export unavailable; it may be the only copy)",
		cellText(h.ID), cellText(recoveryWorkerLabel(h)))
}

// printRecoveryHint prints the next-step hint. `uzi run export` is offered only for holds
// with an available archive (`uzi run export` fails for a hold without one); discard is
// offered for every hold awaiting a decision. runArg is already terminal-safe.
func printRecoveryHint(p *uzicli.Printer, exportable, decisionNeeded int, runArg string) {
	if exportable == 0 && decisionNeeded == 0 {
		return
	}
	p.Printf("\n")
	if exportable > 0 {
		p.Printf("%d hold(s) have a recovery archive: download with `uzi run export`; thin archives also need prerequisite commits and verified import\n", exportable)
	}
	if decisionNeeded > 0 {
		p.Printf("%d hold(s) await a decision: discard with `uzi run discard %s --hold <hold-id> --yes`\n",
			decisionNeeded, runArg)
	}
}

// recoveryHoldCapture is one capture listed under its hold in `run recovery --json` (#1417):
// the metadata an agent needs to pick a capture for `run export --capture`, nothing more.
type recoveryHoldCapture struct {
	ID                  string                                   `json:"id"`
	State               string                                   `json:"state"`
	SourceSha           string                                   `json:"source_sha"`
	ByteSize            *int64                                   `json:"byte_size,omitempty"`
	CreatedAt           time.Time                                `json:"created_at"`
	Checksum            string                                   `json:"checksum,omitempty"`
	PrerequisiteShas    []string                                 `json:"prerequisite_shas,omitempty"`
	ExpiresAt           *time.Time                               `json:"expires_at,omitempty"`
	AttemptedHeadSha    *string                                  `json:"attempted_head_sha,omitempty"`
	PrerequisiteClosure *apitypes.RecoveryPrerequisiteClosureDTO `json:"prerequisite_closure,omitempty"`
}

// recoveryHoldJSON is one hold in `run recovery --json`: the hold DTO's keys, flat and
// unchanged, plus the captures reserved under it.
type recoveryHoldJSON struct {
	recoveryOwnerHoldJSON
	Captures []recoveryHoldCapture `json:"captures"`
}

// captureJSONOf maps one archive to the capture shape shared by hold elements and the
// unattached element of `run recovery <run-id> --json`.
func captureJSONOf(a apitypes.RecoveryArchiveDTO) recoveryHoldCapture {
	return recoveryHoldCapture{
		ID: a.ID, State: a.State, SourceSha: a.SourceSha, ByteSize: a.ByteSize, CreatedAt: a.CreatedAt,
		Checksum: a.Checksum, PrerequisiteShas: a.PrerequisiteShas, ExpiresAt: a.ExpiresAt,
		AttemptedHeadSha: a.AttemptedHeadSha, PrerequisiteClosure: a.PrerequisiteClosure,
	}
}

// recoveryUnattachedJSON is the single trailing element of `run recovery <run-id> --json` that
// carries captures whose hold is not among the listed holds. It has no generation, id or state:
// it is not a hold.
type recoveryUnattachedJSON struct {
	RunID      string                `json:"run_id"`
	Unattached bool                  `json:"unattached"`
	Captures   []recoveryHoldCapture `json:"captures"`
}

// unattachedArchives returns the archives that name a hold id (non-empty) matching none of
// the listed holds, in archive order. An empty hold_id (a server predating the field) is not
// listed: unattributedCaptures warns about those instead.
func unattachedArchives(holds []apitypes.RecoveryCustodyHoldDTO, archives []apitypes.RecoveryArchiveDTO) []apitypes.RecoveryArchiveDTO {
	listed := make(map[string]bool, len(holds))
	for _, h := range holds {
		listed[h.ID] = true
	}
	var out []apitypes.RecoveryArchiveDTO
	for _, a := range archives {
		if a.HoldID != "" && !listed[a.HoldID] {
			out = append(out, a)
		}
	}
	return out
}

// holdsWithCaptures attaches each archive to the hold whose id it names, in archive order.
// Every hold gets a non-nil captures slice, so the JSON is [] and never null. An archive
// whose hold_id names no listed hold (including an empty hold_id from a server predating
// the field) is attached nowhere.
func holdsWithCaptures(holds []apitypes.RecoveryCustodyHoldDTO, archives []apitypes.RecoveryArchiveDTO, reasons map[string]string) []recoveryHoldJSON {
	out := make([]recoveryHoldJSON, 0, len(holds))
	for _, h := range holds {
		caps := []recoveryHoldCapture{}
		for _, a := range archives {
			if a.HoldID != h.ID {
				continue
			}
			caps = append(caps, captureJSONOf(a))
		}
		out = append(out, recoveryHoldJSON{recoveryOwnerHoldJSON: recoveryOwnerHoldJSON{RecoveryCustodyHoldDTO: h, TerminalRejection: terminalRejectionCopy(h), FailureReason: reasons[h.RunID]}, Captures: caps})
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
//
// readOnly marks a view of another user's run (admin run debug): the export and discard hints
// are suppressed because both stay owner-only.
func renderRunRecovery(env Env, gf *globalFlags, runID string, holds []apitypes.RecoveryCustodyHoldDTO,
	archives []apitypes.RecoveryArchiveDTO, reasons map[string]string, readOnly bool) error {
	p := env.printer(gf)
	unattached := unattachedArchives(holds, archives)
	if n := unattributedCaptures(archives); n > 0 {
		// An older server sends no hold_id, so its captures join onto no hold.
		// Warn in human and JSON modes, including quiet, and name the metadata-only
		// listing; run export needs --output and fetches a sole capture.
		_, _ = fmt.Fprintf(env.Stderr,
			"uzi: %d capture(s) carry no hold id (server predates it) and are not listed; 'uzi run get %s' lists the available ones\n",
			n, cellText(runID))
	}
	if p.Format == uzicli.FormatJSON {
		// Never emit a null slice: an empty result is [] so a consuming agent iterates it.
		out := make([]any, 0, len(holds)+1)
		for _, h := range holdsWithCaptures(holds, archives, reasons) {
			out = append(out, h)
		}
		if len(unattached) > 0 {
			el := recoveryUnattachedJSON{RunID: runID, Unattached: true, Captures: make([]recoveryHoldCapture, 0, len(unattached))}
			for _, a := range unattached {
				el.Captures = append(el.Captures, captureJSONOf(a))
			}
			out = append(out, el)
		}
		return p.JSON(out)
	}
	if len(holds) == 0 {
		if !gf.quiet {
			p.Printf("run %s has no retained custody holds\n", cellText(runID))
		}
		return printUnattachedCaptures(p, gf, unattached)
	}
	rows := make([][]string, 0, len(holds))
	decisionNeeded, exportable := 0, 0
	for _, h := range holds {
		if h.Attention == "source_only" || h.Attention == "needs_action" {
			decisionNeeded++
		}
		if h.State == "open" && h.HasAvailableCapture {
			exportable++
		}
		rows = append(rows, []string{
			h.ID,
			fmt.Sprintf("%d", h.Generation),
			sanitizeTTY(h.Attention),
			admissionCell(h),
			sanitizeTTY(h.State),
			captureCell(h.CaptureState),
			cellText(recoveryWorkerLabel(h)),
		})
	}
	if err := p.Table([]string{"HOLD ID", "GEN", "DISPOSITION", "ADMISSION", "STATE", "CAPTURE", "WORKER"}, rows); err != nil {
		return err
	}
	for _, h := range holds {
		if !gf.quiet {
			labelled := false
			for _, a := range archives {
				if a.HoldID == "" || a.HoldID != h.ID {
					continue
				}
				if !labelled {
					p.Printf("hold %s captures:\n", cellText(h.ID))
					labelled = true
				}
				for _, line := range captureDetailLines(a) {
					p.Printf("%s\n", line)
				}
			}
		}
		if line := completedPublicationReasonLine(h); line != "" {
			p.Printf("%s\n", line)
		}
		if line := completedPublicationLine(h); line != "" {
			p.Printf("%s\n", line)
		}
		if copy := terminalRejectionCopy(h); copy != "" {
			p.Printf("run %s hold %s gen %d: %s\n", cellText(h.RunID), cellText(h.ID), h.Generation, copy)
		}
		if line := checkpointLine(h); line != "" {
			p.Printf("%s\n", line)
		}
		if line := heldReceiptLine(h); line != "" {
			p.Printf("%s\n", line)
		}
	}
	printRecoveryFailureReasons(p, holds, reasons)
	if err := printUnattachedCaptures(p, gf, unattached); err != nil {
		return err
	}
	if !gf.quiet && !readOnly {
		for _, h := range holds {
			if line := sourceOnlyLine(h); line != "" {
				p.Printf("%s\n", line)
			}
		}
		printRecoveryHint(p, exportable, decisionNeeded, cellText(runID))
	}
	return nil
}

// printUnattachedCaptures prints the captures whose hold is not among the listed holds as a
// table, or nothing when there are none. Every cell is server text and passes cellText.
func printUnattachedCaptures(p *uzicli.Printer, gf *globalFlags, unattached []apitypes.RecoveryArchiveDTO) error {
	if len(unattached) == 0 {
		return nil
	}
	if !gf.quiet {
		p.Printf("\ncaptures (no listed hold)\n")
	}
	rows := make([][]string, 0, len(unattached))
	for _, a := range unattached {
		size, expires := "-", "-"
		if a.ByteSize != nil {
			size = humanBytes(*a.ByteSize)
		}
		if a.ExpiresAt != nil {
			expires = a.ExpiresAt.UTC().Format(time.RFC3339)
		}
		sha := cellText(shortSHA(strings.TrimSpace(a.SourceSha)))
		if sha == "" {
			sha = "-"
		}
		rows = append(rows, []string{
			cellText(a.ID), cellText(a.State), sha, size, a.CreatedAt.UTC().Format(time.RFC3339), expires,
		})
	}
	return p.Table([]string{"CAPTURE ID", "STATE", "SOURCE", "SIZE", "CREATED", "EXPIRES"}, rows)
}

// checkpointLine renders where a hold's retained published checkpoint lives on origin (PRD
// #1810): `hold <id> checkpoint: <ref> @ <12-char tip> (<state>)`, or "" when the hold carries
// no checkpoint ref. Every field is server-supplied, so each passes cellText (control stripping
// plus newline/tab folding) before it reaches the terminal; the tip is cut by rune after
// sanitizing so a hostile value cannot be split into invalid UTF-8.
func checkpointLine(h apitypes.RecoveryCustodyHoldDTO) string {
	ref := cellText(h.CheckpointRef)
	if ref == "" {
		return ""
	}
	line := fmt.Sprintf("hold %s checkpoint: %s", cellText(h.ID), ref)
	if tip := []rune(cellText(h.CheckpointTip)); len(tip) > 0 {
		if len(tip) > 12 {
			tip = tip[:12]
		}
		line += " @ " + string(tip)
	}
	if state := cellText(h.CheckpointState); state != "" {
		line += " (" + state + ")"
	}
	return line
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
			"Discarding a run's last open hold also deletes its retained checkpoint ref on the forge; " +
			"fetch that ref first if you need it.\n\n" +
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
			"Discarding a run's last open hold also deletes its retained checkpoint ref on the forge. "+
			"[y/N]: ",
		holdID, sanitizeTTY(runID))
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, uzicli.Exitf(uzicli.ExitGeneric, "reading confirmation from stdin: %v", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// admissionCell renders the server-derived custody admission membership of a hold (issue #2970):
// "counted (<reason>)" or "counted", "not counted (<reason>)" or "not counted" (a hold that is a
// non-representative member of a same-run group reports "same_run_group"), or "-" when the server
// sent none (a non-open hold, or an older server), which is unknown and never "not counted". The
// reason is server text of an open vocabulary, so it always renders through cellText.
func admissionCell(h apitypes.RecoveryCustodyHoldDTO) string {
	switch {
	case h.CountsTowardAdmission == nil:
		return "-"
	case !*h.CountsTowardAdmission:
		if h.AdmissionReason == "" {
			return "not counted"
		}
		return "not counted (" + cellText(h.AdmissionReason) + ")"
	case h.AdmissionReason == "":
		return "counted"
	default:
		return "counted (" + cellText(h.AdmissionReason) + ")"
	}
}
