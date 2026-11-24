package main

// fresh_resume.go holds `uzi run fresh-resume` (issue #4628): the owner resumes a held run on a
// separate persistent hosted worker from durable evidence. It reads the preview, shows what a start
// would lose and cost, confirms, and starts with ONE request id per confirm action.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// freshResumeCutoffText is the cutoff text the owner confirms against. The web panel carries the same
// substance (web/src/lib/freshResumeText.ts); the server's Notes (freshNotes in
// api/internal/workersvc/fresh_resume_select.go) carry it too for other consumers, and a test pins
// TestFreshResumeCutoffMatchesServerNotes checks each CLI line appears in the server copy.
var freshResumeCutoffText = []string{
	"Edits made after the saved evidence may be missing from the resumed run.",
	"The original worker's source and custody are kept; starting a fresh resume does not delete them.",
	"Unsaved edits may be lost if the original worker's volume is gone.",
	"Each attempt keeps its own custody hold, and a healthy seeded run counts one extra admission unit until its new hold is released or the run ends.",
	"While the original worker and the new worker both hold custody, only a third eligible hosted worker can take a repeat fresh resume. After seeding, the original worker never takes this run again, so an owner with only two workers whose new worker is lost must add a worker or cancel the run.",
}

// freshResumeNoEvidenceGenerationText names a generation that saved nothing.
const freshResumeNoEvidenceGenerationText = "No saved evidence exists for generation %s; any edits made in it are not included."

// freshResumeStatusText explains each preview status that is not "available", and the 409 reasons a
// start can add. Keyed by the server's closed reason/status vocabulary; an unknown value renders the
// generic line and never the raw string.
var freshResumeStatusText = map[string]string{
	apitypes.FreshResumeStatusDisabled:         "Fresh resume is not enabled on this instance.",
	apitypes.FreshResumeStatusNoDestination:    "No other persistent hosted worker is online with fresh resume support (no_destination). Add or start a worker, then try again.",
	apitypes.FreshResumeStatusCustodyLimit:     "Starting would take you past your custody hold limit (custody_limit). Release or discard a hold you no longer need, then try again.",
	apitypes.FreshResumeStatusNoEvidence:       "This run has no saved evidence to resume from (no_evidence). The original worker's source is kept; nothing was started.",
	apitypes.FreshResumeStatusExpired:          "The saved evidence has expired or is about to (expired). Nothing was started.",
	apitypes.FreshResumeStatusUnbound:          "The run's checkpoint cannot be tied to a generation, so it might hold newer work than the evidence (unbound). Nothing was started.",
	apitypes.FreshResumeStatusPrereqUnproven:   "The saved evidence's base commits could not be proven on the forge (prereq_unproven). Nothing was started.",
	apitypes.FreshResumeStatusAmbiguous:        "Several saved copies of this run's work exist and none contains the others (ambiguous). Nothing was started.",
	apitypes.FreshResumeStatusInProgress:       "A fresh resume of this run is already in progress (in_progress). Wait for it to finish.",
	apitypes.FreshResumeStatusIneligible:       "This run is not held in a state a fresh resume can start from (ineligible).",
	apitypes.FreshResumeReasonStaleSelection:   "The selection changed since the preview (stale_selection). Run `uzi run fresh-resume` again to review the new preview.",
	apitypes.FreshResumeReasonRequestCompleted: "This request already finished (request_completed). Run `uzi run fresh-resume` again for a new preview.",
	"fresh_resume_in_progress":                 "A fresh resume of this run is already in progress (fresh_resume_in_progress). Wait for it to finish.",
}

const freshResumeGenericRefusal = "Fresh resume was refused for a reason this CLI does not know; nothing was started."

// freshResumeHint is appended to a held run's recovery line.
func freshResumeHint(runID string) string {
	return "Fresh resume on a separate hosted worker: uzi run fresh-resume " + cellText(runID)
}

// freshResumeStartRetries bounds the same-request-id retries of a start whose transport failed.
const freshResumeStartRetries = 2

// freshResumeMaxBackoff bounds the wait before a start retry; freshResumeBaseBackoff is the wait when
// the server gave no Retry-After.
const (
	freshResumeBaseBackoff = 500 * time.Millisecond
	freshResumeMaxBackoff  = 5 * time.Second
)

