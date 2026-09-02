package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Run-input kinds, the exact wire strings the inputs endpoint accepts
// (handler/workers.go runInputKinds). Named so a typo is a compile error, not a
// silent 400.
const (
	kindApprovePlan = "approve_plan"
	kindRejectPlan  = "reject_plan"
	kindCancel      = "cancel"
	kindFollowUp    = "follow_up"
	kindRevisePlan  = "revise_plan"
	kindAnswer      = "answer"
	kindStop        = "stop"
	kindScope       = "scope"
)

// agentSources are the two rosters a plan approval may draw its subagents from
// (apitypes.AgentSelection.Source). "own" is the run owner's template roster; "repo"
// is the set the worker detected in the clone's .claude/agents/.
const (
	agentSourceOwn  = "own"
	agentSourceRepo = "repo"
)

// statusLimitWait is the status a run carries while parked behind its owner's
// exhausted Anthropic usage window (PRD #35). Named so the three surfaces that must
// agree about it — the follow loop, the steer queue's delivery label and `uzi run
// get`'s detail block — compare against one literal.
//
// NON-TERMINAL, and that is the whole reason it needs handling rather than a slot in
// terminalRunStatuses: a parked run is promoted back to `queued` by the server's sweep
// once retry_not_before passes, and resumes on the same SDK session. Treating it as
// terminal would make `--follow` exit on a run that is about to produce more messages.
const statusLimitWait = "limit_wait"

// statusPoolWait is the status an `auto` run carries while HELD because its token pool
// is genuinely empty (PRD #754 M4). Named for the same reason as statusLimitWait: the
// follow loop, the steer-queue delivery label and `uzi run get`'s detail block compare
// against one literal.
//
// NON-TERMINAL, and non-locking (it does not lock the issue). A held run resumes when a
// token is pooled (reactively, or manually) in M5. Like limit_wait, treating it as
// terminal would make `--follow` exit on a run that is about to produce more messages.
const statusPoolWait = "pool_wait"

// logsPollInterval is how often `uzi run logs --follow` re-polls
// /api/runs/{id}/messages?after=<seq>. REST polling ships instead of a WebSocket
// (PRD #64 Out of scope). A var, not a const, only so tests can shrink the wait;
// nothing at runtime reassigns it.
var logsPollInterval = 2 * time.Second

// terminalRunStatuses are the run states from which no further messages can
// arrive. `uzi run logs --follow` stops once the run reaches one of them (after a
// final drain) instead of polling forever — otherwise an agent capturing a
// finished run hangs. Mirrors the store's `status NOT IN (...)` active filter.
//
// statusLimitWait is NOT here, and it is the one omission worth writing down because
// it looks like an oversight: a park is a long silence, which is exactly what this map
// is used to end. But the run resumes, and stopping the follow on it would truncate
// the capture in the middle of a run that finishes normally hours later. The silence
// is instead EXPLAINED — see the follow loop's one-shot notice below.
var terminalRunStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"cancelled": true,
}

// allRunStatusesOrder is the run status enum in wire/enum order (matching
// runs_status_check — last rewritten by migration 00165, eleven values), the ONE source
// of truth both allRunStatuses (membership)
// and the `--until` validation-error's "valid: …" list derive from — so a status added
// here can never be silently omitted from the human-readable enumeration.
var allRunStatusesOrder = []string{
	"queued",
	"claimed",
	"running",
	"awaiting_approval",
	"awaiting_input",
	"awaiting_followup",
	statusLimitWait,
	statusPoolWait,
	"completed",
	"failed",
	"cancelled",
}

// allRunStatuses is the run status enum the skill documents and migration
// 00165 constrains (runs_status_check). It is the source of truth `run wait`
// validates `--until` against, so a typo'd target is a clean usage error rather than
// a silent forever-wait. A status the SERVER reports that is NOT in this set is a
// newer server than this binary (surfaced, treated non-terminal — never a target,
// since `--until` can only name a member here). Derived from allRunStatusesOrder so the
// membership set and the enumerated list cannot drift.
var allRunStatuses = func() map[string]bool {
	m := make(map[string]bool, len(allRunStatusesOrder))
	for _, s := range allRunStatusesOrder {
		m[s] = true
	}
	return m
}()

// defaultWaitStates is `run wait`'s `--until` default (PRD #264 D2): the "actionable"
// set — every state that needs the caller or ends the run. It INCLUDES awaiting_followup
// (PRD #517 D9): an interactive task parked awaiting the user's next follow-up needs the
// caller and does NOT auto-resume, so a bare `uzi run wait <id>` must stop on it. It
// deliberately OMITS queued/claimed/running (still working), limit_wait (auto-resumes;
// parking on it is legitimate) AND pool_wait (PRD #754: a held run resumes on its own
// once a token is pooled, so it is legitimate to wait through, exactly like limit_wait),
// so a bare `uzi run wait <id>` returns at the plan gate, a clarification park, a
// follow-up park, or a terminal — the common "wait for the gate OR the end" case.
var defaultWaitStates = []string{"awaiting_approval", "awaiting_input", "awaiting_followup", "completed", "failed", "cancelled"}

// run wait poll cadence and transient-blip resilience knobs (PRD #264 D1/D9). Vars,
// not consts, only so tests shrink the waits; nothing at runtime reassigns them.
//
//   - runWaitPollInterval — the default `--interval` between polls of GET /api/runs/:id.
//   - runWaitBackoff — the pause after a mid-wait ExitUnreachable before retrying.
//   - runWaitMaxUnreachable — how many CONSECUTIVE unreachable polls end the wait with
//     exit 6. A single 5xx/network blip (the skill calls it "transient; back off and
//     retry") must not kill a multi-hour gate-wait, so the default is small but > 1.
var (
	runWaitPollInterval   = 3 * time.Second
	runWaitBackoff        = 2 * time.Second
	runWaitMaxUnreachable = 5
)

// runAgeCell renders the AGE column for `uzi run list` (issue #256 M5), the CLI twin
// of the web's runDurationLabel. AGE means different things per state, so the anchor is
// chosen by status (Decision 2), not by a single "created" reading:
//
//   - running   → time since StartedAt (when the agent began), or CreatedAt if unstamped.
//   - claimed   → time since ClaimedAt (when a worker took it), or CreatedAt if unstamped.
//   - queued    → time since CreatedAt (how long it has waited to be claimed).
//   - awaiting_approval / awaiting_input / awaiting_followup / limit_wait / pool_wait →
//     time since UpdatedAt, i.e. how long it has been parked/held in that waiting state.
//   - completed / failed / cancelled → the STATIC span FinishedAt−StartedAt, how long it
//     actually ran, independent of now. A terminal run with no StartedAt (cancelled or
//     failed before it ever started) never ran, so it renders "-".
//   - any other/unknown status → "-".
//
// The live (non-terminal) buckets pass now.Sub(anchor) through formatUptimeDuration, which
// floors negatives (clock skew) to "<1m". now is taken as a parameter, not read from the
// clock inside, so the buckets unit-test deterministically — the same split as
// formatUptimeDuration out of uptimeCell.
func runAgeCell(r apitypes.RunDTO, now time.Time) string {
	var anchor *time.Time
	switch r.Status {
	case "running":
		if r.StartedAt != nil {
			anchor = r.StartedAt
		} else {
			anchor = &r.CreatedAt
		}
	case "claimed":
		if r.ClaimedAt != nil {
			anchor = r.ClaimedAt
		} else {
			anchor = &r.CreatedAt
		}
	case "queued":
		anchor = &r.CreatedAt
	case "awaiting_approval", "awaiting_input", "awaiting_followup", statusLimitWait, statusPoolWait:
		anchor = &r.UpdatedAt
	case "completed", "failed", "cancelled":
		// A static ran-span, not a live age: only meaningful when the run both started
		// and finished. Missing either end (never started) → "-".
		if r.StartedAt == nil || r.FinishedAt == nil {
			return "-"
		}
		return formatUptimeDuration(r.FinishedAt.Sub(*r.StartedAt))
	default:
		return "-"
	}
	if anchor == nil {
		return "-"
	}
	return formatUptimeDuration(now.Sub(*anchor))
}

