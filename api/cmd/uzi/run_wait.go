package main

// run_wait.go holds `uzi run wait` and its target/deadline helpers (PRD #1009 M1).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// runWait blocks until the run enters one of `until` (default defaultWaitStates),
// polling GET /api/runs/:id every `interval` (PRD #264 D1, client-side — no server
// long-poll). It returns nil (exit 0) the instant the run is in a target state,
// including on the first poll; it never waits on a state it cannot leave toward a
// target, and `timeout` (opt-in, D6) bounds even an unrecognized status.
//
// Exit codes (D3): 0 target reached; 4 not found (immediate, never retried); 6 server
// unreachable after runWaitMaxUnreachable CONSECUTIVE failures (D9 — a single blip is
// ridden out); 7 timeout. Transitions go to STDERR (D4) so `--json`'s final run object
// on stdout stays a clean single document.
func runWait(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, runID string, until []string, interval, timeout time.Duration, minPlanSeq int) error {
	targets, err := waitTargets(until)
	if err != nil {
		return err
	}
	if interval <= 0 {
		interval = runWaitPollInterval
	}

	// One timer for the whole wait so the deadline fires during a poll interval AND
	// during a post-blip backoff. Nil channel (timeout <= 0) blocks forever in the
	// select, which is exactly "no timeout".
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}

	consecutiveUnreachable := 0
	lastStatus := ""
	sawStatus := false
	warnedUnknown := map[string]bool{}
	warnedPlanSeqErr := false
	ctx := cmd.Context()

	for {
		run, err := c.GetRun(ctx, runID)
		if err != nil {
			var ee *uzicli.ExitError
			// Only an unreachable server (5xx/network) is transient; a 4/other is a fact
			// about the request and is returned at once.
			if errors.As(err, &ee) && ee.Code == uzicli.ExitUnreachable {
				consecutiveUnreachable++
				if consecutiveUnreachable >= runWaitMaxUnreachable {
					return err
				}
				_, _ = fmt.Fprintf(env.Stderr, "run %s: %s (retry %d/%d)\n",
					runID, cellText(err.Error()), consecutiveUnreachable, runWaitMaxUnreachable)
				if done, werr := waitOrDeadline(ctx, runID, runWaitBackoff, deadline); done {
					return werr
				}
				continue
			}
			return err
		}
		consecutiveUnreachable = 0

		// A transition line on every status change, including the first observation, so a
		// human watching stderr sees the run move. cellText folds a newline a rotted
		// "server-controlled" status could carry, keeping one status per line.
		if run.Status != lastStatus {
			if sawStatus {
				_, _ = fmt.Fprintf(env.Stderr, "run %s: %s → %s\n", runID, cellText(lastStatus), cellText(run.Status))
			} else {
				_, _ = fmt.Fprintf(env.Stderr, "run %s: %s\n", runID, cellText(run.Status))
			}
			lastStatus = run.Status
			sawStatus = true
		}
		// A status outside the ten-value enum means the server is newer than this
		// binary. Surface it once and keep waiting (it can never be a target — `--until`
		// only names known statuses), so it is never a silent forever-wait; `--timeout`
		// still bounds it (R1).
		if !allRunStatuses[run.Status] && !warnedUnknown[run.Status] {
			warnedUnknown[run.Status] = true
			_, _ = fmt.Fprintf(env.Stderr,
				"run %s: unrecognized status %q — treating as non-terminal; this CLI may be older than the server\n",
				runID, cellText(run.Status))
		}

		if targets[run.Status] {
			// PRD #603: `--min-plan-seq N` (N >= 0) gates ONLY the awaiting_approval
			// stop so a wait after `run revise` returns on the REVISED plan, not the
			// stale gate left by the pre-revise plan. Every other target — terminals,
			// awaiting_input, awaiting_followup — still stops unconditionally, matching
			// watch-run.sh (cur > MIN, awaiting_approval only). On a RunLogs error we
			// cannot confirm a fresh plan, so we keep waiting (bounded by --timeout)
			// rather than exit 0 on the stale gate; a first-error stderr note aids a
			// human watching but does not change control flow.
			if minPlanSeq >= 0 && run.Status == "awaiting_approval" {
				cur := 0
				msgs, lerr := c.RunLogs(ctx, runID, 0)
				if lerr != nil {
					if !warnedPlanSeqErr {
						warnedPlanSeqErr = true
						_, _ = fmt.Fprintf(env.Stderr,
							"run %s: cannot read plan messages (%s) — still waiting for a plan newer than seq %d\n",
							runID, cellText(lerr.Error()), minPlanSeq)
					}
				} else {
					cur = maxPlanSeq(msgs)
				}
				if cur <= minPlanSeq {
					// No fresh plan yet: fall through to the poll/sleep and keep waiting.
					if done, werr := waitOrDeadline(ctx, runID, interval, deadline); done {
						return werr
					}
					continue
				}
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return nil
		}

		if done, werr := waitOrDeadline(ctx, runID, interval, deadline); done {
			return werr
		}
	}
}

// maxPlanSeq returns the highest Seq among plan messages, or 0 when there are none —
// matching watch-run.sh's `[.[]|select(.kind=="plan")|.seq]|max // 0`. The plan kind is
// the literal "plan" (agent/src/runner.ts).
func maxPlanSeq(msgs []apitypes.MessageDTO) int {
	max := 0
	for _, m := range msgs {
		if m.Kind == "plan" && int(m.Seq) > max {
			max = int(m.Seq)
		}
	}
	return max
}

// waitTargets resolves `run wait`'s target set from the `--until` flag, defaulting to
// defaultWaitStates. Every named status is validated against allRunStatuses so a typo
// is a clean usage error (exit 2) rather than a wait that can never end.
func waitTargets(until []string) (map[string]bool, error) {
	until = nonEmpty(until)
	if len(until) == 0 {
		until = defaultWaitStates
	}
	targets := make(map[string]bool, len(until))
	for _, s := range until {
		s = strings.TrimSpace(s)
		if !allRunStatuses[s] {
			return nil, uzicli.Exitf(uzicli.ExitUsage,
				"--until: %q is not a run status (valid: %s)", s, strings.Join(allRunStatusesOrder, ", "))
		}
		targets[s] = true
	}
	return targets, nil
}

// waitOrDeadline sleeps for d, returning (true, exit-7 error) if the wait deadline
// fires first and (true, nil) if the context is cancelled — mirroring `run logs
// --follow`, which treats a cancelled context as a clean stop. (false, nil) means the
// pause elapsed and the caller should poll again.
func waitOrDeadline(ctx context.Context, runID string, d time.Duration, deadline <-chan time.Time) (bool, error) {
	select {
	case <-ctx.Done():
		return true, nil
	case <-deadline:
		return true, uzicli.Exitf(uzicli.ExitTimeout, "run %s did not reach a target state before the timeout", runID)
	case <-time.After(d):
		return false, nil
	}
}