// freshResumeSleep waits d or until ctx ends; tests replace it.
var freshResumeSleep = func(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// freshResumeBackoff is the bounded wait before retry number attempt (0-based): the server's
// Retry-After when it sent one, else a short doubling base, never above freshResumeMaxBackoff.
func freshResumeBackoff(ee *uzicli.ExitError, attempt int) time.Duration {
	d := freshResumeBaseBackoff << uint(attempt)
	if ee != nil && ee.RetryAfter > 0 {
		d = ee.RetryAfter
	}
	if d > freshResumeMaxBackoff {
		d = freshResumeMaxBackoff
	}
	return d
}

func newRunFreshResumeCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fresh-resume <run-id> [--yes] [--request-id <uuid> [--selection-token <token>]]",
		Short: "Resume a held run on a separate hosted worker from durable evidence",
		Long: "Resume a run held at a worker-exhaustion recovery wait on a separate persistent hosted " +
			"worker, seeded from the run's saved evidence (issue #4628). The command shows the evidence " +
			"(generation, kind, commit, and any generations that saved nothing) and what a fresh resume " +
			"may lose or cost, then asks you to confirm; pass --yes to skip the prompt. Without a " +
			"terminal and without --yes it refuses and starts nothing.\n\n" +
			"One confirm uses one request id. A retry after a lost reply reuses it automatically. To retry " +
			"a separate invocation of the SAME confirm, pass both values the first one printed: " +
			"--request-id and --selection-token; the start is then sent directly, without a new preview. " +
			"--request-id alone still goes through the preview. Exit 5 on a refusal (stale selection, " +
			"request already completed, in progress, or a status that says why a start is not possible).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := args[0]
			yes, _ := cmd.Flags().GetBool("yes")
			requestID := strings.TrimSpace(mustFlagString(cmd, "request-id"))
			token := strings.TrimSpace(mustFlagString(cmd, "selection-token"))
			if requestID == "" {
				if token != "" {
					return uzicli.Exitf(uzicli.ExitUsage, "--selection-token is only used together with --request-id")
				}
				requestID = uuid.NewString()
			} else if u, err := uuid.Parse(requestID); err != nil || u.String() != requestID {
				return uzicli.Exitf(uzicli.ExitUsage, "--request-id must be a canonical lowercase UUID")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			// The cutoff text is what the owner confirms against: print it whenever the prompt can be
			// shown, whatever -q or -o json say; with --yes only for human, non-quiet output.
			showText := !yes || (p.Format != uzicli.FormatJSON && !gf.quiet)
			if token != "" {
				// --request-id with its token: post directly. The preview reports in_progress while that
				// transfer is active, so gating on it would keep the replay from reaching the server.
				if showText {
					_, _ = fmt.Fprint(env.Stderr, renderFreshResumeCutoff(apitypes.FreshResumePreviewDTO{}))
				}
			} else {
				prev, err := c.FreshResumePreview(cmd.Context(), runID)
				if err != nil {
					return err
				}
				if prev.Status != apitypes.FreshResumeStatusAvailable || prev.SelectionToken == "" {
					if p.Format == uzicli.FormatJSON {
						if err := p.JSON(prev); err != nil {
							return err
						}
					}
					return uzicli.Exitf(uzicli.ExitConflict, "%s", freshResumeStatusMessage(prev.Status))
				}
				token = prev.SelectionToken
				if showText {
					_, _ = fmt.Fprint(env.Stderr, renderFreshResumePreview(runID, prev))
				}
			}
			if !yes {
				if !env.StdinTTY {
					return uzicli.Exitf(uzicli.ExitUsage, "refusing to start a fresh resume of run %s without confirmation: pass --yes (stdin is not a terminal, so an interactive confirmation cannot be shown)", cellText(runID))
				}
				ok, err := confirmFreshResume(env)
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
			req := apitypes.FreshResumeStartRequest{SelectionToken: token, RequestID: requestID}
			var out apitypes.FreshResumeStartDTO
			for attempt := 0; ; attempt++ {
				out, err = c.StartFreshResume(cmd.Context(), runID, req)
				if err == nil {
					break
				}
				// A transport failure may have lost a reply that did start the transfer: retry the SAME
				// request id (the server answers a replay with the existing row), bounded, after a short
				// wait that honors the server's Retry-After.
				var ee *uzicli.ExitError
				if attempt < freshResumeStartRetries && errors.As(err, &ee) && ee.Code == uzicli.ExitUnreachable && ee.Reason == "" && cmd.Context().Err() == nil {
					freshResumeSleep(cmd.Context(), freshResumeBackoff(ee, attempt))
					continue
				}
				return freshResumeStartError(err, requestID, token)
			}
			if p.Format == uzicli.FormatJSON {
				return p.JSON(out)
			}
			if !gf.quiet {
				verb := "started"
				if out.Replay {
					verb = "already started by this request"
				}
				p.Printf("fresh resume %s for run %s (transfer %s, state %s, request id %s)\n",
					verb, cellText(runID), cellText(out.ID), cellText(out.State), cellText(out.RequestID))
			}
			return nil
		},
	}
	cmd.Flags().BoolP("yes", "y", false, "skip the interactive confirmation (required for a non-interactive run)")
	cmd.Flags().String("request-id", "", "reuse the request id of an earlier invocation of the same confirm (default: a new UUID)")
	cmd.Flags().String("selection-token", "", "with --request-id: the selection token the earlier invocation printed; posts the start directly")
	return cmd
}