// newRunCmd — `uzi run` and its verbs. Every verb is wired to the Client: the reads
// (list/get/logs/review/inputs) and the writes (create/approve/reject/cancel/
// follow-up/answer).
func newRunCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "List, inspect, and drive runs",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(runs)
			}
			rows := make([][]string, 0, len(runs))
			now := time.Now()
			for _, r := range runs {
				rows = append(rows, []string{r.ID, r.Kind, effectiveRunStatus(r.Status, r.IsPlanning, r.IsRevising), runAgeCell(r.RunDTO, now), runTitle(r.RunDTO)})
			}
			return p.Table([]string{"ID", "KIND", "STATUS", "AGE", "TITLE"}, rows)
		},
	}

	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's status and details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			fields, _ := cmd.Flags().GetStringSlice("field")
			fields = nonEmpty(fields)
			p := env.printer(gf)
			// --field and --json are two output modes; refusing the combination up front
			// keeps each single-purpose (a scalar-per-line stream vs a JSON document).
			if len(fields) > 0 && p.Format == uzicli.FormatJSON {
				return uzicli.Exitf(uzicli.ExitUsage, "--field cannot be combined with --json")
			}
			run, err := c.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(fields) > 0 {
				return printRunFields(env, run, fields)
			}
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	get.Flags().StringSlice("field", nil,
		"print only these top-level scalar field(s), raw and one per line (repeatable; mutually exclusive with --json)")

	logs := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Print a run's message history (REST polling)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			follow, _ := cmd.Flags().GetBool("follow")
			after, _ := cmd.Flags().GetInt32("after")
			p := env.printer(gf)
			seq := after
			drain := func() error {
				msgs, err := c.RunLogs(cmd.Context(), args[0], seq)
				if err != nil {
					return err
				}
				for _, m := range msgs {
					if err := renderMessage(p, m); err != nil {
						return err
					}
					if m.Seq > seq {
						seq = m.Seq
					}
				}
				return nil
			}
			// parked tracks whether the LAST poll saw the run parked on a usage limit,
			// so the notice below fires on the EDGE into the park rather than every
			// 2 seconds for the hours one lasts.
			parked := false
			for {
				if err := drain(); err != nil {
					return err
				}
				if !follow {
					return nil
				}
				// Stop once the run reaches a terminal state: no further messages can
				// arrive, so an agent running `--follow` on a finished run must exit
				// (exit 0), not poll forever. Check AFTER draining this round; on a
				// terminal run drain once more (messages are persisted before the run
				// flips terminal — a gapless-seq guarantee) so nothing is dropped.
				run, err := c.GetRun(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if terminalRunStatuses[run.Status] {
					return drain()
				}
				// A park is the only status this loop rides out that produces NOTHING
				// for hours, and it is indistinguishable from a hang or a wedged agent
				// from the outside — the failure mode the milestone exists to fix.
				//
				// STDERR, not the Printer: the Printer is stdout, and `--json` streams
				// NDJSON there for an agent to parse line by line (renderMessage). A
				// human-readable notice on that stream would corrupt the contract. This
				// is the same split cobra's deprecation notice already uses here.
				if run.Status == statusLimitWait || run.Status == statusPoolWait {
					if !parked {
						parked = true
						// pool_wait is the sibling silence limit_wait is (both are long,
						// output-less holds that look like a hang from the outside), so it
						// earns the same one-shot notice — but a DIFFERENT one, because it
						// resumes on a different trigger: a pooled token, not a clock. A
						// direct limit_wait⇄pool_wait transition would be missed by the bare
						// `parked` bool, but it cannot happen — a held run is promoted to
						// `queued` (a non-held status that clears `parked` via the else-if
						// below) before it could hold again, so re-arming here is exact.
						if run.Status == statusPoolWait {
							_, _ = fmt.Fprintf(env.Stderr,
								"run %s held — its token pool is empty; still following, it resumes when a token is pooled\n",
								args[0])
						} else {
							_, _ = fmt.Fprintf(env.Stderr, "run %s %s — still following; it resumes on its own\n",
								args[0], limitWaitLine(run, time.Now()))
						}
					}
				} else if parked {
					parked = false
					// cellText, NOT sanitizeTTY, and the difference is the whole point:
					// sanitizeTTY spares "\n", so a status carrying one would inject a
					// line onto stderr. Unreachable today because runs_status_check
					// constrains status to eleven values (migration 00165) — which is precisely the argument
					// limitWaitLine's own comment REJECTS for rate_limit_type ("server-
					// controlled today" is exactly the assumption that rots). Holding one
					// line of this file to a weaker standard than the line beside it, on a
					// premise that line disowns, is not a defensible split.
					_, _ = fmt.Fprintf(env.Stderr, "run %s resumed (%s)\n", args[0], cellText(run.Status))
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(logsPollInterval):
				}
			}
		},
	}
	logs.Flags().Bool("follow", false, "keep polling for new messages")
	logs.Flags().Int32("after", 0, "only show messages after this sequence number")

	wait := &cobra.Command{
		Use:   "wait <run-id>",
		Short: "Block until a run reaches a chosen state",
		Long: "Poll a run until its status enters the `--until` set, then exit 0 (PRD #264).\n\n" +
			"With no `--until`, it stops on any state that needs you or ends the run: " +
			strings.Join(defaultWaitStates, ", ") + ". It does NOT stop " +
			"on queued/claimed/running (still working), limit_wait (auto-resumes), or pool_wait " +
			"(an auto run held on an empty token pool; resumes when a token is pooled), so a bare " +
			"`uzi run wait <id>` waits for the plan gate OR the end.\n\n" +
			"Transitions print to stderr; `--json` prints the final run object (same shape as " +
			"`run get --json`) to stdout. Exit codes: 0 a target state was reached (including if " +
			"the run was already in one); 4 not found; 6 server unreachable after repeated " +
			"failures; 7 `--timeout` elapsed before any target state.\n\n" +
			"After approving a plan, NARROW the second wait — `run wait <id> --until " +
			"completed,failed,cancelled` — because a run lingers at awaiting_approval for a beat " +
			"after a successful approve, so a bare `run wait` would return immediately at the " +
			"gate it just cleared.\n\n" +
			"To wait for a REVISED plan after `run revise`, capture the latest plan seq first " +
			"(`run logs <id> --json | jq -rs '[.[]|select(.kind==\"plan\")|.seq]|max // 0'`), then " +
			"`run wait <id> --min-plan-seq <seq>`. That gates ONLY the awaiting_approval stop — it " +
			"returns at the gate only once a plan message with seq > <seq> exists, so it does not " +
			"return on the stale gate left by the pre-revise plan. Terminal states (and every other " +
			"target) still stop unconditionally.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			until, _ := cmd.Flags().GetStringSlice("until")
			interval, _ := cmd.Flags().GetDuration("interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			minPlanSeq, _ := cmd.Flags().GetInt("min-plan-seq")
			return runWait(env, gf, c, cmd, args[0], until, interval, timeout, minPlanSeq)
		},
	}
	wait.Flags().StringSlice("until", nil,
		"comma-separated run statuses to wait for (default: "+strings.Join(defaultWaitStates, ",")+")")
	wait.Flags().Duration("interval", runWaitPollInterval, "how often to poll the run's status")
	wait.Flags().Duration("timeout", 0, "give up with exit 7 after this long (default: no timeout)")
	wait.Flags().Int("min-plan-seq", -1,
		"after `run revise`, only stop at awaiting_approval once a plan message with seq > N exists "+
			"(-1 = off; 0 = wait for any plan, since plan seqs start at 1). Terminal states still stop unconditionally")

	// review is now a HIDDEN, DEPRECATED alias of `uzi review show` (PRD #94
	// Decision 10). It shares runReviewShow so the two stay byte-identical.
	// SetOut(env.Stderr) forces cobra's deprecation notice — printed via
	// OutOrStderr, which the root's SetOut(env.Stdout) would otherwise route to
	// STDOUT — onto stderr, keeping --json output pure (TestRunReviewJSON*).
	review := &cobra.Command{
		Use:        "review <run-id>",
		Short:      "Show the judge's review for a run (read-only)",
		Args:       cobra.ExactArgs(1),
		Hidden:     true,
		Deprecated: "use \"uzi review show\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runReviewShow(env, gf, c, cmd, args[0])
		},
	}
	review.SetOut(env.Stderr)

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
			force, _ := cmd.Flags().GetBool("force")
			run, err := c.CreateRun(cmd.Context(), repoID, issue, waitOnLimitFlag(cmd), mrReworkFlag(cmd), force, seed)
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

	scope := &cobra.Command{
		Use:   "scope <run-id>",
		Short: "Cap a milestone run's scope: complete through milestone N, then finalize (issue runs)",
		Long: "Set an operator SCOPE CEILING on an in-flight milestone-structured issue run: the run \n" +
			"completes through milestone N (1-based count over the approved, frozen milestone list), \n" +
			"then finalizes the committed slice (pushes the branch, opens the merge request when \n" +
			"requested) and starts no further milestone. The ceiling is clamped to " +
			"[already-completed, total]. A later `run scope` (or `run stop`) supersedes an earlier one. \n" +
			"Owner-only; valid only on a milestone-structured issue run (409 otherwise).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("through") {
				return uzicli.Exitf(uzicli.ExitUsage, "run scope needs --through N")
			}
			n, _ := cmd.Flags().GetInt("through")
			if err := submitInput(env, gf, c, cmd, args[0], kindScope, strconv.Itoa(n), nil); err != nil {
				return err
			}
			// The server clamps the ceiling to [already-completed, total]; the applied value
			// and its disposition surface via `run inputs`, not here (a read-back would race
			// the worker settling it). Point the operator there.
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stderr, "run `uzi run inputs %s` to see the applied (clamped) ceiling and its disposition\n", args[0])
			}
			return nil
		},
	}
	scope.Flags().Int("through", 0, "complete through milestone N, then stop (1-based count over the frozen list)")

	followUp := &cobra.Command{
		Use:   "follow-up <run-id>",
		Short: "Send a follow-up message to a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			if strings.TrimSpace(msg) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a follow-up needs a message: pass -m <message> or pipe it on stdin")
			}
			return submitInput(env, gf, c, cmd, args[0], kindFollowUp, msg, nil)
		},
	}
	followUp.Flags().StringP("message", "m", "", "the follow-up message (or pipe it on stdin)")

	revise := &cobra.Command{
		Use:   "revise <run-id>",
		Short: "Revise a run's plan at the approval gate (re-plan without stopping the run)",
		Long: "Send feedback to a run parked at its plan-approval gate (`awaiting_approval`) so the " +
			"agent re-plans from your notes and re-gates, WITHOUT stopping the run — unlike `reject`, " +
			"which ends it.\n\n" +
			"The run stays live: the agent revises its plan in place using your feedback and returns to " +
			"the gate for another approve/reject/revise decision. Revisions are capped by the run's " +
			"revision limit; once exhausted — or if the run has already finished — the server answers " +
			"409 (exit 5). Use it on a run parked at its `awaiting_approval` gate; that is where the " +
			"agent folds your feedback into a new plan.\n\n" +
			"A revision needs feedback (an empty one tells the agent nothing to change), so pass -m or " +
			"pipe it on stdin.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			if strings.TrimSpace(msg) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a revision needs a message: pass -m <feedback> or pipe it on stdin")
			}
			return submitInput(env, gf, c, cmd, args[0], kindRevisePlan, msg, nil)
		},
	}
	revise.Flags().StringP("message", "m", "", "the plan feedback to send back (or pipe it on stdin)")

	answer := &cobra.Command{
		Use:   "answer <run-id>",
		Short: "Answer the clarifying question a run is waiting on",
		Long: "Answer the question a run asked with ask_user (PRD #88). The run parks in " +
			"`awaiting_input` until you reply, then resumes the same agent session with your " +
			"answer.\n\n" +
			"The open question is read from the run's own feed — the newest `question` " +
			"message — rather than from a run field, so the CLI, the web UI and Slack all " +
			"derive it the same way. Pass -m, or pipe the answer on stdin. Repeat -m once " +
			"per question when the agent asked several; answers are matched in order.\n\n" +
			"The answer names the question it answers, so a reply written against a question " +
			"the agent has already moved on from is rejected rather than applied to the " +
			"current one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			answers, _ := cmd.Flags().GetStringArray("message")
			if len(answers) == 1 {
				answers[0] = resolveMessage(env, answers[0])
			} else if len(answers) == 0 {
				answers = []string{resolveMessage(env, "")}
			}
			if len(answers) == 1 && strings.TrimSpace(answers[0]) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "an answer needs text: pass -m <answer> or pipe it on stdin")
			}
			q, err := openQuestion(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			// Cross-check the count against the QUESTION, not just against the server's
			// upper bound. Answers are index-aligned, so a miscount does not fail — it
			// silently pairs answer 2 with question 3 and hands the agent a confident
			// mismatch. The server cannot catch this: maxAnswerCount bounds the top end
			// but nothing server-side knows how many questions this id asked.
			if len(answers) != len(q.Questions) {
				return uzicli.Exitf(uzicli.ExitUsage,
					"this question has %d part(s) but you gave %d answer(s) — answers are matched in order, so pass one -m per part",
					len(q.Questions), len(answers))
			}
			// Reject a blank among several. A lone blank is already rejected above; a
			// blank in the middle is worse, because it looks like an answer and reads to
			// the agent as "(no answer given)" for a question the user believed they had
			// answered.
			for i, a := range answers {
				if strings.TrimSpace(a) == "" {
					return uzicli.Exitf(uzicli.ExitUsage, "answer %d is empty — every part needs text", i+1)
				}
			}
			body, err := json.Marshal(answerBody{QuestionID: q.QuestionID, Answers: answers})
			if err != nil {
				return err
			}
			return submitInput(env, gf, c, cmd, args[0], kindAnswer, string(body), nil)
		},
	}
	answer.Flags().StringArrayP("message", "m", nil, "the answer (repeat once per question; or pipe a single answer on stdin)")

	inputs := &cobra.Command{
		Use:   "inputs <run-id>",
		Short: "List a run's steer queue (follow-ups) with delivery status",
		Long: "List the steer queue sent to a run (PRD #95, #634), newest first, each with its " +
			"KIND and state plus a relative age. Owner-only: a read-only admin token gets 404 on " +
			"another user's run.\n\n" +
			"The queue lists two kinds. A follow-up (kind='follow_up') carries a delivery state — " +
			"queued (the worker has not drained it yet) or delivered (handed to the worker for its " +
			"next turn). An operator scope directive (kind='scope', PRD #634) carries its " +
			"disposition instead — applied/declined/superseded, or active while the ceiling is still " +
			"pending — because a scope row is never consumed. Approve/reject/cancel are omitted.\n\n" +
			"A chat run seeds every chat turn as a follow_up, so `uzi run inputs` on a chat run lists " +
			"the seeded prompt and all chat turns; an issue run's queue starts empty (its prompt " +
			"rides the claim payload, not an input row).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			list, err := c.RunInputs(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				// The agent contract: emit the raw DTO list. State is derived
				// client-side (from consumed_at + the run's status), so an agent
				// computes it from these fields itself — the CLI never fetches the
				// run in --json mode.
				return p.JSON(list)
			}
			// Human render only: derive the delivery state (Decision 7), which needs
			// the run's live status for the gate/terminal nuance. One cheap GetRun; if
			// it fails, status stays "" and the state degrades to the queued/delivered
			// floor (Decision 10). Skipped entirely for an empty queue.
			status := ""
			if len(list) > 0 {
				if run, err := c.GetRun(cmd.Context(), args[0]); err == nil {
					status = run.Status
					if run.Kind == runkind.Chat {
						// N3: a chat run's queue is every chat turn. Note it only when it
						// actually applies, so an issue run's output stays clean.
						_, _ = fmt.Fprintln(env.Stderr, "note: chat run — this queue lists every chat turn as a follow-up (chat has its own web composer, unaffected)")
					}
				}
			}
			return renderRunInputs(p, list, status)
		},
	}

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

	cmd.AddCommand(list, get, logs, wait, review, create, approve, reject, revise, cancel, stop, scope, followUp, answer, inputs, expedite, resumeNow, mrRework)
	return cmd
}

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

// approveSelection maps the plan-gate flags to the wire AgentSelection
// {source, exclusions} (PRD #37 Decision 4: pick a source, exclude individual
// agents — either/or, no mixing, no allow-list). It NEVER fetches the run: the
// server validates the selection against the run's live roster, so the CLI just
// forwards the structured choice.
//
//   - neither flag → nil: send NO selection; the server applies the run's default
//     (repo-when-detected else own, no exclusions). This is the common case.
//   - --agent-source own|repo → that source, with any --exclude-agents.
//   - --exclude-agents without --agent-source → usage error: without a fetch the CLI
//     can't know the run's default source, and exclusions are validated against the
//     chosen source's roster server-side.
//
// `lead` is never excludable; if a user names it, the server rejects it — the CLI
// does not special-case it client-side.
func approveSelection(source string, exclude []string) (*apitypes.AgentSelection, error) {
	exclude = nonEmpty(exclude)
	if source == "" {
		if len(exclude) > 0 {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "specify --agent-source with --exclude-agents")
		}
		return nil, nil
	}
	if source != agentSourceOwn && source != agentSourceRepo {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "--agent-source must be 'own' or 'repo'")
	}
	return &apitypes.AgentSelection{Source: source, Exclusions: exclude}, nil
}

// maxPlanReadBytes bounds a seeded plan read from STDIN (PRD #209 M3) so a hostile
// pipe cannot make the CLI allocate without bound. It sits ABOVE the server's
// create-time cap (workersvc.MaxSeededPlanBytes, 256 KiB) on purpose: an over-cap plan
// is then read whole and rejected by the server's 422, never silently truncated here
// into a shorter plan that would look valid.
const maxPlanReadBytes = 1 << 20 // 1 MiB

// plannedCommitRe mirrors the server's workersvc.plannedCommitRe (PRD #209 M4): hex, 7-64
// chars. The server is authoritative; this is a pre-flight so a malformed --planned-commit
// is a clean exit-2 usage error before any request rather than a round-trip 400.
var plannedCommitRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// seededPlanFlag assembles PRD #209's optional seeded plan from the `create` flags, and
// returns nil when no --plan-file was given (an ordinary issue-planned run, unchanged).
//
// It reuses approveSelection — so `--exclude-agents` without `--agent-source` is the
// SAME usage error the plan gate raises — and mirrors the server's coupling
// client-side: a roster flag with no --plan-file is a usage error here (the server
// would answer 400 "agent_selection requires plan_md"), so the user gets a clean
// message before any request is sent. The plan's size cap and empty-plan rejection stay
// the SERVER's (422): readPlanFile forwards the bytes.
func seededPlanFlag(env Env, cmd *cobra.Command) (*uzicli.CreateRunSeed, error) {
	source, _ := cmd.Flags().GetString("agent-source")
	exclude, _ := cmd.Flags().GetStringSlice("exclude-agents")
	sel, err := approveSelection(source, exclude)
	if err != nil {
		return nil, err
	}
	plannedCommit, _ := cmd.Flags().GetString("planned-commit")
	plannedCommit = strings.TrimSpace(plannedCommit)
	requireBase, _ := cmd.Flags().GetBool("require-base")
	// M4: --require-base needs a --planned-commit to compare against; without one the
	// flag can never fire (usage error before any request, mirroring the server's 400).
	if requireBase && plannedCommit == "" {
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"--require-base requires --planned-commit (there is no commit to compare the clone's base against)")
	}
	// A pre-flight mirror of the server's ErrInvalidPlannedCommit (the server is the
	// authority): reject a --planned-commit that is not a plausible git sha before any
	// request, so the user gets exit 2 rather than a round-trip 400. A too-short value is
	// the load-bearing case — it would silently disarm --require-base server-side.
	if plannedCommit != "" && !plannedCommitRe.MatchString(plannedCommit) {
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"--planned-commit must be a hex commit sha of 7-64 characters")
	}
	planProvided := cmd.Flags().Changed("plan-file")
	// A roster / a planned commit / a require-base is only meaningful for a seeded plan.
	// Reject any of them without --plan-file up front, mirroring the server's
	// "agent_selection requires plan_md" / "planned_commit and require_base require
	// plan_md" 400s with a clean message before any request is sent.
	if (sel != nil || plannedCommit != "" || requireBase) && !planProvided {
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"--agent-source/--exclude-agents/--planned-commit/--require-base require --plan-file "+
				"(they are only meaningful for a seeded plan)")
	}
	if !planProvided {
		return nil, nil
	}
	path, _ := cmd.Flags().GetString("plan-file")
	plan, err := readPlanFile(env, path)
	if err != nil {
		return nil, err
	}
	return &uzicli.CreateRunSeed{
		PlanMD:        plan,
		Selection:     sel,
		PlannedCommit: plannedCommit,
		RequireBase:   requireBase,
	}, nil
}

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