func freshResumeStatusMessage(status string) string {
	if t, ok := freshResumeStatusText[status]; ok {
		return t
	}
	return freshResumeGenericRefusal
}

// freshResumeStartError renders a start refusal: a typed 409 by its reason, anything else as is. A
// transport error names the request id and selection token so the owner can retry the same confirm.
func freshResumeStartError(err error, requestID, token string) error {
	var ee *uzicli.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	if ee.Code == uzicli.ExitConflict {
		return uzicli.Exitf(uzicli.ExitConflict, "%s", freshResumeStatusMessage(ee.Reason))
	}
	if ee.Code == uzicli.ExitUnreachable {
		return uzicli.Exitf(ee.Code, "%v; the start may or may not have been recorded: retry with --request-id %s --selection-token %s", ee.Err, cellText(requestID), cellText(token))
	}
	return err
}

// renderFreshResumePreview renders the evidence facts and the cutoff text. Every server-supplied field
// goes through cellText: the sha, ref and ids are untrusted text on a terminal.
func renderFreshResumePreview(runID string, d apitypes.FreshResumePreviewDTO) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fresh resume of run %s\n", cellText(runID))
	fmt.Fprintf(&b, "  evidence generation (Ge): %d (latest claim generation %d)\n", d.EvidenceGeneration, d.ClaimGeneration)
	fmt.Fprintf(&b, "  evidence kind:            %s\n", cellText(d.Kind))
	fmt.Fprintf(&b, "  commit:                   %s\n", cellText(d.Sha))
	if d.CheckpointRef != "" {
		fmt.Fprintf(&b, "  checkpoint ref:           %s\n", cellText(d.CheckpointRef))
	}
	if d.CaptureID != "" {
		fmt.Fprintf(&b, "  capture:                  %s\n", cellText(d.CaptureID))
	}
	b.WriteString("\n")
	b.WriteString(renderFreshResumeCutoff(d))
	return b.String()
}

// renderFreshResumeCutoff renders the cutoff text and any uncovered generations of d.
func renderFreshResumeCutoff(d apitypes.FreshResumePreviewDTO) string {
	var b strings.Builder
	for _, t := range freshResumeCutoffText {
		b.WriteString("  - " + t + "\n")
	}
	if len(d.UncoveredGenerations) > 0 {
		parts := make([]string, len(d.UncoveredGenerations))
		for i, g := range d.UncoveredGenerations {
			parts[i] = strconv.FormatInt(g, 10)
		}
		fmt.Fprintf(&b, "  - "+freshResumeNoEvidenceGenerationText+"\n", strings.Join(parts, ", "))
	}
	b.WriteString("\n")
	return b.String()
}

func confirmFreshResume(env Env) (bool, error) {
	_, _ = fmt.Fprint(env.Stderr, "Start a fresh resume on a separate hosted worker? [y/N]: ")
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, uzicli.Exitf(uzicli.ExitGeneric, "reading confirmation from stdin: %v", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