// submitInput sends one steering input and reports the outcome. server_side (a
// cancel/reject applied without a live worker) is surfaced so the caller knows the
// action took effect immediately rather than being queued.
func submitInput(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, runID, kind, body string, sel *apitypes.AgentSelection) error {
	res, err := c.SubmitRunInput(cmd.Context(), runID, kind, body, sel)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res)
	}
	if !gf.quiet {
		p.Printf("%s: %s\n", runID, inputOutcome(kind, res.ServerSide))
	}
	return nil
}

// answerBody is the wire shape of an `answer` input (PRD #88 M1). JSON rather than
// the bare prose every other kind carries, because an answer must name the question
// it answers — the server rejects one naming a question that is no longer open, which
// is what stops a reply written against an earlier question from resolving the
// current one.
type answerBody struct {
	QuestionID string   `json:"question_id"`
	Answers    []string `json:"answers"`
}

// questionPayload is the part of a `question` run-message the CLI reads.
type questionPayload struct {
	QuestionID string `json:"question_id"`
	Questions  []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// openQuestion reads the run's newest `question` message and returns its payload.
//
// Derived from the FEED rather than a run field (PRD #88 D-L), so the CLI, the web UI
// and Slack share one derivation rule instead of three. RunLogs returns messages in
// seq order, so the last `question` is the open one.
//
// KNOWN COST, stated rather than hidden: this pulls the run's ENTIRE message history
// to read one field. Nothing new is exposed — it is the same route and the same
// authorization as `uzi run logs` — but `logs` is expected to be heavy and `answer` is
// not, so a long run makes a one-line command download megabytes. Fixing it properly
// needs a server-side "latest message of kind K" read that does not exist yet; adding
// one for a single CLI verb was judged the wrong trade against D-L's one-derivation
// rule, which is what keeps the CLI, web and Slack from drifting.
func openQuestion(ctx context.Context, c uzicli.Client, runID string) (questionPayload, error) {
	msgs, err := c.RunLogs(ctx, runID, 0)
	if err != nil {
		return questionPayload{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind != "question" {
			continue
		}
		var q questionPayload
		if err := json.Unmarshal(msgs[i].Payload, &q); err != nil {
			return questionPayload{}, uzicli.Exitf(uzicli.ExitGeneric, "could not read the run's open question: %v", err)
		}
		if strings.TrimSpace(q.QuestionID) == "" {
			return questionPayload{}, uzicli.Exitf(uzicli.ExitGeneric, "the run's open question carries no id")
		}
		return q, nil
	}
	// No question in the feed at all. Distinct from "the run moved on", which the
	// server reports, because this one is almost always a mistyped run id.
	return questionPayload{}, uzicli.Exitf(uzicli.ExitConflict, "run %s is not waiting for an answer", runID)
}

// inputOutcome is the human confirmation line for a submitted input.
func inputOutcome(kind string, serverSide bool) string {
	switch kind {
	case kindApprovePlan:
		return "plan approved"
	case kindRejectPlan:
		if serverSide {
			return "plan rejected (run stopped)"
		}
		return "plan rejection sent"
	case kindCancel:
		if serverSide {
			return "run cancelled"
		}
		return "cancellation sent"
	case kindStop:
		// A stop always enqueues (never a server-side transition), so serverSide is always
		// false here — the worker finalizes the graceful wind-down.
		return "stop sent"
	case kindFollowUp:
		return "follow-up sent"
	case kindRevisePlan:
		return "plan revision sent"
	case kindAnswer:
		return "answer sent"
	case kindScope:
		return "scope directive sent"
	default:
		return "input submitted"
	}
}

// renderCreatedRun prints a newly created run and — Risk 4 — warns on stderr when it
// already carries a health reason (e.g. a locked vault parks it queued). The reliable
// surface remains `uzi run get`/`run list`, which poll the same reason; this warns at
// create time so an agent that queues then polls is not left blind on the first read.
func renderCreatedRun(env Env, gf *globalFlags, run apitypes.RunDTO) error {
	if run.HealthReason != nil && strings.TrimSpace(*run.HealthReason) != "" {
		_, _ = fmt.Fprintf(env.Stderr, "warning: %s\n", sanitizeTTY(strings.TrimSpace(*run.HealthReason)))
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"run": run})
	}
	if !gf.quiet {
		return renderRunDetail(p, run)
	}
	return nil
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

// nonEmpty drops blank entries from a StringSlice flag (e.g. --exclude-agents "" or
// a trailing comma), so a flag artifact never rides as an empty exclusion. It
// returns a non-nil slice so a source-only selection marshals as `exclusions: []`
// (matching the web AgentPicker), not `null`.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// printRunFields projects a run to its named top-level scalar field(s), printing each
// value raw and unquoted on its own line, in the order the caller named them (PRD #264
// D5). It is the cheap read a poller wants: no whole-object JSON parse, so the exact
// class of footgun that produced the zsh-`echo` blindness — piping `--json` through a
// shell that reinterprets the CLI's valid \uXXXX escapes — never arises for the common
// status/mr case.
//
// The field enum is DERIVED, not restated: marshaling the concrete RunDTO to
// map[string]json.RawMessage makes its key set the source of truth (self-maintaining as
// the DTO grows), gives `null → empty line` for free, and makes the non-scalar case a
// one-byte test — a RawMessage whose first non-space byte is `[` or `{` (the four array
// fields: milestones, milestones_candidate, milestones_completed, milestones_in_progress)
// has no meaningful one-line raw form and is a usage error. An unknown field is likewise
// a usage error (exit 2), never a silent blank.
//
// Written to Stdout, NOT through the Printer, because on a PIPE this is a machine
// channel whose contract is byte-fidelity — a poller reads `.status`/`.mr_web_url`
// verbatim, so a non-TTY destination gets the raw decoded bytes. On a TTY, though, a
// field can be forge-authored free text (`issue_title`, `title`, `issue_description`)
// carrying raw control/ANSI bytes, and unlike `--json` there is no encoder here to
// escape them — so the value is run through SanitizeTTY when stdout is a terminal.
// That closes the same terminal-injection vector the rest of the CLI's human output
// guards (Risk 13) while leaving the agent/pipe contract exactly raw.
func printRunFields(env Env, run apitypes.RunDTO, fields []string) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	// Validate every field FIRST, so a bad name in a multi-field call fails cleanly
	// (exit 2) rather than printing some lines and then erroring mid-stream.
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		raw, ok := m[f]
		if !ok {
			return uzicli.Exitf(uzicli.ExitUsage, "unknown field %q", f)
		}
		v, err := scalarField(f, raw)
		if err != nil {
			return err
		}
		lines = append(lines, v)
	}
	for _, l := range lines {
		if env.StdoutTTY {
			l = uzicli.SanitizeTTY(l)
		}
		_, _ = fmt.Fprintln(env.Stdout, l)
	}
	return nil
}

// scalarField renders one RunDTO field's RawMessage as a raw one-line value, or a usage
// error if it is non-scalar (an array/object). `null` renders as the empty string (an
// empty line); a JSON string is unquoted to its raw content; a number or bool prints its
// literal bytes.
func scalarField(name string, raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return "", nil
	}
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		return "", uzicli.Exitf(uzicli.ExitUsage, "field %q is not a scalar (it is an array or object); use --json to read it", name)
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	return trimmed, nil
}

// renderRunDetail prints a run as an aligned key/value block. Health + the health
// reason are surfaced here (Risk 4): a run parked behind a locked vault carries
// its reason on the DTO, so `uzi run get` shows it without a webui round-trip.
//
// STOP_KIND is here for PRD #108 M9b, and it is the one row this block genuinely
// owed. Without it an auto-stopped run reads
//
//	STATUS           failed
//	FAILURE_REASON   run cancelled
//
// — byte-identical to a user cancel, because on the live-poller half the worker's
// own SetRunFailed overwrites failure_reason with REASON_CANCELLED. stop_kind is
// the ONLY field that survives both halves of that stop, so without this row the
// CLI cannot distinguish "you stopped this" from "uzi stopped this because your
// updates could not be saved". The reasons need no change here: this block already
// prints HEALTH_REASON and FAILURE_REASON generically, switching on no vocabulary,
// so PRD #108's new reason strings flow through untouched.
func renderRunDetail(p *uzicli.Printer, r apitypes.RunDTO) error {
	rows := [][]string{
		{"ID", r.ID},
		{"KIND", r.Kind},
		// issue #857: what/how/who started the run. A NOT NULL server enum (DEFAULT
		// 'manual'), so it is always set and printed unconditionally like KIND above.
		{"TRIGGER", r.TriggerSource},
		// RunDTO carries no is_revising (issue #750): the detail page keeps its own
		// derivePlanRevision panel, so revising is never surfaced through this helper here.
		{"STATUS", effectiveRunStatus(r.Status, r.IsPlanning, false)},
		{"TITLE", runTitle(r)},
		{"BRANCH", strOr(r.Branch, "-")},
		{mrAbbrev(r.ForgeType), int64Or(r.MrIID, "-")},
		{"HEALTH", r.Health},
	}
	if r.HealthReason != nil && *r.HealthReason != "" {
		rows = append(rows, []string{"HEALTH_REASON", sanitizeTTY(*r.HealthReason)})
	}
	if r.FailureReason != nil && *r.FailureReason != "" {
		rows = append(rows, []string{"FAILURE_REASON", sanitizeTTY(*r.FailureReason)})
	}
	// The run's inferred scheduling requirement set (PRD #84 M4), the CLI twin of the
	// three DTO fields added in 4c. All three are model/inference-derived, hence UNTRUSTED,
	// and each is emit-only-when-set exactly like HEALTH_REASON/FAILURE_REASON above: a run
	// predating the feature, or one whose plan-time inference produced nothing, carries no
	// blank row. The two slices are comma-joined into a single cell before sanitizeTTY, so
	// the untrusted join goes through the same scrub the sibling free-text rows use.
	if len(r.RequiredCapabilities) > 0 {
		rows = append(rows, []string{"REQUIRED_CAPABILITIES", sanitizeTTY(strings.Join(r.RequiredCapabilities, ","))})
	}
	if len(r.RequiredTools) > 0 {
		rows = append(rows, []string{"REQUIRED_TOOLS", sanitizeTTY(strings.Join(r.RequiredTools, ","))})
	}
	if r.SizeClass != "" {
		rows = append(rows, []string{"SIZE_CLASS", sanitizeTTY(r.SizeClass)})
	}
	// Emitted only when set, like its two neighbours — every non-stopped run would
	// otherwise carry a blank row. It is a server-controlled enum, so sanitizeTTY is
	// not strictly required; applied anyway for uniformity with the free text above,
	// and because "server-controlled today" is exactly the assumption that rots.
	if r.StopKind != nil && *r.StopKind != "" {
		rows = append(rows, []string{"STOP_KIND", sanitizeTTY(*r.StopKind)})
	}
	// STOP_REASON is the operator's OPTIONAL free-text cancel reason (issue #525),
	// captured beside stop_kind on the cancel paths. Unlike the STOP_KIND enum it is
	// free text, so it goes through sanitizeTTY like FAILURE_REASON above; emitted only
	// when set, so a non-stopped run (or a stop with no reason given) carries no blank row.
	if r.StopReason != nil && *r.StopReason != "" {
		rows = append(rows, []string{"STOP_REASON", sanitizeTTY(*r.StopReason)})
	}
	// issue #279: a report-only completion opened no MR on purpose, so the `MR -` above
	// would otherwise read as a run that never produced its deliverable. Emitted only when
	// set (a server-controlled bool, false on every normal completion), like STOP_KIND.
	// The scrubbed report_md itself is printed below the table, off the rail — see the
	// tail of this function.
	if r.ReportOnly {
		rows = append(rows, []string{"REPORT_ONLY", "yes"})
	}
	// Plain-English run summaries (PRD #362 M5): the intent ("what this run will
	// implement"), the plan summary ("what the proposed plan will do"), and the deltas
	// (how the plan diverged from the ask). All three are model-authored UNTRUSTED text
	// and every one is emit-only-when-set, so a pre-feature run or one whose summaries
	// have not landed yet is byte-for-byte unchanged. Routed through cellText below.
	rows = append(rows, summaryRows(r)...)
	// PRD-link lifecycle (#150), the CLI twin of the fields exposed on the DTO in the
	// prior commit. Both rows are emit-only-when-set: a run that moved no PRD, or one
	// predating the feature, must not print a blank row. PRD_MOVE carries the run's own
	// worker-declared path, so it is untrusted text and goes through sanitizeTTY like the
	// free-text rows above; PRD_PATCH_SETTLED_AT is a server timestamp rendered exactly
	// like LIMIT_RESETS_AT below.
	if r.PrdDonePath != nil {
		rows = append(rows, []string{"PRD_MOVE", sanitizeTTY(*r.PrdDonePath)})
	}
	if r.PrdPatchSettledAt != nil {
		rows = append(rows, []string{"PRD_PATCH_SETTLED_AT", r.PrdPatchSettledAt.UTC().Format(time.RFC3339)})
	}
	// Milestone progress + effective budget (PRD #122 M5), the CLI twin of the web's
	// MilestoneChecklist / MilestoneBadge. Both blocks are conditional so a run with no
	// frozen milestone list and a global-default budget is byte-for-byte unchanged — the
	// same back-compat contract the DTO's nil slices and null budgets carry.
	rows = append(rows, milestoneRows(r)...)
	if r.BudgetMaxIterations != nil {
		rows = append(rows, []string{"BUDGET_ITERATIONS", itoa(*r.BudgetMaxIterations)})
	}
	if r.BudgetWallSeconds != nil {
		rows = append(rows, []string{"BUDGET_WALL", fmtUntil(time.Duration(*r.BudgetWallSeconds) * time.Second)})
	}
	// Which Anthropic credential this run spent (PRD #111 M1). Emitted only when the
	// server recorded one — a pre-feature or still-queued run has nothing to say and
	// must not print a blank row.
	//
	// Through cellText, NOT sanitizeTTY, and the difference is load-bearing here in a
	// way it is not for the rows above. This is the first genuinely USER-AUTHORED
	// string in this block. cellText is the table-cell wrapper: sanitizeTTY plus
	// newline folding, tab folding and a length cap.
	//
	// This used to justify itself with "uzicli.Printer.Table does not sanitize what it
	// is handed", which #180 made false: Table now runs CellText over every cell, so
	// the newline fold happens with or without this call. What is left is a genuine
	// reason rather than a residual one — the length cap is cellText's alone, and a
	// call site that names its own untrusted input keeps stating that fact if the row
	// is ever moved off Printer.Table.
	//
	// 🔴 THE PREMISE THIS USED TO GIVE IS NOW FALSE, AND THE CONCLUSION STILL HOLDS —
	// which is exactly why the premise is being corrected rather than deleted. It read
	// "validateSecretLabel rejects control characters and U+FFFD but NOT unicode.Cf, so
	// a bidi-override label is storable". Since PRD #111 M2 the validator DOES reject
	// Cf, so a reader checking that premise would find it false and reasonably conclude
	// that sanitizeTTY now suffices here.
	//
	// It does not, for two reasons that survive the validator:
	//   - NEWLINE FOLDING. sanitizeTTY deliberately spares "\n"; a newline in a table
	//     cell breaks the rail. Only cellText folds it. That is the difference the
	//     mutation control in render_test.go pins, and the one an earlier version of
	//     that control failed to test.
	//   - The validator governs what the SERVER ACCEPTS ON WRITE. Rows stored before
	//     it landed were never re-validated and nothing re-validates on read, so a
	//     hostile label still reaches this line through history, through a future
	//     write path that skips validation, and through a direct database write.
	if r.AnthropicSecretLabel != nil && *r.AnthropicSecretLabel != "" {
		rows = append(rows, []string{"ANTHROPIC_TOKEN", credentialCell(r)})
	}
	rows = append(rows, limitWaitRows(r, time.Now())...)
	// MR_REWORK rides every run like WAIT_ON_LIMIT above, and is tri-state (PRD #841):
	// "inherit" (nil → follow the owner's account default), "on" or "off". An always-present
	// row keeps "inherit" distinguishable from an old CLI that does not know the field.
	rows = append(rows, []string{"MR_REWORK", triStateStr(r.MrReworkEnabled)})
	if err := p.Table(nil, rows); err != nil {
		return err
	}
	// issue #279: the report-only run's actual deliverable is report_md — the lead's
	// server-scrubbed findings. It is UNTRUSTED worker/model text and is multi-line, so it
	// prints on its own lines below the table (a table cell would fold the newlines),
	// through sanitizeTTY exactly like the judge summary. Guarded so a normal completion
	// and a report_only run with an empty summary print nothing.
	if r.ReportOnly && r.ReportMd != nil {
		if s := sanitizeTTY(strings.TrimSpace(*r.ReportMd)); s != "" {
			p.Println()
			p.Println("findings:")
			p.Println(s)
		}
	}
	// PRD #212: the plan turn's worktree writes (git status --porcelain lines), surfaced
	// at the approval gate exactly like the web PlanPanel (M3). Multi-line by nature, so it
	// prints below the table like report_md above, not in a table cell. Each element is
	// UNTRUSTED repo-controlled path text; a synthetic non-porcelain "… (+K more)" marker
	// may be present (print it verbatim, do not parse porcelain status codes).
	//
	// 🔴 sanitizeTTY, NOT cellText. cellText ends in strings.TrimSpace (termsafe.CellText),
	// which strips the LEADING space of the porcelain XY status code (" M path" =
	// modified-in-worktree vs "M  path" = modified-in-index) — the exact corruption M1
	// fixed server-side, and the whole point of storing porcelain lines (status + path).
	// sanitizeTTY preserves the leading space and still strips control/bidi runes. The
	// server already stripped \n/\t per line (M1's sanitizePlanChangedLine), so cellText's
	// newline fold buys nothing here; and this is off-table free text (like the report_md
	// tail), not a tabwriter rail an embedded newline could forge a row in. Emitted only
	// when non-empty: a clean plan turn and any pre-#212 run carry [] and print nothing.
	if len(r.PlanChangedFiles) > 0 {
		p.Println()
		p.Println("files changed during planning:")
		for _, line := range r.PlanChangedFiles {
			p.Println(sanitizeTTY(line))
		}
	}
	return nil
}

// limitWaitRows is the usage-limit park block of `uzi run get` (PRD #35), split out
// because it is the one part of this render with a live clock in it and therefore the
// one part worth testing without a Printer.
//
// It answers TWO different questions off the SAME columns, and conflating them would
// be the bug. The server leaves limit_resets_at / retry_not_before / rate_limit_type in
// place across a promotion, deliberately, as history — so on a parked run they say
// "here is when this resumes", and on a run that has already resumed the identical
// columns say "this run spent part of its wall clock waiting on a usage limit". A
// completed run rendering "resumes in 4h12m" would be the natural result of ignoring
// the distinction, and it would be nonsense.
func limitWaitRows(r apitypes.RunDTO, now time.Time) [][]string {
	// WAIT_ON_LIMIT rides every run, unlike its neighbours above, because it is
	// meaningful BEFORE any park has happened — it is the answer to "will this run
	// survive a usage limit, or just die on one?", which is a question about a queued
	// run as much as a parked one. A conditional row would make "off" indistinguishable
	// from an old CLI that does not know the field.
	rows := [][]string{{"WAIT_ON_LIMIT", boolStr(r.WaitOnLimit)}}
	if r.Status == statusLimitWait {
		rows = append(rows, []string{"LIMIT_WAIT", limitWaitLine(r, now)})
		// The reported window reset, as CONTEXT for the countdown above and never as a
		// substitute for it: a pool-aware promotion can land hours earlier than this.
		if r.LimitResetsAt != nil {
			rows = append(rows, []string{"LIMIT_RESETS_AT", r.LimitResetsAt.UTC().Format(time.RFC3339)})
		}
		return rows
	}
	if r.LimitWaitCount > 0 {
		rows = append(rows, []string{"LIMIT_WAITS", itoa(int(r.LimitWaitCount)) + " (resumed)"})
	}
	return rows
}

// milestoneRows renders a milestone-structured run's progress block for `uzi run get`
// (PRD #122 M5): a `{done}/{total} reported complete` summary followed by one indented
// row per milestone in FROZEN order, marked done / in progress / left. It is the CLI twin
// of the web's MilestoneChecklist, so both surfaces show the same state off the same fields.
// PRD #390 M4: a run that NEVER reported progress (MilestonesCompleted == nil) renders a
// NEUTRAL `–/{total}` numerator instead of `0/{total}`, matching the web badge's `M–/N`
// (see the summary branch below for the null-vs-`[]` contract).
//
// Empty for a run with no frozen milestone list, so a pre-#122 (or non-milestone) run is
// byte-for-byte unchanged — the same back-compat contract the nil Milestones slice carries.
//
// The summary says "reported complete", NEVER "verified" (PRD Decision 6): the worker
// REPORTS a milestone done and nothing in uzi has verified it, so the wording must not
// imply it has. `done` counts only completed ids that are MEMBERS of the frozen list —
// milestones_completed is a monotone union that can still name a milestone dropped after
// it was ticked, and counting those would let the summary read 8/7. Iterating the frozen
// list (rather than reducing over the completed set) also makes the count immune to a
// duplicate id, matching the web's milestoneBadge exactly.
//
// Titles are UNTRUSTED repo/agent-authored text (apitypes.Milestone), so each goes through
// cellText — the same newline-fold, tab-fold and 200-char cap the ANTHROPIC_TOKEN row
// relies on, which is what keeps a hostile title from breaking the table rail.
func milestoneRows(r apitypes.RunDTO) [][]string {
	if len(r.Milestones) == 0 {
		return nil
	}
	completed := make(map[string]bool, len(r.MilestonesCompleted))
	for _, id := range r.MilestonesCompleted {
		completed[id] = true
	}
	inProgress := make(map[string]bool, len(r.MilestonesInProgress))
	for _, id := range r.MilestonesInProgress {
		inProgress[id] = true
	}
	done := 0
	perMilestone := make([][]string, 0, len(r.Milestones))
	for _, m := range r.Milestones {
		state := "left"
		switch {
		case completed[m.ID]:
			state = "done"
			done++
		case inProgress[m.ID]:
			state = "in progress"
		}
		perMilestone = append(perMilestone, []string{"  " + state, cellText(m.Title)})
	}
	rows := make([][]string, 0, len(r.Milestones)+1)
	// PRD #390 D5: distinguish "never reported" from "reported zero". The null-vs-`[]`
	// contract is carried by MilestonesCompleted: nil ⇒ the milestones_completed column
	// is SQL NULL (the run never reported progress), non-nil (even an empty `[]`) ⇒ a
	// report landed. A never-reported run must render a NEUTRAL numerator (en-dash `–`,
	// matching the web badge's `M–/N`), NOT `0/N`, which reads as a failure. Test nil
	// exactly — len()==0 would conflate a reported empty list with never-reported.
	var summary string
	if r.MilestonesCompleted == nil {
		summary = fmt.Sprintf("–/%d reported complete", len(r.Milestones))
	} else {
		summary = fmt.Sprintf("%d/%d reported complete", done, len(r.Milestones))
	}
	rows = append(rows, []string{"MILESTONES", summary})
	rows = append(rows, perMilestone...)
	return rows
}

// summaryRows is the CLI surface of the plain-English run summaries (PRD #362 M5): the
// intent summary, the plan summary, and the per-delta rows describing how the proposed
// plan diverged from the original ask. It is the `run get` twin of RunView's summary
// cards; the web derives a "proposed"/"approved" label from run status, but the CLI
// keeps a single plain "PLAN SUMMARY" label — a status-derived label is a web concern.
//
// EVERYTHING here is model-authored UNTRUSTED text (Decision 10: the summary runner is
// tool-less and a crafted issue/PRD could bias it), so every value goes through cellText
// — the same newline-fold, tab-fold and length cap the ANTHROPIC_TOKEN and milestone-title
// rows rely on to keep a hostile string from breaking the table rail.
//
// Every row is emit-only-when-non-empty: a run with no summaries (pre-feature, still
// queued, or a seeded run that never planned) adds nothing, so its detail is byte-for-byte
// what it was before this milestone. Malformed deltas are already coerced to null
// server-side (Decision 6), so r.SummaryDeltas is nil/[] here rather than junk; the
// per-entry emptiness guard covers a stray blank entry without crashing regardless.
func summaryRows(r apitypes.RunDTO) [][]string {
	var rows [][]string
	if r.SummaryIntent != nil {
		if s := cellText(*r.SummaryIntent); s != "" {
			rows = append(rows, []string{"INTENT", s})
		}
	}
	if r.SummaryPlan != nil {
		if s := cellText(*r.SummaryPlan); s != "" {
			rows = append(rows, []string{"PLAN SUMMARY", s})
		}
	}
	for _, d := range r.SummaryDeltas {
		// The whole line (glyph, kind and text) is sanitized together: kind is a
		// server-validated enum today, but "tolerated on read" means the CLI must not
		// trust it, and cellText over the composed line caps and folds the untrusted
		// text in one pass. Drop an entry whose text SANITIZES to empty (whitespace, or
		// a control/bidi rune that cellText strips) rather than render a bare "+ added:".
		if cellText(d.Text) == "" {
			continue
		}
		rows = append(rows, []string{"DELTA", cellText(deltaGlyph(d.Kind) + " " + d.Kind + ": " + d.Text)})
	}
	return rows
}

// deltaGlyph is the one-rune prefix for a plan-summary delta kind, mirroring the web's
// added/changed/dropped affordance in ASCII the table rail can hold. An unrecognised
// kind (a newer server than this binary) renders a neutral bullet rather than being
// dropped — same pass-through-the-unknown stance as limitWaitLine's rate_limit_type.
func deltaGlyph(kind string) string {
	switch kind {
	case "added":
		return "+"
	case "changed":
		return "~"
	case "dropped":
		return "-"
	default:
		return "•"
	}
}

// credentialCell renders WHICH credential a run spent and WHY (PRD #111 M5, D20).
//
// The label alone was never enough, and this is the sentence that says why: an auto
// pick and a default fallback can name the SAME token, so "console-key" answers
// "which account paid" and leaves "why that account" unanswered. PRD #104's
// compatibility path also creates a row labelled literally `default`, so the label is
// not even a reliable hint at the mode.
//
// Since #754 the two live non-pick reasons name a POOLED token, not the default:
// `pool_stale` renders `auto (pooled token, no fresh readings)` and `open_failed`
// `auto (fell to another pooled token; …)` — the auto lane floors onto a pooled token
// and never spends the out-of-pool default. Only the LEGACY `pool_empty` still renders
// `default (auto: …)`, and only on pre-#754 rows where the default genuinely was spent
// (an empty pool now HOLDS in pool_wait instead). The mode is spelled out rather than
// left as a bare label because an auto pick and a legacy default can name the same
// token, so the label alone cannot say which account paid or why.
//
// An UNRECOGNISED reason prints as itself. The CLI is versioned separately from the
// API, so a newer server can ship a ninth reason this binary has never heard of, and
// inventing a rendering for it — or dropping it — would be worse than passing it
// through. Same stance as the web's autoStatusChip.
//
// A nil reason prints the bare label: a run claimed before M1 recorded no mode, and
// guessing one is exactly the wrong answer.
func credentialCell(r apitypes.RunDTO) string {
	label := cellText(*r.AnthropicSecretLabel)
	// F8's CLI half. The id goes null when the token is deleted while the snapshotted
	// label survives (00086's SET NULL), so this is a normal historical run — and the
	// difference between "go look at this token" and "this token is gone" is worth one
	// word. The web chip says the same thing; CLI parity is a rule here, not a nicety.
	if r.AnthropicSecretID == nil {
		label += " (deleted)"
	}
	if r.AnthropicSelectReason == nil || *r.AnthropicSelectReason == "" {
		return label
	}
	return label + " — " + selectReasonText(autoselect.Reason(*r.AnthropicSelectReason), r.AnthropicHeadroomPct)
}

// selectReasonText is the mode half, EXHAUSTIVE over autoselect.AllReasons() and
// pinned as such by TestCredentialCellCoversEveryReason — asserted over the
// vocabulary rather than by sampling, because a reason with no rendering is invisible
// until the one user it happens to reaches support.
//
// headroom is rendered only where it exists. It is present on an auto pick and nil
// everywhere else, including on D14's retry, where the measurement described the
// credential that would NOT open.
//
// 🔴 A LATENT COUPLING WITH THE WEB, recorded because no test can currently see it.
// This appends the suffix only under `auto` and `best_of_pool`; web's
// lib/runCredential.ts appends it for ANY reason with a non-null headroom. The two
// agree today by accident of the server rather than by construction — headroom is
// written only on an auto pick, and D14 explicitly nulls it on the retry, so no
// reachable state distinguishes them. The day something records a headroom on a
// fallback, the CLI silently drops it and the web silently shows it. Whichever
// surface changes first should make the other match.
func selectReasonText(reason autoselect.Reason, headroom *int) string {
	pct := ""
	if headroom != nil {
		pct = ", " + strconv.Itoa(*headroom) + "% headroom"
	}
	switch reason {
	case autoselect.ReasonDefault:
		return "default"
	case autoselect.ReasonPinned:
		return "pinned"
	case autoselect.ReasonJudge:
		return "judge binding"
	case autoselect.ReasonAuto:
		return "auto" + pct
	case autoselect.ReasonBestOfPool:
		// Named rather than folded into `auto`: every pooled token was under the
		// headroom floor and the emptiest was picked anyway (D10). The run worked, and
		// the user's pool is nearly exhausted — a thing to know, not an error.
		return "auto (best of pool)" + pct
	case autoselect.ReasonPoolEmpty:
		// LEGACY VALUE — no longer produced on new runs (#754). An auto worker with
		// a genuinely empty pool now HOLDS the run in pool_wait and spends nothing,
		// rather than falling back to the out-of-pool default. This string is kept
		// only for PRE-#754 historical rows, where the default genuinely WAS spent;
		// it is deliberately worded to stay true for those without implying that an
		// empty pool spends the default today.
		return "default (auto: pool was empty — legacy)"
	case autoselect.ReasonPoolStale:
		// The run FLOORED onto a POOLED token that had no fresh usage reading to rank
		// on — it spent one of the user's OWN pooled tokens as a last resort, not the
		// out-of-pool default (#754). A floored stale token carries no headroom, so no
		// pct is appended here.
		return "auto (pooled token, no fresh readings)"
	case autoselect.ReasonOpenFailed:
		// The selector's picked token would not decrypt, so the run FLOORED onto
		// ANOTHER POOLED token — again not the default (#754).
		return "auto (fell to another pooled token; the chosen one would not open)"
	}
	return string(reason)
}

// renderMessage prints one run message. In --json mode it emits one compact JSON
// object per line (NDJSON), so `--follow` streams cleanly and an agent parses it
// line by line — and since it marshals the DTO whole, `agent_instance` and
// `agent_label` have been in the JSON output since they were added to MessageDTO
// (PRD #99 M1), with no work here. In human mode it prints a single
// #seq/kind/actor/payload line, where the actor cell is what M4 widens.
func renderMessage(p *uzicli.Printer, m apitypes.MessageDTO) error {
	if p.Format == uzicli.FormatJSON {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		p.Println(string(b))
		return nil
	}
	// %-*s pads to a fixed RUNE width (Go's fmt measures width in runes, not bytes),
	// and actorCell has already capped its result to that width, so the payload column
	// starts at the same rune offset on every line.
	p.Printf("#%-4d %-16s %-*s %s\n", m.Seq, m.Kind, actorCellWidth, actorCell(m), compactPayload(m.Payload))
	return nil
}

// actorCellWidth bounds the human table's actor column. The payload column beside
// it already runs to 200 characters, so terminal width is not the scarce resource
// this trades against — the cap exists to keep the columns ALIGNED down the stream,
// which is what `uzi run logs --follow` is read through. (Rune alignment, like the
// rest of this CLI's tables: a CJK label still occupies two terminal columns per
// rune, which no part of this tool accounts for — out of scope, not a regression.)
const actorCellWidth = 34

// actorCell renders WHO produced a message: the role, the short id of the
// invocation that produced it when the frame carries one, and that invocation's
// task label. Two parallel `coder` subagents therefore read distinctly (PRD #99
// Decision 10) instead of as two identical `coder` rows:
//
//	#12   tool_use   coder/3v6ptu · API wi…  {"name":"Edit"}
//	#13   tool_use   coder/2k9xqf · web ga…  {"name":"Edit"}
//
// The short id alone already separates them, so a truncated label costs clarity,
// never correctness. A frame with no instance — the orchestrator's own turns, infra
// frames, and every pre-migration message — renders as the bare role exactly as it
// did before, and a missing role still renders "-".
//
// Both new fields are worker-supplied text bound for a TTY, and `agent_label` is
// free model-authored prose rather than a role drawn from a fixed roster, so each
// goes through cellText's sanitize-and-fold (Risk 13: a raw CSI sequence in this
// cell could clear the screen or spoof output). That also closes the same hole for
// `agent`, which this function inherited printing verbatim.
func actorCell(m apitypes.MessageDTO) string {
	cell := "-"
	if m.Agent != nil {
		if role := cellText(*m.Agent); role != "" {
			cell = role
		}
	}
	if m.AgentInstance != nil {
		if id := cellText(shortInstanceID(*m.AgentInstance)); id != "" {
			cell += "/" + id
		}
	}
	if m.AgentLabel != nil {
		if label := cellText(*m.AgentLabel); label != "" {
			cell += " · " + label
		}
	}
	return capCell(cell, actorCellWidth)
}

// shortInstanceID renders an invocation id compactly: its LAST 6 runes.
//
// A tail, not a prefix, and deliberately NOT shortRecID's first-8 rule. That rule is
// right for a random UUID, but an SDK tool-use id carries a constant `toolu_01`
// prefix, so first-8 would return the same eight characters for every instance in a
// run — the one thing this column exists to tell apart. A tail needs no claim about
// the prefix's shape at all, which matters because the real id format is documented
// (~30 characters, PRD #99) but is not verified here against a live SDK run.
//
// Display only, and lossy: two ids CAN share a 6-rune tail. --json carries the full
// value, and that is the agent contract.
func shortInstanceID(id string) string {
	const n = 6
	r := []rune(id)
	if len(r) <= n {
		return id
	}
	return string(r[len(r)-n:])
}

// compactPayload renders a message payload as a single truncated line for the
// human table. The payload is server-forwarded run content; it is DATA, printed
// verbatim (never interpreted).
func compactPayload(raw json.RawMessage) string {
	return compactText(string(raw))
}

// renderRunInputs prints the steer queue (follow-ups and scope directives,
// newest-first) as a kind/body/state/age table (PRD #95 M4, #634). The body is the
// user's own text, sanitized like any free text bound for a TTY. State is derived per
// kind — from (consumed_at, runStatus) for a follow_up, from disposition for a scope
// directive; age is relative to created_at.
func renderRunInputs(p *uzicli.Printer, inputs []apitypes.SteerInputDTO, runStatus string) error {
	rows := make([][]string, 0, len(inputs))
	for _, in := range inputs {
		body := "-"
		if in.Body != nil {
			body = compactText(*in.Body)
		}
		rows = append(rows, []string{
			steerKindLabel(in.Kind),
			body,
			steerState(in.Kind, in.ConsumedAt, in.Disposition, runStatus),
			relAge(in.CreatedAt),
		})
	}
	return p.Table([]string{"KIND", "BODY", "STATE", "AGE"}, rows)
}

// steerKindLabel renders the KIND column: a scope directive shows "scope", everything
// else (a follow_up, or an empty kind on a degraded read) shows "follow-up".
func steerKindLabel(kind string) string {
	if kind == kindScope {
		return "scope"
	}
	return "follow-up"
}

// steerState derives a steer-queue row's state label. For a kind='scope' operator
// directive (PRD #634) the state IS its disposition (a scope row is never consumed):
// nil → "active (scope ceiling set)", applied/declined/superseded → their explained
// forms, any other value → the raw string. For a follow_up it derives the delivery
// label from its consumed_at and the run's live status, mirroring PRD #95 Decision 7
// as closely as the CLI can:
//   - not consumed, run terminal  → "not delivered (run finished)"
//   - not consumed, run parked    → "queued (run paused on a usage limit)"
//   - not consumed, otherwise     → "queued"
//   - consumed, run at plan gate  → "delivered (applies after approval)"
//   - consumed, awaiting follow-up → "delivered (resumes the run)"
//   - consumed, run parked        → "delivered (run paused on a usage limit)"
//   - consumed, otherwise         → "delivered"
//
// runStatus may be "" when the run's status could not be fetched (Decision 10
// floor): the gate/terminal nuance is then dropped and only queued/delivered
// show — the acceptable CLI minimum.
//
// The two statusLimitWait arms are a SUFFIX on the existing answer, never a
// replacement for it (PRD #35). A park changes nothing about whether a follow-up was
// handed to the worker; it changes only whether anything is currently acting on it.
// Rewriting "queued" to something else on a parked run would be the same lie as
// dropping the park entirely, one level down: the queue state is still queued.
func steerState(kind string, consumedAt *time.Time, disposition *string, runStatus string) string {
	// PRD #634: a scope directive's state IS its disposition — it is never consumed, so
	// consumed_at/runStatus carry no delivery signal for it. A nil disposition means the
	// ceiling is still pending (only reachable on a live run).
	if kind == kindScope {
		if disposition == nil {
			return "active (scope ceiling set)"
		}
		switch *disposition {
		case "applied":
			return "applied (finalized at the ceiling)"
		case "declined":
			return "declined (not acted on)"
		case "superseded":
			return "superseded (a later directive replaced it)"
		default:
			return *disposition
		}
	}
	// A parked/held run is deliberately NOT terminal here, matching terminalRunStatuses:
	// its queue survives the park and drains when the run resumes, so "not delivered
	// (run finished)" would be false.
	const parkedSuffix = " (run paused on a usage limit)"
	// PRD #754: a pool_wait run is HELD on an empty token pool, not a usage limit, so its
	// suffix names the actual reason (distinct copy for a distinct hold).
	const heldSuffix = " (run held on an empty token pool)"
	if consumedAt == nil {
		if terminalRunStatuses[runStatus] {
			return "not delivered (run finished)"
		}
		if runStatus == statusLimitWait {
			return "queued" + parkedSuffix
		}
		if runStatus == statusPoolWait {
			return "queued" + heldSuffix
		}
		return "queued"
	}
	if runStatus == "awaiting_approval" {
		return "delivered (applies after approval)"
	}
	if runStatus == "awaiting_input" {
		// PRD #88: the run is parked on a question, so a follow-up sits behind the
		// answer rather than behind an approval. Distinct wording because the action
		// the user owes is different, and telling them to approve something would be
		// simply wrong.
		return "delivered (applies after the question is answered)"
	}
	if runStatus == "awaiting_followup" {
		// PRD #517: the interactive task is parked awaiting the user's next follow-up.
		// Tailored copy mirroring the web twin (SteerQueueCard's "Delivered — resumes
		// the run") — a delivered follow-up here is what wakes the parked run.
		return "delivered (resumes the run)"
	}
	if runStatus == statusLimitWait {
		return "delivered" + parkedSuffix
	}
	if runStatus == statusPoolWait {
		return "delivered" + heldSuffix
	}
	return "delivered"
}

// waitOnLimitFlag resolves `--wait-on-limit` into the TRI-STATE the create endpoint
// takes (PRD #35 Decision 7): nil omits the key so the server stamps the run from the
// owner's Settings default, &true opts this run in, &false opts it explicitly out.
//
// 🔴 THE DEFAULT VALUE IN THE FLAG DEFINITION IS NOT THE DEFAULT BEHAVIOUR, and that
// is the trap this function exists to remove. `Bool("wait-on-limit", false, …)` makes
// GetBool return false when the flag is absent — identical to `--wait-on-limit=false`.
// Passing that straight through would send `"wait_on_limit": false` on EVERY CLI-created
// run and silently override the user's own Settings default, which is precisely the
// regression the server's field comment cites for taking a *bool. Changed() is what
// separates "the user said false" from "the user said nothing"; pflag sets it for
// `--wait-on-limit` and for `--wait-on-limit=false` alike, and leaves it false when the
// flag is absent, which is exactly the three-way split needed.
//
// There is NO precedent for a tri-state flag in this CLI — `--force`, `--with-token`
// and `--follow` are all plain switches whose false is a real default rather than an
// absence — so this is the first, and the Changed() idiom is the standard pflag answer
// rather than an invention. A `--no-wait-on-limit` twin was the alternative and loses:
// two flags need a mutual-exclusion check, and they can be passed together.
//
// Note `--wait-on-limit false` (a SPACE, not `=`) does not set false — pflag reads a
// bare bool flag as true and leaves `false` as a positional argument. `create` is
// cobra.NoArgs, so that mistake is a loud usage error rather than a silent inversion,
// which is why it needs no guard of its own.
func waitOnLimitFlag(cmd *cobra.Command) *bool {
	if !cmd.Flags().Changed("wait-on-limit") {
		return nil
	}
	v, err := cmd.Flags().GetBool("wait-on-limit")
	if err != nil {
		return nil
	}
	return &v
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

// limitWaitLine is the ONE sentence every CLI surface renders for a parked run
// (PRD #35): `uzi run get`'s detail block, the `--follow` notice and the TUI's run
// header all call this, so the terminal cannot give three readings of one park.
//
// It returns "" for a run that is not parked, which is what makes it safe to call
// unconditionally at each of those sites.
//
// The countdown is off RetryNotBefore, NOT LimitResetsAt, and the two routinely
// differ — RetryNotBefore carries jitter, is clamped to RUN_LIMIT_MAX_PARK, is
// cross-checked against the owner's gauge and is pool-aware, so a user with a second
// credential that still has headroom is promoted long BEFORE the window it hit
// reopens. RetryNotBefore is when work resumes; LimitResetsAt is context, and
// renderRunDetail prints it as its own row rather than folding it in here.
//
// RateLimitType is server-allowlisted (anything outside the SDK union is coerced to
// "unknown"), so it is an enum by the time it reaches here — and it still goes through
// cellText, for the same reason the STOP_KIND row does: "server-controlled today" is
// exactly the assumption that rots, and this string lands in a table cell where a
// newline breaks the rail. An UNRECOGNISED type prints as itself; the CLI is versioned
// separately from the API, so inventing a rendering for a member a newer server ships,
// or dropping it, would both be worse than passing it through.
func limitWaitLine(r apitypes.RunDTO, now time.Time) string {
	if r.Status != statusLimitWait {
		return ""
	}
	line := "paused: Anthropic usage limit"
	if t := cellText(strOr(r.RateLimitType, "")); t != "" {
		line += " (" + t + ")"
	}
	line += " · " + resumesIn(r.RetryNotBefore, now)
	// Attempt N, never "N/cap": the cap is one server constant and is deliberately not
	// on RunDTO, so a denominator here would have to be a second copy of it in a binary
	// that ships on its own release cadence. 0 is a run that has not parked, which this
	// function has already excluded — but the guard stays, because a server that
	// reports a park without a count should print no number rather than "attempt 0".
	if r.LimitWaitCount > 0 {
		line += " · attempt " + itoa(int(r.LimitWaitCount))
	}
	return line
}

// resumesIn renders the promotion clock for a parked run.
//
// A nil RetryNotBefore is a real case, not a defensive branch: an older server, or a
// park whose stamp failed to record. It must not render as "resumes in 0s" (a promise
// the CLI cannot keep) nor be silently omitted (the park then reads as an unexplained
// hang), so it says what is actually known.
//
// A stamp already in the past is the NORMAL steady state for a few seconds — the
// sweeper runs on a ticker, so there is always a window where the clock has passed and
// the promotion has not fired yet.
func resumesIn(retryNotBefore *time.Time, now time.Time) string {
	if retryNotBefore == nil {
		return "no resume time recorded"
	}
	d := retryNotBefore.Sub(now)
	if d <= 0 {
		return "resuming shortly"
	}
	return "resumes in " + fmtUntil(d)
}

// fmtUntil renders a forward-looking duration at TWO units, which is where it parts
// company with relAge's single unit next door.
//
// relAge answers "how old is this row" for a table column, where 3h is precise enough.
// This answers "when do I come back", over a range that runs from seconds to the
// seven-day window RUN_LIMIT_MAX_PARK clamps to — and across that range a single unit
// is unusable: a park with 3h59m left and one with 3h01m left both read "3h", which is
// an hour of error on the only number the user came for.
func fmtUntil(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
